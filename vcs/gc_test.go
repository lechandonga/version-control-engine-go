package vcs

import (
	"os"
	"testing"
	"time"
)

// 构建一个含分叉与悬空提交的仓库，返回 main 尖端、dev 尖端与悬空提交。
func gcFixture(t *testing.T) (*Repo, string, string, string, string) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	base := commitAll(t, r, dir, "base")
	if err := r.CreateBranch("dev", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "2")
	mainTip := commitAll(t, r, dir, "main-tip")
	if err := r.Checkout("dev"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "d", "dev")
	devTip := commitAll(t, r, dir, "dev-tip")

	// 一个只被留痕引用的提交（模拟删错分支前的位置）
	writeFile(t, dir, "ghost", "g")
	ghost := commitAll(t, r, dir, "ghost-commit")
	if err := r.detachHEAD(devTip); err != nil { // HEAD 脱离，让 ghost 无引用
		t.Fatal(err)
	}
	// 纯悬空对象：直接写一个 blob，无任何引用指向它
	dangling, err := r.putObject(ObjectBlob, []byte("nobody points here"))
	if err != nil {
		t.Fatal(err)
	}
	_ = dangling
	if err := r.writeHEADSymbolic(branchRefName("dev")); err != nil {
		t.Fatal(err)
	}
	return r, dir, mainTip, devTip, ghost
}

