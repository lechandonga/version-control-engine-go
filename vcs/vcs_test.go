package vcs

import (
	"os"
	"path/filepath"
	"testing"
)

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(t.TempDir())
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return r
}

func writeWork(t *testing.T, r *Repo, path, content string) {
	t.Helper()
	p := r.workPath(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, r *Repo, msg string) string {
	t.Helper()
	id, err := r.Commit(msg)
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return id
}

func readWork(t *testing.T, r *Repo, path string) string {
	t.Helper()
	data, err := os.ReadFile(r.workPath(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestBasicOps 回归：提交、分支、切换、合并的基本行为。
func TestBasicOps(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "a1")
	c1 := commitAll(t, r, "c1")
	t.Logf("场景: 基础提交 c1=%s", c1[:8])

	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "b.txt", "b1")
	commitAll(t, r, "c2-master")

	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.workPath("b.txt")); !os.IsNotExist(err) {
		t.Fatal("切换后 b.txt 应不存在")
	}
	writeWork(t, r, "c.txt", "c1")
	commitAll(t, r, "c2-feature")

	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Merge("feature"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := readWork(t, r, "c.txt"); got != "c1" {
		t.Fatalf("合并后 c.txt = %q", got)
	}
	if got := readWork(t, r, "b.txt"); got != "b1" {
		t.Fatalf("合并后 b.txt = %q", got)
	}
	head, _ := r.CurrentCommit()
	hc, err := r.ReadCommit(head)
	if err != nil {
		t.Fatal(err)
	}
	if len(hc.Parents) != 2 {
		t.Fatalf("合并提交应有 2 个父提交, got %d", len(hc.Parents))
	}
	t.Log("判定: 合并提交双亲正确, 双方文件均在场")
}

// TestMergeConflict 回归：冲突时保留现场，可放弃。
func TestMergeConflict(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "f.txt", "base")
	commitAll(t, r, "base")
	if err := r.CreateBranch("side"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "ours")
	commitAll(t, r, "ours")
	if err := r.Checkout("side"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "theirs")
	theirs := commitAll(t, r, "theirs")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	_, err := r.Merge("side")
	mc, ok := err.(*MergeConflict)
	if !ok {
		t.Fatalf("期望 MergeConflict, got %v", err)
	}
	t.Logf("场景: 合并冲突路径=%v", mc.Paths)
	st, _ := r.readMergeState()
	if st == nil || st.Theirs != theirs {
		t.Fatal("冲突后应保留合并现场")
	}
	if err := r.MergeAbort(); err != nil {
		t.Fatal(err)
	}
	if got := readWork(t, r, "f.txt"); got != "ours" {
		t.Fatalf("abort 后应恢复 ours, got %q", got)
	}
	t.Log("判定: 现场已保存并可回退")
}

// TestRebase 回归：重放前进。
func TestRebase(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "a1")
	commitAll(t, r, "base")
	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "m.txt", "m1")
	commitAll(t, r, "master-work")
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "f1")
	commitAll(t, r, "feature-work")

	if err := r.Rebase("master"); err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if got := readWork(t, r, "m.txt"); got != "m1" {
		t.Fatal("重放后应包含 master 的文件")
	}
	if got := readWork(t, r, "f.txt"); got != "f1" {
		t.Fatal("重放后应包含 feature 的文件")
	}
	head, _ := r.CurrentCommit()
	c, _ := r.ReadCommit(head)
	if c.Message != "feature-work" {
		t.Fatalf("重放保留提交信息, got %q", c.Message)
	}
	t.Log("判定: 重放后历史线性且内容完整")
}

// TestRebaseConflictContinueAbort 回归：重放冲突暂停后继续与回退。
func TestRebaseConflictContinueAbort(t *testing.T) {
	setup := func() *Repo {
		r := newRepo(t)
		writeWork(t, r, "f.txt", "base")
		commitAll(t, r, "base")
		if err := r.CreateBranch("feature"); err != nil {
			t.Fatal(err)
		}
		writeWork(t, r, "f.txt", "master-version")
		commitAll(t, r, "master-change")
		if err := r.Checkout("feature"); err != nil {
			t.Fatal(err)
		}
		writeWork(t, r, "f.txt", "feature-version")
		commitAll(t, r, "feature-change")
		return r
	}

	t.Run("continue", func(t *testing.T) {
		r := setup()
		err := r.Rebase("master")
		rc, ok := err.(*RebaseConflict)
		if !ok {
			t.Fatalf("期望 RebaseConflict, got %v", err)
		}
		t.Logf("场景: 重放在 %s 暂停", rc.Commit[:8])
		if st, _ := r.ReadRebaseState(); st == nil {
			t.Fatal("冲突后应保留重放现场")
		}
		writeWork(t, r, "f.txt", "resolved")
		if err := r.RebaseContinue(); err != nil {
			t.Fatalf("continue: %v", err)
		}
		if got := readWork(t, r, "f.txt"); got != "resolved" {
			t.Fatalf("继续后应为解决内容, got %q", got)
		}
		if st, _ := r.ReadRebaseState(); st != nil {
			t.Fatal("完成后现场应清除")
		}
	})

	t.Run("abort", func(t *testing.T) {
		r := setup()
		before, _ := r.CurrentCommit()
		if err := r.Rebase("master"); err == nil {
			t.Fatal("期望冲突")
		}
		if err := r.RebaseAbort(); err != nil {
			t.Fatal(err)
		}
		after, _ := r.CurrentCommit()
		if after != before {
			t.Fatalf("回退后应回到原位 %s, got %s", before[:8], after[:8])
		}
		if got := readWork(t, r, "f.txt"); got != "feature-version" {
			t.Fatalf("回退后工作区应恢复, got %q", got)
		}
		t.Log("判定: 回退后指针与工作区均回到重放前")
	})
}

// TestCheckoutProtectsLocalChanges 回归：切换保护本地修改。
func TestCheckoutProtectsLocalChanges(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "f.txt", "v1")
	commitAll(t, r, "c1")
	if err := r.CreateBranch("other"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "v2")
	commitAll(t, r, "c2")
	if err := r.Checkout("other"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "local-edit")
	err := r.Checkout("master")
	if _, ok := err.(*WouldOverwrite); !ok {
		t.Fatalf("期望 WouldOverwrite, got %v", err)
	}
	t.Log("判定: 本地修改被保护")
}
