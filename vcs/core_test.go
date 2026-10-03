package vcs

import (
	"errors"
	"path/filepath"
	"testing"
)

// 基础场景：提交历史、分支、快进/三方合并判定结果稳定。
func TestCommitBranchMerge(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a.txt", "v1\n")
	c1 := commitAll(t, r, dir, "c1")

	if err := r.CreateBranch("dev", c1); err != nil {
		t.Fatal(err)
	}
	// main 上前进一步
	writeFile(t, dir, "a.txt", "v1\nmain\n")
	c2 := commitAll(t, r, dir, "c2-main")

	// dev 上前进（改另一个文件，避免冲突）
	if err := r.Checkout("dev"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "b.txt", "dev\n")
	c3 := commitAll(t, r, dir, "c3-dev")

	res, err := r.Merge("main", "")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Mode != "merge-commit" {
		t.Fatalf("判定依据: base=c1 两侧各有提交 => 期望 merge-commit, 实际 %s", res.Mode)
	}
	merged, err := r.ReadCommit(res.CommitID)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Parents) != 2 || merged.Parents[0] != c3 || merged.Parents[1] != c2 {
		t.Fatalf("合并提交父节点错误: %+v", merged.Parents)
	}
	if got := readWorkdirFile(t, dir, "a.txt"); got != "v1\nmain\n" {
		t.Fatalf("a.txt 内容错误: %q", got)
	}
	if got := readWorkdirFile(t, dir, "b.txt"); got != "dev\n" {
		t.Fatalf("b.txt 内容错误: %q", got)
	}

	// 快进判定
	r2, _ := newTestRepo(t)
	writeFile(t, filepath.Dir(r2.root), "x", "1")
	base := commitAll(t, r2, filepath.Dir(r2.root), "base")
	if err := r2.CreateBranch("f", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Dir(r2.root), "x", "2")
	commitAll(t, r2, filepath.Dir(r2.root), "tip")
	if err := r2.Checkout("f"); err != nil {
		t.Fatal(err)
	}
	ff, err := r2.Merge("main", "")
	if err != nil {
		t.Fatal(err)
	}
	if ff.Mode != "fast-forward" {
		t.Fatalf("判定依据: base==HEAD => 期望 fast-forward, 实际 %s", ff.Mode)
	}
}

// 合并冲突分类与现场保留。
func TestMergeConflictKeepsState(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "f", "base\n")
	c1 := commitAll(t, r, dir, "base")
	if err := r.CreateBranch("dev", c1); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "main change\n")
	commitAll(t, r, dir, "main")
	if err := r.Checkout("dev"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "dev change\n")
	commitAll(t, r, dir, "dev")

	_, err := r.Merge("main", "")
	var mc *MergeConflict
	if !errors.As(err, &mc) {
		t.Fatalf("期望 MergeConflict, 实际 %v", err)
	}
	if len(mc.Paths) != 1 || mc.Paths[0].Path != "f" {
		t.Fatalf("冲突清单错误: %+v", mc.Paths)
	}
	st, err := r.mergeInProgress()
	if err != nil {
		t.Fatalf("判定依据: 冲突后 merge-state 必须保留, err=%v", err)
	}
	if st.Conflicts[0].Ours == "" || st.Conflicts[0].Theirs == "" || st.Conflicts[0].Base == "" {
		t.Fatalf("冲突三方 blob 必须齐全: %+v", st.Conflicts[0])
	}
	if err := r.ResolveConflict("f", []byte("resolved\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mergeInProgress(); !errors.Is(err, ErrNoMerge) {
		t.Fatalf("消解后现场应清理, err=%v", err)
	}
	if got := readWorkdirFile(t, dir, "f"); got != "resolved\n" {
		t.Fatalf("消解结果错误: %q", got)
	}
}