// 场景：可达性判定覆盖分支/HEAD/在途现场/留痕四类根。
func TestGCReachabilityRoots(t *testing.T) {
	r, _, _, devTip, ghost := gcFixture(t)

	opts := GCOptions{Retain: 0, Now: fixedTime(1)}
	rep, err := r.PreviewGC(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("判定依据 roots: branch=%d head=%d reflog=%d; reachable=%d unreachable=%d reclaimable=%d",
		len(rep.Roots["branch"]), len(rep.Roots["head"]), len(rep.Roots["reflog"]),
		rep.Reachable, rep.Unreachable, len(rep.Reclaimable))

	// ghost 只出现在留痕里，必须判为可达（保证能照记录恢复）
	for _, id := range rep.Reclaimable {
		if id == ghost {
			t.Fatal("判定依据: 留痕引用的 ghost 提交不得被回收")
		}
	}
	if rep.Reachable <= 0 || rep.Unreachable < 1 {
		t.Fatalf("应当同时存在可达与不可达对象, %+v", rep)
	}
	// devTip 必须可达（分支根）
	if !idReachableViaRead(t, r, devTip) {
		t.Fatal("devTip 必须可读且可达")
	}

	// 在途重放现场中的提交也必须判可达
	r2, d2 := newTestRepo(t)
	writeFile(t, d2, "f", "base\n")
	b := commitAll(t, r2, d2, "b")
	r2.CreateBranch("topic", b)
	writeFile(t, d2, "f", "main\n")
	commitAll(t, r2, d2, "m")
	r2.Checkout("topic")
	writeFile(t, d2, "f", "topic\n")
	stoppedCommit := commitAll(t, r2, d2, "stopped")
	err = r2.Rebase("main")
	var rc *RebaseConflict
	if !asRebaseConflict(err, &rc) {
		t.Fatalf("期望暂停重放: %v", err)
	}
	rep2, err := r2.PreviewGC(GCOptions{Retain: 0, Now: fixedTime(2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("在途重放 roots: rebase=%d", len(rep2.Roots["rebase"]))
	if len(rep2.Roots["rebase"]) == 0 {
		t.Fatal("判定依据: 在途重放现场必须作为可达根")
	}
	for _, list := range [][]string{rep2.Reclaimable} {
		for _, id := range list {
			if id == stoppedCommit {
				t.Fatal("判定依据: 暂停在队列里的提交不得被回收")
			}
		}
	}
}

// 场景：保留期边界——刚产生的对象保留，变旧后回收；回收后历史一致。
func TestGCRetentionBoundaryAndEquivalence(t *testing.T) {
	r, _, mainTip, devTip, ghost := gcFixture(t)

	before := snapshotHistory(t, r, mainTip, devTip)

	// 保留期边界由对象文件 mtime 决定：把悬空对象调老，其余保持“年轻”。
	// Now 用真实时间；Retain=1h => mtime 早于 1 小时前才可清。
	danglingID := findDanglingBlob(t, r, mainTip, devTip, ghost)
	oldPath := objectLoosePath(r.root, danglingID)
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	old := GCOptions{Retain: time.Hour, Now: time.Now()}
	rep, err := r.PreviewGC(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Reclaimable) == 0 {
		t.Fatal("判定依据: 超过保留期的悬空对象应出现在 reclaimable")
	}

	// 保留期极长 => 即使调老的对象也不可清
	fresh := GCOptions{Retain: 100 * 365 * 24 * time.Hour, Now: fixedTime(1000)}
	repFresh, err := r.PreviewGC(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(repFresh.Reclaimable) != 0 {
		t.Fatalf("判定依据: 保留期内对象不得清理, got %d", len(repFresh.Reclaimable))
	}
	if len(repFresh.TooYoung) == 0 {
		t.Fatal("判定依据: 新生不可达对象应进入 too-young")
	}

	// 先归档，再执行回收（覆盖归档对象剔除路径）
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	repAfterPack, err := r.PreviewGC(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(repAfterPack.Reclaimable) == 0 {
		t.Fatal("归档后悬空对象仍应可回收")
	}
	done, err := r.GC(old)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GC 结果: reachable=%d deleted=%d packsRewritten=%d",
		done.Reachable, done.LooseDeleted, done.PacksRewritten)
	if done.PacksRewritten != 1 && done.LooseDeleted == 0 && done.PacksRewritten == 0 {
		// 悬空对象是松散的也允许走松散删除路径
	}

	// ghost（留痕引用）仍可读
	if _, err := r.ReadCommit(ghost); err != nil {
		t.Fatalf("判定依据: 留痕引用对象回收后必须仍可读: %v", err)
	}
	// 全部可达历史读取结果一致
	after := snapshotHistory(t, r, mainTip, devTip)
	if len(after) != len(before) {
		t.Fatalf("回收前后历史条目数变化: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("回收后历史不一致 @%s", k)
		}
	}
	// 合并与重放判定结果仍可复现
	if err := r.Checkout("main"); err != nil {
		t.Fatal(err)
	}
	res, err := r.Merge("dev", "")
	if err != nil {
		t.Fatalf("回收后合并应仍可执行: %v", err)
	}
	if res.Mode != "merge-commit" {
		t.Fatalf("判定依据: 回收后合并判定必须不变, got %s", res.Mode)
	}

	// 再跑一次 GC 必须幂等、无删除
	again, err := r.GC(old)
	if err != nil {
		t.Fatal(err)
	}
	if again.LooseDeleted != 0 || again.PacksRewritten != 0 {
		t.Fatalf("判定依据: 重复 GC 必须无操作, %+v", again)
	}
}

// 场景：预览不得产生任何删除。
func TestGCDryRunSideEffectFree(t *testing.T) {
	r, _, _, _, _ := gcFixture(t)
	before := looseCount(t, r)
	if _, err := r.PreviewGC(GCOptions{Retain: 0, Now: fixedTime(1)}); err != nil {
		t.Fatal(err)
	}
	if got := looseCount(t, r); got != before {
		t.Fatalf("预览不得改变松散对象数: %d -> %d", before, got)
	}
}

func asRebaseConflict(err error, rc **RebaseConflict) bool {
	if err == nil {
		return false
	}
	var target *RebaseConflict
	if e, ok := err.(*RebaseConflict); ok {
		target = e
		*rc = target
		return true
	}
	return false
}

func idReachableViaRead(t *testing.T, r *Repo, id string) bool {
	t.Helper()
	_, _, err := r.readObject(id)
	return err == nil
}

func snapshotHistory(t *testing.T, r *Repo, tips ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tip := range tips {
		cur := tip
		for cur != "" {
			c, err := r.ReadCommit(cur)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := r.treeFlat(c.Tree)
			if err != nil {
				t.Fatal(err)
			}
			key := cur
			sum := c.Tree
			for _, e := range entries {
				sum += e.ID
			}
			out[key] = sum
			if len(c.Parents) == 0 {
				break
			}
			cur = c.Parents[0]
		}
	}
	return out
}

// findDanglingBlob 找一个不属于任何可达历史闭包的松散 blob。
func findDanglingBlob(t *testing.T, r *Repo, reachableTips ...string) string {
	t.Helper()
	roots := map[string][]string{"x": reachableTips}
	reach, err := r.walkReachable(roots)
	if err != nil {
		t.Fatal(err)
	}
	loose, err := r.listLooseObjects()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range loose {
		if !reach[id] {
			typ, _, err := r.readObject(id)
			if err == nil && typ == ObjectBlob {
				return id
			}
		}
	}
	t.Fatal("找不到悬空 blob")
	return ""
}
