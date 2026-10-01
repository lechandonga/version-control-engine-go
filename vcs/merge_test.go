package vcs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func repeatHex(n int) string { return strings.Repeat("0", n) }

func mustBranch(t *testing.T, r *Repository, name, ctx string) {
	t.Helper()
	_, err := r.Branch(name)
	assertNoError(t, err, ctx)
}

func hexID(prefix byte) string {
	return string(prefix) + repeatHex(IDLen-1)
}

// commitOnBranch 在 base 之上用分离头构造一个提交并写入指定分支引用。
func (r *Repository) commitOnBranch(t *testing.T, branch, base string, n int, msg string, files map[string]string, deletes []string) string {
	t.Helper()
	if err := r.detachToLocked(base); err != nil {
		t.Fatalf("detach: %v", err)
	}
	dir := r.WorkDir()
	for p, c := range files {
		writeWork(t, dir, p, c)
	}
	for _, p := range deletes {
		removeWork(t, dir, p)
	}
	paths := make([]string, 0, len(files)+len(deletes))
	for p := range files {
		paths = append(paths, p)
	}
	paths = append(paths, deletes...)
	if err := r.Add(paths); err != nil {
		t.Fatalf("add: %v", err)
	}
	id, err := r.Commit(CommitOptions{
		Message: msg,
		Author:  Signature{Name: "t", Email: "t@x", When: time.Unix(1700000000+int64(n)*60, 0).UTC()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if branch != "" {
		if err := r.writeRefAtomic(branch, id); err != nil {
			t.Fatalf("writeRef: %v", err)
		}
	}
	return id
}

// TestThreeWayMergeUnit 表驱动覆盖三方合并的全部判定分支。
func TestThreeWayMergeUnit(t *testing.T) {
	B, O, T := hexID('b'), hexID('a'), hexID('c')
	cases := []struct {
		name       string
		base       BlobMap
		ours       BlobMap
		theirs     BlobMap
		wants      map[string]string
		wantKind   string
		wantDelete bool
	}{
		{"both identical", BlobMap{"f": B}, BlobMap{"f": O}, BlobMap{"f": O}, map[string]string{"f": O}, "", false},
		{"only ours", BlobMap{"f": B}, BlobMap{"f": O}, BlobMap{"f": B}, map[string]string{"f": O}, "", false},
		{"only theirs", BlobMap{"f": B}, BlobMap{"f": B}, BlobMap{"f": T}, map[string]string{"f": T}, "", false},
		{"ours deletes clean", BlobMap{"f": B}, BlobMap{}, BlobMap{"f": B}, map[string]string{}, "", false},
		{"both delete", BlobMap{"f": B}, BlobMap{}, BlobMap{}, map[string]string{}, "", false},
		{"both add identical", BlobMap{}, BlobMap{"f": O}, BlobMap{"f": O}, map[string]string{"f": O}, "", false},
		{"both add different", BlobMap{}, BlobMap{"f": O}, BlobMap{"f": T}, nil, ConflictModifyModify, false},
		{"divergent modify", BlobMap{"f": B}, BlobMap{"f": O}, BlobMap{"f": T}, nil, ConflictModifyModify, false},
		{"ours delete/they modify", BlobMap{"f": B}, BlobMap{}, BlobMap{"f": T}, nil, ConflictDeleteModify, false},
		{"they delete/ours modify", BlobMap{"f": B}, BlobMap{"f": O}, BlobMap{}, nil, ConflictDeleteModify, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, conflicts := threeWayMerge(tc.base, tc.ours, tc.theirs)
			if tc.wantKind == "" {
				if len(conflicts) != 0 {
					t.Fatalf("expected no conflict, got %+v", conflicts)
				}
				if !blobMapsEqual(res, BlobMap(tc.wants)) {
					t.Fatalf("result=%v want %v", res, tc.wants)
				}
			} else {
				if len(conflicts) != 1 || conflicts[0].Kind != tc.wantKind || conflicts[0].Path != "f" {
					t.Fatalf("expected %s at f, got %+v", tc.wantKind, conflicts)
				}
			}
		})
	}
}

func TestFastForwardMerge(t *testing.T) {
	r, _ := testRepo(t)
	commitFiles(t, r, 1, "base", map[string]string{"f": "1"})
	mustBranch(t, r, "dev", "branch dev")
	tip := commitFiles(t, r, 2, "advance", map[string]string{"f": "2"})
	assertNoError(t, r.Checkout("dev"), "checkout dev")

	res, err := r.Merge("main", CommitOptions{Author: testAuthor(), When: testClock(3)})
	assertNoError(t, err, "merge")
	if !res.FastForward || res.CommitID != tip {
		t.Fatalf("expected fast-forward to %s, got %+v", tip[:8], res)
	}
	devID, _ := r.readRef("dev")
	if devID != tip {
		t.Fatalf("ff pointer wrong: %s", devID[:8])
	}
	mc, _ := r.ReadCommit(tip)
	if len(mc.Parents) != 1 {
		t.Fatalf("fast-forward must not create a merge commit, parents=%v", mc.Parents)
	}
}

func TestDivergentMergeContent(t *testing.T) {
	r, dir := testRepo(t)
	base := commitFiles(t, r, 1, "base", map[string]string{"a": "0", "b": "0"})
	mustBranch(t, r, "side", "branch side")
	ours := commitFiles(t, r, 2, "ours", map[string]string{"a": "ours"})
	assertNoError(t, r.Checkout("side"), "checkout side")
	_ = r.commitOnBranch(t, "side", base, 3, "theirs", map[string]string{"b": "theirs"}, nil)
	assertNoError(t, r.Checkout("main"), "checkout main")

	res, err := r.Merge("side", CommitOptions{Author: testAuthor(), When: testClock(4)})
	assertNoError(t, err, "merge divergent")
	if res.Kind != "merge-commit" {
		t.Fatalf("expected merge-commit, got %s", res.Kind)
	}
	mc, err := r.ReadCommit(res.CommitID)
	assertNoError(t, err, "read merge commit")
	if len(mc.Parents) != 2 || mc.Parents[0] != ours {
		t.Fatalf("merge parents wrong: %+v", mc.Parents)
	}
	if readWork(t, dir, "a") != "ours" || readWork(t, dir, "b") != "theirs" {
		t.Fatalf("merged content wrong a=%q b=%q", readWork(t, dir, "a"), readWork(t, dir, "b"))
	}
	assertNoError(t, r.Fsck(), "fsck")
}

func TestMergeConflictLeavesHeadUntouched(t *testing.T) {
	r, _ := testRepo(t)
	base := commitFiles(t, r, 1, "base", map[string]string{"f": "same\n"})
	mustBranch(t, r, "side", "branch side")
	_ = commitFiles(t, r, 2, "ours", map[string]string{"f": "ours\n"})
	assertNoError(t, r.Checkout("side"), "co side")
	_ = r.commitOnBranch(t, "side", base, 3, "theirs", map[string]string{"f": "theirs\n"}, nil)
	assertNoError(t, r.Checkout("main"), "co main")

	before, _ := r.HeadCommit()
	_, err := r.Merge("side", CommitOptions{Author: testAuthor(), When: testClock(4)})
	var mc *MergeConflictError
	if !errors.As(err, &mc) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if len(mc.Conflicts) != 1 || mc.Conflicts[0].Kind != ConflictModifyModify {
		t.Fatalf("want single modify-modify, got %+v", mc.Conflicts)
	}
	after, _ := r.HeadCommit()
	if before != after {
		t.Fatalf("HEAD changed after conflict %s->%s", before[:8], after[:8])
	}
}

func TestMergeDeleteModifyConflict(t *testing.T) {
	r, _ := testRepo(t)
	base := commitFiles(t, r, 1, "base", map[string]string{"f": "x"})
	mustBranch(t, r, "side", "branch side")
	_ = r.commitOnBranch(t, "main", base, 2, "delete", nil, []string{"f"})
	assertNoError(t, r.Checkout("side"), "co side")
	_ = r.commitOnBranch(t, "side", base, 3, "modify", map[string]string{"f": "edited"}, nil)
	assertNoError(t, r.Checkout("main"), "co main")

	_, err := r.Merge("side", CommitOptions{Author: testAuthor(), When: testClock(4)})
	var mc *MergeConflictError
	if !errors.As(err, &mc) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if len(mc.Conflicts) != 1 || mc.Conflicts[0].Kind != ConflictDeleteModify {
		t.Fatalf("want delete-modify, got %+v", mc.Conflicts)
	}
	t.Logf("input=main删除f/side修改f 判定依据=双方相对base变化且一边缺失 => %s", mc.Conflicts[0].Kind)
}
