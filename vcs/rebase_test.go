package vcs

import (
	"os"
	"path/filepath"
	"testing"
)

// buildRebaseHistory 构造：main=base->upstream，topic=base->w1->w2。
// 返回 r, dir。
func buildRebaseHistory(t *testing.T) (*Repository, string, string, string) {
	t.Helper()
	r, dir := testRepo(t)
	base := commitFiles(t, r, 1, "base", map[string]string{"base.txt": "base\n"})
	mustBranch(t, r, "topic", "branch topic")
	up := commitFiles(t, r, 2, "upstream", map[string]string{"base.txt": "base\n", "up.txt": "up\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	commitFiles(t, r, 3, "work1", map[string]string{"base.txt": "base\n", "w1.txt": "w1\n"})
	commitFiles(t, r, 4, "work2", map[string]string{"base.txt": "base\n", "w1.txt": "w1\n", "w2.txt": "w2\n"})
	return r, dir, base, up
}

func TestRebaseReplaysCommits(t *testing.T) {
	r, dir, _, up := buildRebaseHistory(t)
	t.Logf("up(main tip)=%s", up)
	res, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(10)})
	assertNoError(t, err, "rebase")
	if res.Applied != 2 || res.Skipped != 0 {
		t.Fatalf("want applied=2 skipped=0, got %+v", res)
	}
	// topic 现在以 upstream 为父。
	head, _ := r.HeadCommit()
	hc, _ := r.ReadCommit(head)
	t.Logf("head=%s parent=%v", head, hc.Parents)
	if len(hc.Parents) != 1 {
		t.Fatalf("rebased head should have one parent, got %v", hc.Parents)
	}
	w1New := hc.Parents[0]
	w1c, _ := r.ReadCommit(w1New)
	if len(w1c.Parents) != 1 || w1c.Parents[0] != up {
		t.Fatalf("first rebased commit must sit on upstream %s, got %v", up[:8], w1c.Parents)
	}
	// 三个文件都在。
	for _, f := range []string{"base.txt", "up.txt", "w1.txt", "w2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s after rebase: %v", f, err)
		}
	}
	// 状态目录已清理。
	if fileExists(r.rebasePath()) {
		t.Fatal("rebase state dir should be removed after completion")
	}
	assertNoError(t, r.Fsck(), "fsck")
}

func TestRebaseSkipsEquivalentChange(t *testing.T) {
	r, _ := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "1\n"})
	mustBranch(t, r, "topic", "branch topic")
	// main 与 topic 独立地把 f 改成完全相同的内容。
	_ = commitFiles(t, r, 2, "main-change", map[string]string{"f": "same\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	_ = commitFiles(t, r, 3, "topic-change-different-msg", map[string]string{"f": "same\n"})

	res, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(9)})
	assertNoError(t, err, "rebase")
	if res.Skipped != 1 || res.Applied != 0 {
		t.Fatalf("equivalent change must be skipped, got %+v", res)
	}
	t.Logf("input=topic提交(不同message)产出与main相同的f 判定依据=逐路径blob相同 => skip=%d", res.Skipped)
	head, _ := r.HeadCommit()
	mainID, _ := r.readRef("main")
	if head != mainID {
		t.Fatalf("after full skip topic should equal main, %s vs %s", head[:8], mainID[:8])
	}
}

// TestRebaseEquivalentIndependentOfOrder 判定不受提交信息/时间戳/顺序影响：
// 用不同 message、不同 author 时间重放，仍判定等效跳过。
func TestRebaseEquivalentIndependentOfOrder(t *testing.T) {
	r, _ := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "1\n"})
	mustBranch(t, r, "topic", "branch topic")
	_ = commitFiles(t, r, 2, "m", map[string]string{"f": "same\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")

	// 手工构造一个 message 完全不同、时间戳也不同、但树效果相同的提交。
	base, _ := r.readRef("main")
	bc, _ := r.ReadCommit(base)
	id := r.commitOnBranch(t, "topic", base, 99, "totally-different-message", map[string]string{"f": "same\n"}, nil)
	_ = bc
	_ = id
	res, err := r.Rebase(RebaseOptions{
		Upstream: "main",
		Author:   Signature{Name: "other", Email: "o@x", When: testClock(123)},
		When:     testClock(555),
	})
	assertNoError(t, err, "rebase")
	if res.Skipped != 1 {
		t.Fatalf("must skip regardless of metadata, got applied=%d skipped=%d", res.Applied, res.Skipped)
	}
}

