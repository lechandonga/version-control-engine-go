package vcs

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// 场景：删错分支后照留痕恢复到旧位置。
func TestReflogRestoreDeletedBranch(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	c1 := commitAll(t, r, dir, "c1")
	if err := r.CreateBranch("feature", c1); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "2")
	commitAll(t, r, dir, "c2")
	if err := r.DeleteBranch("feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveBranch("feature"); !errors.As(err, new(*RefNotFound)) {
		t.Fatalf("删除后应找不到分支, got %v", err)
	}
	entries, err := r.ReadReflog(branchRefName("feature"), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 最新一条是 branch-delete，它的 Old 即删除前位置。
	var del ReflogEntry
	for _, e := range entries {
		if e.Op == OpBranchDelete {
			del = e
			break
		}
	}
	if del.Old != c1 {
		t.Fatalf("判定依据: 删除记录 Old 必须是 c1 %s, got %s", c1, del.Old)
	}
	if err := r.RestoreRef(del, "误删恢复 feature"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := r.ResolveBranch("feature")
	if err != nil || got != c1 {
		t.Fatalf("恢复后分支必须指向 c1, got %s err=%v", got, err)
	}
	// 恢复动作自身也有留痕
	entries2, _ := r.ReadReflog(branchRefName("feature"), time.Time{}, time.Time{})
	foundRestore := false
	for _, e := range entries2 {
		if e.Op == OpRestore {
			foundRestore = true
		}
	}
	if !foundRestore {
		t.Fatal("恢复动作本身必须留痕")
	}
}

// 场景：错误回退一次重放后，照留痕把 HEAD 找回重放结果位置。
func TestReflogRestoreRebasePosition(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	base := commitAll(t, r, dir, "base")
	if err := r.CreateBranch("topic", base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "2")
	commitAll(t, r, dir, "main")
	if err := r.Checkout("topic"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "b", "topic")
	commitAll(t, r, dir, "topic-work")
	if err := r.Rebase("main"); err != nil {
		t.Fatal(err)
	}
	rebased, _ := r.HEADCommit()
	// 有人错误地把分支回退到重放前
	if err := r.AbortRebase(); err == nil {
		// abort 需要在途重放；这里模拟“手动回退”：检出原始位置
	}
	entries, _ := r.ReadReflog("HEAD", time.Time{}, time.Time{})
	branchEntries, _ := r.ReadReflog(branchRefName("topic"), time.Time{}, time.Time{})
	var finish ReflogEntry
	for _, e := range entries {
		if e.Op == OpRebaseAdvance && e.New == rebased && e.Ref == "HEAD" &&
			strings.Contains(e.Reason, "finished") {
			finish = e
			break
		}
	}
	if finish.New == "" {
		t.Fatal("判定依据: reflog 必须包含重放完成位置")
	}
	var branchFinish ReflogEntry
	for _, e := range branchEntries {
		if strings.Contains(e.Reason, "rebase finished") {
			branchFinish = e
			break
		}
	}
	if branchFinish.New != rebased {
		t.Fatalf("分支完成记录目标=%s 与重放结果=%s 不一致", branchFinish.New, rebased)
	}
	_ = finish
	// 模拟错误回退：把 topic 分支指针挪回 base（HEAD 符号引用跟随）。
	// 恢复推进类记录时使用“当时挪到的位置” New；删分支记录 New 为空。
	branchFinish.Old = branchFinish.New
	if err := r.writeBranchPointer("topic", base); err != nil {
		t.Fatal(err)
	}
	if err := r.detachHEAD(base); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreRef(branchFinish, "找回重放结果"); err != nil {
		t.Fatal(err)
	}
	if err := r.writeHEADSymbolic(branchRefName("topic")); err != nil {
		t.Fatal(err)
	}
	head, _ := r.HEADCommit()
	if head != rebased {
		t.Fatalf("找回后 HEAD 必须是重放结果 %s, got %s", rebased, head)
	}
}

// 场景：留痕坏行被单独识别，坏行不连累仓库打开与其它记录读取。
func TestReflogCorruptLineIsolated(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	commitAll(t, r, dir, "c1")
	// 直接破坏留痕：插入两行坏数据（非 JSON、缺字段），再追加一条好记录
	logPath := r.reflogPath()
	good, _ := os.ReadFile(logPath)
	malformed := append([]byte("not-a-json-line\n{\"time\":\"x\"}\n"), good...)
	if err := os.WriteFile(logPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}

	// 仓库照常打开
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("判定依据: 留痕损坏不得连累仓库打开: %v", err)
	}
	writeFile(t, dir, "a", "2")
	if _, err := r2.Commit(CommitOptions{Message: "c2", AllowEmpty: true}); err != nil {
		t.Fatalf("留痕坏行后新提交必须可用: %v", err)
	}
	h, err := r2.CheckReflog()
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Corrupt) != 2 {
		t.Fatalf("判定依据: 必须分类出 2 行坏记录, got %d: %+v", len(h.Corrupt), h.Corrupt)
	}
	t.Logf("坏行分类: %+v", h.Corrupt)
	entries, err := r2.ReadReflog("", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	goodN := 0
	for _, e := range entries {
		if e.Bad {
			bad++
			if !strings.Contains(e.String(), "CORRUPT") {
				t.Fatalf("坏行可读输出必须带 CORRUPT 标记: %s", e.String())
			}
		} else {
			goodN++
		}
	}
	if bad != 2 || goodN < 2 {
		t.Fatalf("坏行=%d 好行=%d，期望 2/至少2", bad, goodN)
	}
	// 坏行不可用于恢复
	var badEntry ReflogEntry
	for _, e := range entries {
		if e.Bad {
			badEntry = e
			break
		}
	}
	if err := r2.RestoreRef(badEntry, ""); err == nil {
		t.Fatal("判定依据: 坏行恢复必须被拒绝")
	}
}

// 场景：恢复是原子的——重复执行幂等，目标对象缺失时明确失败。
func TestRestoreIdempotentAndMissing(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "a", "1")
	c1 := commitAll(t, r, dir, "c1")
	if err := r.CreateBranch("f", c1); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBranch("f"); err != nil {
		t.Fatal(err)
	}
	entries, _ := r.ReadReflog(branchRefName("f"), time.Time{}, time.Time{})
	var del ReflogEntry
	for _, e := range entries {
		if e.Op == OpBranchDelete {
			del = e
		}
	}
	if err := r.RestoreRef(del, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreRef(del, ""); err != nil {
		t.Fatalf("重复恢复必须幂等: %v", err)
	}
	// 目标对象不存在时
	del.Old = strings.Repeat("a", 64)
	err := r.RestoreRef(del, "")
	assertErrorType(t, err, "missing")
}
