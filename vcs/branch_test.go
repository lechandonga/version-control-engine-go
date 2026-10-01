package vcs

import (
	"errors"
	"testing"
)

func TestBranchCreateSwitchDelete(t *testing.T) {
	r, _ := testRepo(t)
	c1 := commitFiles(t, r, 1, "c1", map[string]string{"a": "1"})

	_, err := r.Branch("feat")
	assertNoError(t, err, "create feat")
	// 重复创建报错。
	_, err = r.Branch("feat")
	if err == nil {
		t.Fatal("duplicate branch should fail")
	}

	assertNoError(t, r.Checkout("feat"), "checkout feat")
	cur, _ := r.CurrentBranch()
	if cur != "feat" {
		t.Fatalf("current=%s want feat", cur)
	}
	c2 := commitFiles(t, r, 2, "c2", map[string]string{"b": "2"})

	// 切回 main，工作区应还原为 c1 快照（b 被删除）。
	assertNoError(t, r.Checkout("main"), "checkout main")
	if _, err := r.readBlob(mustBlob(r, t, "b")); err == nil {
		t.Fatal("b.txt should not be on main")
	}
	head, _ := r.HeadCommit()
	if head != c1 {
		t.Fatalf("main head changed unexpectedly: %s", head[:8])
	}

	assertNoError(t, r.Checkout("feat"), "back to feat")
	head, _ = r.HeadCommit()
	if head != c2 {
		t.Fatalf("feat head=%s want %s", head[:8], c2[:8])
	}

	// 不能删除当前分支。
	err = r.DeleteBranch("feat")
	if err == nil {
		t.Fatal("deleting current branch must fail")
	}
	assertNoError(t, r.Checkout("main"), "co main for delete")
	assertNoError(t, r.DeleteBranch("feat"), "delete feat")
	_, err = r.readRef("feat")
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("feat should be gone, got %v", err)
	}
}

func mustBlob(r *Repository, t *testing.T, path string) string {
	t.Helper()
	id, err := r.HeadCommit()
	assertNoError(t, err, "head")
	c, err := r.ReadCommit(id)
	assertNoError(t, err, "commit")
	m, err := r.flattenTree(c.Tree)
	assertNoError(t, err, "flatten")
	b, ok := m[path]
	if !ok {
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return b
}

// TestCheckoutRejectsOverwrite 未提交修改与未跟踪文件都不能被悄悄覆盖。
func TestCheckoutRejectsOverwrite(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"tracked": "base\n", "same": "x\n"})
	mustBranch(t, r, "other", "branch other")

	// other 分支把 tracked 改成不同内容，并新增 newfile。
	assertNoError(t, r.Checkout("other"), "co other")
	_ = commitFiles(t, r, 2, "other", map[string]string{"tracked": "other\n", "newfile": "n\n"})
	assertNoError(t, r.Checkout("main"), "co main")

	// 场景 1：本地未提交修改 tracked，切换会覆盖 -> 拒绝。
	writeWork(t, dir, "tracked", "local-uncommitted\n")
	err := r.Checkout("other")
	var ue *UnsafeOverwriteError
	if !errors.As(err, &ue) {
		t.Fatalf("expected unsafe overwrite error, got %v", err)
	}
	if readWork(t, dir, "tracked") != "local-uncommitted\n" {
		t.Fatal("local edit must be preserved after rejection")
	}

	// 还原 tracked。
	writeWork(t, dir, "tracked", "base\n")

	// 场景 2：未跟踪文件 newfile 与目标分支新增文件冲突 -> 拒绝。
	writeWork(t, dir, "newfile", "untracked-local\n")
	err = r.Checkout("other")
	if !errors.As(err, &ue) {
		t.Fatalf("expected untracked-overwrite rejection, got %v", err)
	}
	t.Logf("input=未跟踪newfile与目标新增同名 判定依据=目标新增且本地未跟踪存在 => 拒绝; paths=%v", ue.Paths)
	if readWork(t, dir, "newfile") != "untracked-local\n" {
		t.Fatal("untracked file must be preserved")
	}
}

// TestCheckoutAllowsWhenIdentical 本地修改结果与目标一致时允许切换。
func TestCheckoutAllowsWhenIdentical(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "base\n"})
	mustBranch(t, r, "other", "branch other")
	assertNoError(t, r.Checkout("other"), "co other")
	_ = commitFiles(t, r, 2, "other", map[string]string{"f": "other\n"})
	assertNoError(t, r.Checkout("main"), "co main")

	// 本地把 f 提前改成 other 的内容；切换应成功（结果相同）。
	writeWork(t, dir, "f", "other\n")
	assertNoError(t, r.Checkout("other"), "checkout with identical local edit")
}

// TestMergeRejectsDirtyOverwrite 合并也拒绝覆盖未提交修改。
func TestMergeRejectsDirtyOverwrite(t *testing.T) {
	r, dir := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "base\n"})
	mustBranch(t, r, "side", "branch side")
	_ = commitFiles(t, r, 2, "main", map[string]string{"f": "main\n"})
	assertNoError(t, r.Checkout("side"), "co side")
	_ = commitFiles(t, r, 3, "side", map[string]string{"g": "side\n"})
	assertNoError(t, r.Checkout("main"), "co main")

	writeWork(t, dir, "g", "local-dirty\n")
	_, err := r.Merge("side", CommitOptions{Author: testAuthor(), When: testClock(4)})
	var ue *UnsafeOverwriteError
	if !errors.As(err, &ue) {
		t.Fatalf("merge must reject overwrite, got %v", err)
	}
	if readWork(t, dir, "g") != "local-dirty\n" {
		t.Fatal("dirty content lost after rejected merge")
	}
}
