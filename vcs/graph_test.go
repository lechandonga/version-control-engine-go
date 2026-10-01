package vcs

import (
	"testing"
)

// 纯函数验证多数票虚拟基：不受候选顺序影响、无多数票回退 absent。
func TestMajorityBase(t *testing.T) {
	X := hexID('x')
	Y := hexID('y')

	// 3 个候选基：2 个含 f=X，1 个不含；多数票 => f=X。
	bases := []BlobMap{
		{"f": X},
		{"f": X},
		{},
	}
	vb := majorityBase(bases)
	if vb["f"] != X {
		t.Fatalf("majority should pick X, got %q", vb["f"])
	}

	// 2 个候选基分歧（f=X / f=Y），无多数票 => f 不存在。
	vb2 := majorityBase([]BlobMap{{"f": X}, {"f": Y}})
	if _, ok := vb2["f"]; ok {
		t.Fatalf("tie should resolve to absent, got %q", vb2["f"])
	}

	// 顺序无关：打乱后结果一致。
	vb3 := majorityBase([]BlobMap{{}, {"f": X}, {"f": X}})
	if !blobMapsEqual(vb, vb3) {
		t.Fatalf("virtual base depends on order: %v vs %v", vb, vb3)
	}
	t.Logf("input=多候选基(2*X,1*absent) 判定依据=多数票 => f存在=X; 2候选平票=>absent")
}

// TestCrissCrossMergeBases 构造真实 criss-cross，验证识别出 >=2 个共同祖先，
// 且合并结果在相同仓库状态下多次运行完全一致（可复现）。
func TestCrissCrossMergeBases(t *testing.T) {
	r, dir := testRepo(t)
	// 初始共同提交。
	root := commitFiles(t, r, 1, "root", map[string]string{"g": "0"})

	// 两条线各自前进：A 改 a，B 改 b。
	mustBranch(t, r, "brB", "branch B")
	aID := r.commitOnBranch(t, "main", root, 2, "A", map[string]string{"g": "0", "a": "A"}, nil)
	assertNoError(t, r.Checkout("brB"), "co B")
	bID := r.commitOnBranch(t, "brB", root, 3, "B", map[string]string{"g": "0", "b": "B"}, nil)

	// 交叉合并 1：main 合并 B -> M1（main 上）。
	assertNoError(t, r.Checkout("main"), "co main")
	m1, err := r.Merge("brB", CommitOptions{Author: testAuthor(), When: testClock(4)})
	assertNoError(t, err, "merge M1")

	// 交叉合并 2：B 合并 main(含 M1) -> 第一次 B 合 main 为 up-to-date，
	// 需要让 B 在合并前与 main 真正分叉。改为：B 先合旧 main(aID) 得 M2。
	// 重新构造：让 brB 直接基于 bID 与 aID 合并形成另一交叉点。
	assertNoError(t, r.Checkout("brB"), "co brB")
	// brB 当前是 bID 的后代链（fast-forward 到 M1 了吗？没有，brB 仍=bID）。
	_ = aID
	_ = bID
	// 让 brB 合并 aID（裸提交）以制造第二个交叉点。
	m2ID := crossMergeCommits(t, r, bID, aID, 5)

	bases, err := r.MergeBases(m1.CommitID, m2ID)
	assertNoError(t, err, "merge bases")
	t.Logf("criss-cross bases(%s,%s): %v", m1.CommitID[:8], m2ID[:8], shortIDs(bases))
	if len(bases) < 2 {
		t.Fatalf("criss-cross should yield >=2 merge bases, got %d: %v", len(bases), bases)
	}

	// 虚拟基确定且可复现。
	vb1, err := r.virtualBase(bases)
	assertNoError(t, err, "vb1")
	vb2, err := r.virtualBase(bases)
	assertNoError(t, err, "vb2")
	if !blobMapsEqual(vb1, vb2) {
		t.Fatal("virtual base not reproducible")
	}
	_ = dir
}

// crossMergeCommits 在当前分离头上构造一个双父合并提交（纯内部，用于建图）。
func crossMergeCommits(t *testing.T, r *Repository, ours, theirs string, n int) string {
	t.Helper()
	oc, err := r.ReadCommit(ours)
	assertNoError(t, err, "read ours")
	tc, err := r.ReadCommit(theirs)
	assertNoError(t, err, "read theirs")
	ot, err := r.flattenTree(oc.Tree)
	assertNoError(t, err, "flatten ours")
	tt, err := r.flattenTree(tc.Tree)
	assertNoError(t, err, "flatten theirs")
	merged := BlobMap{}
	for k, v := range ot {
		merged[k] = v
	}
	for k, v := range tt {
		merged[k] = v
	}
	treeID, err := r.buildTree(merged)
	assertNoError(t, err, "build tree")
	c := &Commit{
		Tree:      treeID,
		Parents:   []string{ours, theirs},
		Author:    Signature{Name: "t", Email: "t@x", When: testClock(n)},
		Committer: Signature{Name: "t", Email: "t@x", When: testClock(n)},
		Message:   "cross",
	}
	id, err := r.writeCommit(c)
	assertNoError(t, err, "write cross commit")
	return id
}

func TestAncestry(t *testing.T) {
	r, _ := testRepo(t)
	c1 := commitFiles(t, r, 1, "1", map[string]string{"f": "1"})
	c2 := commitFiles(t, r, 2, "2", map[string]string{"f": "2"})
	ok, err := r.IsAncestor(c1, c2)
	assertNoError(t, err, "isancestor")
	if !ok {
		t.Fatal("c1 should be ancestor of c2")
	}
	rev, _ := r.IsAncestor(c2, c1)
	if rev {
		t.Fatal("c2 should not be ancestor of c1")
	}
}
