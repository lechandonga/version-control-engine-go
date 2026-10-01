package vcs

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRebaseRecoverPaused 冲突暂停后模拟重启：
// 用 Open 重新打开仓库，Recover 应识别 paused 并对齐现场。
func TestRebaseRecoverPaused(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "b\n"})
	mustBranch(t, r, "topic", "branch topic")
	_ = commitFiles(t, r, 2, "main", map[string]string{"f": "b\nm\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	_ = commitFiles(t, r, 3, "topic", map[string]string{"f": "b\nt\n"})

	res, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(10)})
	assertNoError(t, err, "rebase pause")
	if res.Kind != "paused" {
		t.Fatalf("want paused, got %s", res.Kind)
	}

	// 模拟进程重启：重新 Open，不改任何状态。
	r2, err := Open(dir)
	assertNoError(t, err, "reopen")
	action, err := r2.RebaseRecover()
	assertNoError(t, err, "recover")
	if action != "paused" {
		t.Fatalf("want paused recovery, got %s", action)
	}
	st, err := r2.RebaseStatus()
	assertNoError(t, err, "status after recover")
	if !st.Present || !st.Paused {
		t.Fatalf("recovered state should still be paused: %+v", st)
	}
	// 仍可继续完成。
	writeWork(t, dir, "f", "resolved\n")
	assertNoError(t, r2.Add([]string{"f"}), "add resolve")
	cres, err := r2.RebaseContinue(RebaseOptions{Author: testAuthor(), When: testClock(11)})
	assertNoError(t, err, "continue after restart")
	if cres.Applied != 1 {
		t.Fatalf("want applied 1, got %+v", cres)
	}
	assertNoError(t, r2.Fsck(), "fsck")
}