// 重放：无冲突重放后提交顺序/内容不变，只换基。
func TestRebaseAdvance(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	base := commitAll(t, r, dir, "base")
	if err := r.CreateBranch("topic", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "2")
	commitAll(t, r, dir, "main-1")
	if err := r.Checkout("topic"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "t", "topic")
	topic1 := commitAll(t, r, dir, "topic-1")

	if err := r.Rebase("main"); err != nil {
		t.Fatalf("rebase: %v", err)
	}
	head, err := r.HEADCommit()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.ReadCommit(head)
	if err != nil {
		t.Fatal(err)
	}
	if c.Message != "topic-1" {
		t.Fatalf("重放后提交信息应保留, got %q", c.Message)
	}
	mainID, _ := r.ResolveBranch("main")
	if len(c.Parents) != 1 || c.Parents[0] != mainID {
		t.Fatalf("判定依据: 重放后父提交必须是 main 尖端 %s, got %v", mainID, c.Parents)
	}
	_ = topic1
	name, detached, err := r.CurrentBranch()
	if err != nil || detached || name != "topic" {
		t.Fatalf("重放结束应回到 topic 符号引用, got %q detached=%v err=%v", name, detached, err)
	}
	if got := readWorkdirFile(t, dir, "a"); got != "2" {
		t.Fatalf("重放后工作区应含 main 的内容, got %q", got)
	}
}

// 重放冲突暂停 / 继续 / 整体回退。
func TestRebasePauseContinueAbort(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "f", "base\n")
	base := commitAll(t, r, dir, "base")
	if err := r.CreateBranch("topic", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "main\n")
	commitAll(t, r, dir, "main")
	if err := r.Checkout("topic"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "topic\n")
	commitAll(t, r, dir, "topic-tip")

	err := r.Rebase("main")
	var rc *RebaseConflict
	if !errors.As(err, &rc) {
		t.Fatalf("期望 RebaseConflict, got %v", err)
	}
	st, err := r.RebaseInProgress()
	if err != nil {
		t.Fatal(err)
	}
	if st.StoppedCommit == "" || st.Queued != 1 || st.Applied != 0 {
		t.Fatalf("暂停状态错误: %+v", st)
	}
	// 未消解就 continue：必须再次报冲突，现场不动
	if err := r.ContinueRebase(); !errors.As(err, &rc) {
		t.Fatalf("判定依据: 未消解继续仍应报冲突, got %v", err)
	}
	if err := r.ResolveRebaseConflict("f", []byte("rebased\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.ContinueRebase(); err != nil {
		t.Fatalf("continue: %v", err)
	}
	head, _ := r.HEADCommit()
	c, _ := r.ReadCommit(head)
	if c.Message != "topic-tip" || readWorkdirFile(t, dir, "f") != "rebased\n" {
		t.Fatalf("重放继续结果错误")
	}

	// 再建一个重放并整体回退：位置必须回到 topicTip
	// main 与已重放的 topic 对同一文件做不同修改 -> 冲突
	// 已重放的 topic（f=rebased，父=main）再追上 main 新提交：
	// 构造 main 对 f 的新修改，topic 对 f 的修改在被重放提交里，
	// mergeBase=main1，三方均不同 => 冲突
	if err := r.Checkout("main"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "main-new\n")
	commitAll(t, r, dir, "main-new")
	if err := r.Checkout("topic"); err != nil {
		t.Fatal(err)
	}
	topicAtSecondRebase, _ := r.HEADCommit()
	r2err := r.Rebase("main")
	var rc2 *RebaseConflict
	if !errors.As(r2err, &rc2) {
		t.Fatalf("期望第二次重放冲突, got %v", r2err)
	}
	if err := r.AbortRebase(); err != nil {
		t.Fatal(err)
	}
	head, _ = r.HEADCommit()
	if head != topicAtSecondRebase {
		t.Fatalf("判定依据: 回退后 HEAD 必须是本次重放前位置 %s, got %s", topicAtSecondRebase, head)
	}
	if readWorkdirFile(t, dir, "f") != "rebased\n" {
		t.Fatalf("回退后工作区内容错误")
	}
}

// 未提交修改保护。
func TestWouldOverwrite(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "f", "v1")
	c1 := commitAll(t, r, dir, "c1")
	if err := r.CreateBranch("b", c1); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "f", "v2")
	// 只改工作区、不提交：检出 b 必须拒绝覆盖本地未提交修改
	if err := r.Checkout("b"); err != nil {
		var wo *WouldOverwrite
		if !errors.As(err, &wo) {
			t.Fatalf("期望 WouldOverwrite, got %v", err)
		}
		if len(wo.Paths) != 1 || wo.Paths[0] != "f" {
			t.Fatalf("覆盖清单错误: %v", wo.Paths)
		}
		return
	}
	t.Fatal("期望检出被拒绝")
}

func mustHead(t *testing.T, r *Repo) string {
	t.Helper()
	id, err := r.HEADCommit()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
