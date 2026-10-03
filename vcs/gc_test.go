package vcs

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// writeLoose 直接写入一个不挂在任何引用下的对象。
func writeLoose(t *testing.T, r *Repo, content string) string {
	t.Helper()
	id, err := r.WriteObject(TypeBlob, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestGCReachability 场景：可达性判断覆盖分支、HEAD、在途现场与操作记录。
func TestGCReachability(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")

	// 不可达对象（无任何根引用）。
	garbage := writeLoose(t, r, "garbage")
	// 仅被操作记录引用的对象：删分支后其位置仍在日志里。
	if err := r.CreateBranch("tmp"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("tmp"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "t.txt", "tmp")
	tmpC := commitAll(t, r, "tmp-work")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBranch("tmp"); err != nil {
		t.Fatal(err)
	}

	reach, err := r.Reachable()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 可达根含分支/HEAD/日志; 检查 c1 可达=%v, 日志保留的提交可达=%v, 垃圾可达=%v",
		reach[c1], reach[tmpC], reach[garbage])
	if !reach[c1] {
		t.Fatal("分支指向的提交应可达")
	}
	if !reach[tmpC] {
		t.Fatal("被操作记录引用的位置应可达（防止找回前被回收）")
	}
	if reach[garbage] {
		t.Fatal("无根引用的对象应不可达")
	}

	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Removed, garbage) {
		t.Fatalf("垃圾对象应被回收: %v", res.Removed)
	}
	if contains(res.Removed, tmpC) || contains(res.Removed, c1) {
		t.Fatal("可达对象不应被回收")
	}
	if r.HasObject(garbage) {
		t.Fatal("回收后垃圾对象应不存在")
	}
	if !r.HasObject(tmpC) {
		t.Fatal("日志引用的对象应保留")
	}
	t.Log("判定: 可达性含操作记录, 回收只清垃圾")
}

// TestGCInFlightState 场景：在途重放/合并现场是可达根。
func TestGCInFlightState(t *testing.T) {
	t.Run("rebase", func(t *testing.T) {
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
		writeWork(t, r, "f.txt", "feature-v")
		orig := commitAll(t, r, "f1")
		if err := r.Rebase("master"); err == nil {
			t.Fatal("期望冲突暂停")
		}
		// 在途现场引用的对象必须可达。
		reach, err := r.Reachable()
		if err != nil {
			t.Fatal(err)
		}
		if !reach[orig] {
			t.Fatal("在途重放的原始位置必须可达")
		}
		res, err := r.GC(GCOptions{Grace: 0})
		if err != nil {
			t.Fatal(err)
		}
		if contains(res.Removed, orig) {
			t.Fatal("在途重放现场引用的对象不得回收")
		}
		// 回退后仓库完整可用。
		if err := r.RebaseAbort(); err != nil {
			t.Fatal(err)
		}
		if got := readWork(t, r, "f.txt"); got != "feature-v" {
			t.Fatal("回收后重放现场回退仍正常")
		}
		t.Log("判定: 在途重放现场受保护, 回收后回退正常")
	})

	t.Run("merge", func(t *testing.T) {
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
		if _, err := r.Merge("side"); err == nil {
			t.Fatal("期望冲突")
		}
		reach, err := r.Reachable()
		if err != nil {
			t.Fatal(err)
		}
		if !reach[theirs] {
			t.Fatal("在途合并的对方提交必须可达")
		}
		res, err := r.GC(GCOptions{Grace: 0})
		if err != nil {
			t.Fatal(err)
		}
		if contains(res.Removed, theirs) {
			t.Fatal("在途合并现场引用的对象不得回收")
		}
		if err := r.MergeAbort(); err != nil {
			t.Fatal(err)
		}
		t.Log("判定: 在途合并现场受保护")
	})
}

// TestGCGracePeriod 场景：保留期边界——新对象不清，老对象才清。
func TestGCGracePeriod(t *testing.T) {
	r := newRepo(t)
	commitAll(t, r, "c1")
	fresh := writeLoose(t, r, "fresh-garbage")
	old := writeLoose(t, r, "old-garbage")
	// 把 old 的修改时间调到保留期之外。
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(r.loosePath(old), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	res, err := r.GC(GCOptions{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 保留期 24h, fresh 保留=%v, old 删除=%v",
		contains(res.Kept, fresh), contains(res.Removed, old))
	if !contains(res.Kept, fresh) {
		t.Fatal("保留期内的对象不得回收")
	}
	if !contains(res.Removed, old) {
		t.Fatal("超出保留期的对象应回收")
	}
	if !r.HasObject(fresh) {
		t.Fatal("保留期内对象应仍在")
	}
	if r.HasObject(old) {
		t.Fatal("超期对象应已删除")
	}
	t.Log("判定: 保留期边界正确")
}

// TestGCDryRun 场景：预览只报告不删除。
func TestGCDryRun(t *testing.T) {
	r := newRepo(t)
	commitAll(t, r, "c1")
	garbage := writeLoose(t, r, "garbage")
	res, err := r.GC(GCOptions{DryRun: true, Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Removed, garbage) {
		t.Fatal("预览应列出将删除的对象")
	}
	if !r.HasObject(garbage) {
		t.Fatal("预览不得真正删除")
	}
	t.Log("判定: dry-run 只预览不动手")
}

// TestGCInterruptReentrant 场景：回收中途被强杀，重试成功且不误删。
func TestGCInterruptReentrant(t *testing.T) {
	r := newRepo(t)
	ids := []string{}
	for i := 0; i < 5; i++ {
		ids = append(ids, writeLoose(t, r, fmt.Sprintf("garbage-%d", i)))
	}
	keep := commitAll(t, r, "keep")
	// 第一次预览拿到完整清单。
	preview, err := r.GC(GCOptions{DryRun: true, Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟上次回收跑到一半被强杀：只删掉清单里前两个。
	for _, id := range preview.Removed[:2] {
		os.Remove(r.loosePath(id))
	}
	// 重试：应删掉剩余三个，且不报错、不误删可达对象。
	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 上次删掉 2/%d 后中断, 重试删除 %d 个", len(preview.Removed), len(res.Removed))
	if len(res.Removed) != len(preview.Removed)-2 {
		t.Fatalf("重试应只补删剩余对象, got %d", len(res.Removed))
	}
	for _, id := range ids {
		if r.HasObject(id) {
			t.Fatalf("垃圾对象 %s 应被清理", id[:8])
		}
	}
	if !r.HasObject(keep) {
		t.Fatal("可达对象不得误删")
	}
	// 第三次运行：无事可做，幂等。
	res3, err := r.GC(GCOptions{Grace: 0})
	if err != nil || len(res3.Removed) != 0 {
		t.Fatalf("重复回收应幂等: %v %v", res3, err)
	}
	t.Log("判定: 中断可重入, 重试幂等")
}

// TestGCPreservesHistory 场景：回收后历史读取、合并、重放结果与回收前一致。
func TestGCPreservesHistory(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	commitAll(t, r, "c1")
	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "b.txt", "2")
	commitAll(t, r, "c2")
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "c.txt", "3")
	commitAll(t, r, "c3")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	before := snapshotAll(t, r)
	// 制造垃圾并回收。
	for i := 0; i < 10; i++ {
		writeLoose(t, r, fmt.Sprintf("junk-%d", i))
	}
	if _, err := r.GC(GCOptions{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	after := snapshotAll(t, r)
	if len(before) != len(after) {
		t.Fatalf("回收前后可达集合应一致: %d vs %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("回收后内容变化: %s", k)
		}
	}
	// 回收后合并与重放照常。
	if _, err := r.Merge("feature"); err != nil {
		t.Fatalf("回收后合并失败: %v", err)
	}
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	if err := r.Rebase("master"); err != nil {
		t.Fatalf("回收后重放失败: %v", err)
	}
	t.Log("判定: 回收后读取/合并/重放结果一致")
}

// TestGCPackCoexist 场景：归档与回收配合——回收不动归档内对象，历史完整。
func TestGCPackCoexist(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 5; i++ {
		writeWork(t, r, "f.txt", fmt.Sprintf("v%d", i))
		commitAll(t, r, fmt.Sprintf("c%d", i))
	}
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	garbage := writeLoose(t, r, "garbage")
	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Removed, garbage) {
		t.Fatal("松散垃圾应被回收")
	}
	head, _ := r.CurrentCommit()
	if _, err := r.ReadCommit(head); err != nil {
		t.Fatalf("归档内历史应完整可读: %v", err)
	}
	t.Log("判定: 回收只动松散对象, 归档历史不受影响")
}