// TestRebaseRecoverInterruptedSafeAbort 模拟“重放到一半被杀”：
// 手动制造一个非暂停但未完成的状态（meta/snapshot/head 存在，无 current），
// Recover 必须安全回退到起始快照，不留部分应用提交/悬空引用。
func TestRebaseRecoverInterruptedSafeAbort(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "b\n"})
	mustBranch(t, r, "topic", "branch topic")
	_ = commitFiles(t, r, 2, "main", map[string]string{"f": "b\n", "up": "u\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	topicHead := commitFiles(t, r, 3, "work", map[string]string{"f": "b\n", "w": "w\n"})

	// 记录起始现场（与 Rebase 开始时一致：topic 分支、工作区干净）。
	origWork := readWork(t, dir, "f")

	// 直接从干净的 topic 现场手工构造“非暂停的未完成重放”状态：
	// meta/pending/head 都在，但没有 current（表示重放进行到一半被杀）。
	stateDir := r.rebasePath()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaLines := []string{"topic", topicHead, "main", topicHead}
	if err := writeLines(filepath.Join(stateDir, "meta"), metaLines); err != nil {
		t.Fatal(err)
	}
	if err := writeLines(filepath.Join(stateDir, "pending"), []string{topicHead}); err != nil {
		t.Fatal(err)
	}
	if err := writeLines(filepath.Join(stateDir, "applied"), nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLines(filepath.Join(stateDir, "head"), []string{topicHead}); err != nil {
		t.Fatal(err)
	}
	// 保存起始快照（topic 干净现场）。
	sn := &snapshot{
		headMode:  "ref",
		headValue: "topic",
		tracked:   BlobMap{"f": "unused"},
		untracked: FileSet{},
	}
	// 用真实索引填充。
	idx, err := r.readIndex()
	assertNoError(t, err, "read index")
	sn.index = idx
	// tracked 用真实 HEAD 树。
	sn.tracked, err = r.headTreeMap()
	assertNoError(t, err, "head tree")
	assertNoError(t, r.saveSnapshot(sn), "save snapshot")

	// 恢复：应安全回退。
	action, err := r.RebaseRecover()
	assertNoError(t, err, "recover interrupted")
	if action != "aborted" {
		t.Fatalf("want aborted, got %s", action)
	}
	if fileExists(stateDir) {
		t.Fatal("state dir must be removed after safe abort recovery")
	}
	cur, _ := r.CurrentBranch()
	if cur != "topic" {
		t.Fatalf("branch after recovery=%s want topic", cur)
	}
	head, _ := r.HeadCommit()
	if head != topicHead {
		t.Fatalf("branch pointer must return to original %s, got %s", topicHead[:8], head[:8])
	}
	if readWork(t, dir, "f") != origWork {
		t.Fatalf("worktree not restored: %q", readWork(t, dir, "f"))
	}
	assertNoError(t, r.Fsck(), "fsck after recovery")
}

// TestConcurrentWritesSerialization 多个 goroutine 并发提交，
// 仓库锁必须串行化，结果在 -race 下稳定且对象/引用始终一致。
func TestConcurrentWritesSerialization(t *testing.T) {
	r, dir := testRepo(t)
	// 初始提交。
	commitFiles(t, r, 0, "init", map[string]string{"seq": "0\n"})

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个 goroutine 独立打开同一仓库（模拟多进程/多客户端）。
			ri, err := Open(dir)
			if err != nil {
				errs <- err
				return
			}
			// 抢不到锁则退避重试，模拟真实客户端排队等待临界区；
			// 锁保证每个提交在“读 head -> 写提交 -> 更新引用”期间独占。
			var l *LockFile
			for attempt := 0; attempt < 2000; attempt++ {
				var lerr error
				l, lerr = ri.lock()
				if lerr == nil {
					break
				}
				if lerr != ErrLockHeld {
					errs <- lerr
					return
				}
				time.Sleep(time.Millisecond)
			}
			if l == nil {
				errs <- ErrLockHeld
				return
			}
			defer l.Release()
			head, herr := ri.HeadCommit()
			if herr != nil {
				errs <- herr
				return
			}
			hc, _ := ri.ReadCommit(head)
			m, _ := ri.flattenTree(hc.Tree)
			m["seq"] = mustStoreBlob(ri, t, seqContent(i))
			treeID, _ := ri.buildTree(m)
			c := &Commit{
				Tree:      treeID,
				Parents:   []string{head},
				Author:    Signature{Name: "c", Email: "c@x", When: testClock(i)},
				Committer: Signature{Name: "c", Email: "c@x", When: testClock(i)},
				Message:   "concurrent",
			}
			id, werr := ri.writeCommit(c)
			if werr != nil {
				errs <- werr
				return
			}
			if werr := ri.writeRefAtomic("main", id); werr != nil {
				errs <- werr
			}
			return
		}(i)
	}
	wg.Wait()
	close(errs)

	lockFailures := 0
	for err := range errs {
		if err == ErrLockHeld {
			lockFailures++
			continue
		}
		if err != nil {
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	// 无论多少个抢到锁，最终引用必须指向一个校验通过、可走通历史的提交。
	final, err := r.HeadCommit()
	assertNoError(t, err, "final head")
	assertNoError(t, r.Fsck(), "fsck under concurrency")
	// 历史链完整：从 final 可一路走到根。
	cur := final
	steps := 0
	for cur != "" {
		c, err := r.ReadCommit(cur)
		if err != nil {
			t.Fatalf("broken history at %s: %v", cur[:8], err)
		}
		steps++
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
		if steps > n+2 {
			t.Fatal("history too long, likely corruption")
		}
	}
	if steps != n+1 {
		t.Fatalf("all %d commits must land in one linear chain, got length %d", n, steps)
	}
	t.Logf("并发写入完成: 链长=%d, 锁竞争失败=%d（均被安全拒绝）", steps, lockFailures)
}

func seqContent(i int) string {
	return "seq-" + itoa(i) + "\n"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func mustStoreBlob(r *Repository, t *testing.T, content string) string {
	t.Helper()
	id, err := r.writeBlob([]byte(content))
	assertNoError(t, err, "writeBlob")
	return id
}

// TestLockMutualExclusion 锁必须互斥，释放后立即可再获取。
func TestLockMutualExclusion(t *testing.T) {
	r, _ := testRepo(t)
	l1, err := r.lock()
	assertNoError(t, err, "first lock")
	_, err = r.lock()
	if err != ErrLockHeld {
		t.Fatalf("second lock must fail with ErrLockHeld, got %v", err)
	}
	assertNoError(t, l1.Release(), "release")
	l3, err := r.lock()
	assertNoError(t, err, "relock after release")
	assertNoError(t, l3.Release(), "release 2")
}
