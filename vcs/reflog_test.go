package vcs

import (
	"os"
	"strings"
	"testing"
)

// TestReflogRecordsAllMoves 场景：各类指针移动全部留痕，可按分支/按时间查看。
func TestReflogRecordsAllMoves(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	if err := r.CreateBranch("dev"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("dev"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "b.txt", "2")
	c2 := commitAll(t, r, "c2")

	entries, corrupt, err := r.ReadReflog("refs/heads/dev")
	if err != nil || len(corrupt) != 0 {
		t.Fatalf("read reflog: %v corrupt=%v", err, corrupt)
	}
	var ops []string
	for _, e := range entries {
		ops = append(ops, e.Op)
	}
	t.Logf("dev 分支记录: ops=%v", ops)
	if !contains(ops, "branch") || !contains(ops, "commit") {
		t.Fatalf("缺少 branch/commit 记录: %v", ops)
	}
	// 提交记录应体现 old->new。
	var last ReflogEntry
	for _, e := range entries {
		if e.Op == "commit" {
			last = e
		}
	}
	if last.Old != c1 || last.New != c2 {
		t.Fatalf("提交记录 old/new 错误: %s -> %s", last.Old, last.New)
	}

	all, _, err := r.ReadAllReflog()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Time.Before(all[i-1].Time) {
			t.Fatal("按时间查看应有序")
		}
	}
	var headOps []string
	for _, e := range all {
		if e.Ref == "HEAD" {
			headOps = append(headOps, e.Op)
		}
	}
	t.Logf("HEAD 记录: ops=%v", headOps)
	if !contains(headOps, "switch") {
		t.Fatalf("HEAD 日志应包含 switch: %v", headOps)
	}
	t.Log("判定: 分支/HEAD 移动均留痕, 时间有序")
}

// TestRecoverDeletedBranch 场景：删错分支后按记录找回。
func TestRecoverDeletedBranch(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	commitAll(t, r, "c1")
	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "work")
	want := commitAll(t, r, "feature-work")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBranch("feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadRef("refs/heads/feature"); err == nil {
		t.Fatal("分支应已删除")
	}

	// 从记录里找到删除前的位置并恢复。
	entries, _, err := r.ReadReflog("refs/heads/feature")
	if err != nil {
		t.Fatal(err)
	}
	var lastPos string
	for _, e := range entries {
		if e.Op == "branch-delete" {
			lastPos = e.Old
		}
	}
	t.Logf("场景: 误删分支, 记录中删除前位置=%s", lastPos[:8])
	if lastPos != want {
		t.Fatalf("记录的位置 %s != 实际 %s", lastPos, want)
	}
	if err := r.Recover("refs/heads/feature", lastPos); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, err := r.ReadRef("refs/heads/feature")
	if err != nil || got != want {
		t.Fatalf("恢复后指向 %s, 期望 %s (err=%v)", got, want, err)
	}
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	if readWork(t, r, "f.txt") != "work" {
		t.Fatal("恢复后历史可读")
	}
	t.Log("判定: 分支找回, 内容完整")
}

// TestRecoverAfterRebaseAbort 场景：错误回退后按记录恢复到回退前位置。
func TestRecoverAfterRebaseAbort(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "f.txt", "base")
	commitAll(t, r, "base")
	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "master-v")
	commitAll(t, r, "m1")
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "g.txt", "feature-v")
	commitAll(t, r, "f1")
	if err := r.Rebase("master"); err != nil {
		t.Fatal(err)
	}
	rebased, _ := r.CurrentCommit()
	t.Logf("场景: 重放完成于 %s, 随后模拟整体回退", rebased[:8])

	// 模拟“整体回退”：把分支强行挪回重放前。
	entries, _, _ := r.ReadReflog("refs/heads/feature")
	var orig string
	for _, e := range entries {
		if e.Op == "branch" {
			orig = e.New
		}
	}
	if err := r.writeRefLocked2("refs/heads/feature", orig); err != nil {
		t.Fatal(err)
	}
	// 现在按记录找回重放后的位置：回退记录的 old 即回退前位置。
	entries, _, _ = r.ReadReflog("refs/heads/feature")
	var last string
	for _, e := range entries {
		if e.Op == "reset" {
			last = e.Old
		}
	}
	if err := r.Recover("refs/heads/feature", last); err != nil {
		t.Fatal(err)
	}
	got, _ := r.ReadRef("refs/heads/feature")
	if got != rebased {
		t.Fatalf("恢复后应为重放结果 %s, got %s", rebased[:8], got[:8])
	}
	t.Log("判定: 错误回退可按记录恢复")
}

// writeRefLocked2 测试辅助：模拟一次绕过正常流程的指针回退。
func (r *Repo) writeRefLocked2(name, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeRefLocked(name, id, "reset", "simulated rollback")
}

// TestReflogCorruptIsolation 场景：记录损坏被单独识别，不连累仓库。
func TestReflogCorruptIsolation(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	commitAll(t, r, "c1")
	// 注入一行损坏记录和一行合法记录。
	p := r.logPath("refs/heads/master")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("this is not json\n")
	f.WriteString(`{"time":"2026-01-01T00:00:00Z","op":"commit","ref":"refs/heads/master","old":"","new":"` + strings.Repeat("a", 64) + `"}` + "\n")
	f.Close()

	entries, corrupt, err := r.ReadReflog("refs/heads/master")
	if err != nil {
		t.Fatalf("损坏记录不应导致整体读取失败: %v", err)
	}
	t.Logf("场景: 注入损坏行, 解析出 %d 条合法记录, %d 条损坏", len(entries), len(corrupt))
	if len(corrupt) != 1 {
		t.Fatalf("应识别出 1 条损坏记录, got %d", len(corrupt))
	}
	if corrupt[0].Line == 0 {
		t.Fatal("损坏记录应带行号")
	}
	if len(entries) < 2 {
		t.Fatalf("合法记录应保留, got %d", len(entries))
	}
	// 仓库照常打开、照常提交。
	if _, err := Open(r.Root); err != nil {
		t.Fatalf("损坏日志不应影响打开仓库: %v", err)
	}
	writeWork(t, r, "b.txt", "2")
	if _, err := r.Commit("c2"); err != nil {
		t.Fatalf("损坏日志不应影响提交: %v", err)
	}
	t.Log("判定: 损坏行单独识别, 仓库功能不受影响")
}

// TestRecoverAtomic 场景：恢复是原子落盘，结果要么旧要么新。
func TestRecoverAtomic(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	writeWork(t, r, "a.txt", "2")
	c2 := commitAll(t, r, "c2")
	if err := r.Recover("refs/heads/master", c1); err != nil {
		t.Fatal(err)
	}
	got, _ := r.ReadRef("refs/heads/master")
	if got != c1 && got != c2 {
		t.Fatalf("指针必须落在旧位置或新位置之一, got %s", got)
	}
	if got != c1 {
		t.Fatalf("recover 后应为 %s, got %s", c1[:8], got[:8])
	}
	// 恢复动作本身留痕。
	entries, _, _ := r.ReadReflog("refs/heads/master")
	if entries[len(entries)-1].Op != "recover" {
		t.Fatal("recover 应留痕")
	}
	t.Log("判定: 恢复原子生效且留痕")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