func TestRebaseConflictContinueAndAbort(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "base\n"})
	mustBranch(t, r, "topic", "branch topic")
	_ = commitFiles(t, r, 2, "main", map[string]string{"f": "base\nmain\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	_ = commitFiles(t, r, 3, "topic", map[string]string{"f": "base\ntopic\n"})

	// 启动重放 -> 冲突暂停。
	res, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(10)})
	assertNoError(t, err, "rebase start (pause is a result)")
	if res.Kind != "paused" {
		t.Fatalf("expected paused, got %s", res.Kind)
	}
	st, err := r.RebaseStatus()
	assertNoError(t, err, "status")
	if !st.Paused || len(st.Conflicts) != 1 || st.Conflicts[0] != "f" {
		t.Fatalf("bad paused state: %+v", st)
	}

	// 场景 A：abort 精确回退。
	// 先记录暂停前 topic 的现场。
	topicBefore, _ := r.readRef("topic")
	assertNoError(t, r.RebaseAbort(), "abort")
	if fileExists(r.rebasePath()) {
		t.Fatal("state dir must be removed after abort")
	}
	cur, _ := r.CurrentBranch()
	if cur != "topic" {
		t.Fatalf("after abort branch=%s want topic", cur)
	}
	head, _ := r.HeadCommit()
	if head != topicBefore {
		t.Fatalf("abort must restore original topic head %s, got %s", topicBefore[:8], head[:8])
	}
	if readWork(t, dir, "f") != "base\ntopic\n" {
		t.Fatalf("abort must restore worktree, got %q", readWork(t, dir, "f"))
	}
	assertNoError(t, r.Fsck(), "fsck after abort")

	// 场景 B：重新重放，这次解决冲突后 continue。
	res2, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(11)})
	assertNoError(t, err, "rebase restart")
	if res2.Kind != "paused" {
		t.Fatalf("expected pause again, got %s", res2.Kind)
	}
	writeWork(t, dir, "f", "base\nmain\nresolved\n")
	assertNoError(t, r.Add([]string{"f"}), "add resolution")
	res3, err := r.RebaseContinue(RebaseOptions{Author: testAuthor(), When: testClock(12)})
	assertNoError(t, err, "continue")
	if res3.Applied != 1 {
		t.Fatalf("continue should apply 1, got %+v", res3)
	}
	if readWork(t, dir, "f") != "base\nmain\nresolved\n" {
		t.Fatalf("resolved content not materialized: %q", readWork(t, dir, "f"))
	}
	if fileExists(r.rebasePath()) {
		t.Fatal("state should be cleaned after continue completes")
	}
	assertNoError(t, r.Fsck(), "fsck after continue")
}

// TestRebaseAbortRestoresUntracked abort 必须保留未跟踪文件。
func TestRebaseAbortRestoresUntracked(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "b\n"})
	mustBranch(t, r, "topic", "branch topic")
	_ = commitFiles(t, r, 2, "main", map[string]string{"f": "b\nm\n"})
	assertNoError(t, r.Checkout("topic"), "co topic")
	_ = commitFiles(t, r, 3, "topic", map[string]string{"f": "b\nt\n"})
	writeWork(t, dir, "keep-untracked.txt", "keepme\n")
	res, err := r.Rebase(RebaseOptions{Upstream: "main", Author: testAuthor(), When: testClock(10)})
	assertNoError(t, err, "rebase")
	if res.Kind != "paused" {
		t.Fatalf("need a paused rebase to abort, got %s", res.Kind)
	}
	assertNoError(t, r.RebaseAbort(), "abort")
	if readWork(t, dir, "keep-untracked.txt") != "keepme\n" {
		t.Fatal("untracked file lost after abort")
	}
}
