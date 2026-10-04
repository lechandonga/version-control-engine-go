package vcs

import (
	"errors"
	"os"
	"testing"
)

// TestDeleteCurrentBranchRejected 场景：删除当前所在分支被明确拒绝，
// 且分支、HEAD、工作区、操作记录都不发生任何变化。
func TestDeleteCurrentBranchRejected(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	entriesBefore, _, _ := r.ReadReflog("refs/heads/master")

	err := r.DeleteBranch("master")
	var cb *CurrentBranch
	if !errors.As(err, &cb) {
		t.Fatalf("删除当前分支应返回 CurrentBranch 错误, got %v", err)
	}
	t.Logf("场景: 删除当前分支被拒绝: %v", err)

	// 一切保持不变。
	if got, err := r.ReadRef("refs/heads/master"); err != nil || got != c1 {
		t.Fatalf("分支不应变化: %s err=%v", got, err)
	}
	if ref, err := r.HeadRef(); err != nil || ref != "refs/heads/master" {
		t.Fatalf("HEAD 不应变化: %q err=%v", ref, err)
	}
	if got := readWork(t, r, "a.txt"); got != "1" {
		t.Fatal("工作区不应变化")
	}
	entriesAfter, _, _ := r.ReadReflog("refs/heads/master")
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatal("拒绝删除不应产生新的操作记录")
	}
	// 拒绝后照常提交，历史链不断。
	writeWork(t, r, "a.txt", "2")
	c2 := commitAll(t, r, "c2")
	c, err := r.ReadCommit(c2)
	if err != nil || len(c.Parents) != 1 || c.Parents[0] != c1 {
		t.Fatalf("新提交应以 %s 为父: %+v err=%v", c1[:8], c.Parents, err)
	}
	t.Log("判定: 当前分支删除被拒, 分支/HEAD/工作区/记录均未变, 历史链完整")
}

// TestDeleteOtherBranchAndRecover 场景：删除非当前分支照常可用，
// 并能按操作记录找回。
func TestDeleteOtherBranchAndRecover(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	commitAll(t, r, "c1")
	if err := r.CreateBranch("side"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("side"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "s.txt", "side")
	tip := commitAll(t, r, "side-work")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBranch("side"); err != nil {
		t.Fatalf("删除非当前分支应成功: %v", err)
	}
	if _, err := r.ReadRef("refs/heads/side"); err == nil {
		t.Fatal("分支应已删除")
	}
	// 按记录找回。
	entries, _, err := r.ReadReflog("refs/heads/side")
	if err != nil {
		t.Fatal(err)
	}
	var lastPos string
	for _, e := range entries {
		if e.Op == "branch-delete" {
			lastPos = e.Old
		}
	}
	if lastPos != tip {
		t.Fatalf("记录的位置 %s != 删除前 %s", lastPos, tip)
	}
	if err := r.Recover("refs/heads/side", lastPos); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadRef("refs/heads/side"); got != tip {
		t.Fatalf("找回后应指向 %s, got %s", tip[:8], got[:8])
	}
	t.Log("判定: 非当前分支可删可按记录找回")
}

// TestDanglingHeadCommit 场景：老仓库已被旧版本误删当前分支（HEAD 悬空），
// 继续提交必须明确报错并给出恢复指引，而不是静默产生无父新根。
func TestDanglingHeadCommit(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	// 模拟老仓库的损坏状态：当前分支引用被直接删掉。
	if err := os.Remove(r.refPath("refs/heads/master")); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "a.txt", "2")
	_, err := r.Commit("c2")
	var hm *HeadBranchMissing
	if !errors.As(err, &hm) {
		t.Fatalf("悬空 HEAD 上提交应返回 HeadBranchMissing, got %v", err)
	}
	t.Logf("场景: 悬空 HEAD 提交被拒并给出指引: %v", err)
	// 没有产生新根：对象库里没有以 c1 之外为父的新提交，工作区未动。
	if got := readWork(t, r, "a.txt"); got != "2" {
		t.Fatal("工作区内容不应被维护操作改动")
	}
	// 按指引恢复后即可正常提交，且历史链接上。
	if err := r.Recover("refs/heads/master", c1[:8]); err != nil {
		t.Fatalf("用短标识恢复: %v", err)
	}
	c2, err := r.Commit("c2")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.ReadCommit(c2)
	if len(c.Parents) != 1 || c.Parents[0] != c1 {
		t.Fatalf("恢复后提交应接在 %s 之后: %+v", c1[:8], c.Parents)
	}
	t.Log("判定: 悬空 HEAD 明确报错, 恢复后历史链接续正常")
}

// TestFreshRepoCommitAllowed 回归：全新仓库（分支尚未创建）首次提交不受影响。
func TestFreshRepoCommitAllowed(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	c, _ := r.ReadCommit(c1)
	if len(c.Parents) != 0 {
		t.Fatal("全新仓库的首次提交应是根提交")
	}
	t.Log("判定: 新仓库首次提交不受影响")
}
