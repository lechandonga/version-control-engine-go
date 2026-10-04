package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestGCPackThenGC 场景：先归档再回收——已进归档包的无引用对象
// 预览时如实列出、确认后真正清掉，可达对象不受影响，仓库体积下降。
func TestGCPackThenGC(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	garbage := writeLoose(t, r, "orphan-blob")

	packRes, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	oldPack := filepath.Join(r.packsDir(), packRes.PackFile)
	if _, err := os.Stat(oldPack); err != nil {
		t.Fatal("归档包应已生成")
	}
	t.Logf("场景: 垃圾对象 %s 已收进归档包 %s", garbage[:8], packRes.PackFile)

	// 预览：如实列出已归档的无引用对象，但不真正删除。
	dry, err := r.GC(GCOptions{DryRun: true, Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(dry.Removed, garbage) {
		t.Fatalf("预览应列出已归档的垃圾对象: %v", dry.Removed)
	}
	if !r.HasObject(garbage) {
		t.Fatal("预览不得真正删除")
	}

	// 确认执行：真正清掉，仓库瘦下来。
	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("回收结果: removed=%v", shortIDs(res.Removed))
	if !contains(res.Removed, garbage) {
		t.Fatalf("已归档的垃圾对象应被回收: %v", res.Removed)
	}
	if r.HasObject(garbage) {
		t.Fatal("回收后垃圾对象应不存在")
	}
	if _, err := os.Stat(oldPack); !os.IsNotExist(err) {
		t.Fatal("含垃圾的旧归档包应被重写替换")
	}
	// 可达对象（含已归档的历史）一个都不能动。
	if !r.HasObject(c1) {
		t.Fatal("可达对象不得被回收")
	}
	if _, err := r.ReadCommit(c1); err != nil {
		t.Fatalf("归档内历史应完整可读: %v", err)
	}
	if got := readWork(t, r, "a.txt"); got != "1" {
		t.Fatal("工作区内容不应变化")
	}
	// 幂等：再跑一次无事可做。
	res2, err := r.GC(GCOptions{Grace: 0})
	if err != nil || len(res2.Removed) != 0 {
		t.Fatalf("重复回收应幂等: %v %v", res2.Removed, err)
	}
	t.Log("判定: 先归档后回收可清掉无引用对象, 可达对象不受影响, 结果幂等")
}

// TestGCPackOnlyGarbage 场景：整包都是无引用对象时，回收直接删除包文件。
func TestGCPackOnlyGarbage(t *testing.T) {
	r := newRepo(t)
	commitAll(t, r, "c1")
	g1 := writeLoose(t, r, "junk-1")
	g2 := writeLoose(t, r, "junk-2")
	res, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(r.packsDir(), res.PackFile)
	if _, err := r.GC(GCOptions{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packPath); !os.IsNotExist(err) {
		t.Fatal("纯垃圾的归档包应被整体删除")
	}
	if r.HasObject(g1) || r.HasObject(g2) {
		t.Fatal("包内垃圾应被清掉")
	}
	t.Log("判定: 纯垃圾归档包整体删除, 仓库体积下降")
}

// TestGCPackGracePeriod 场景：保留期对已归档对象同样生效——
// 归档和回收的先后顺序不会让保留期内的对象被误清。
func TestGCPackGracePeriod(t *testing.T) {
	r := newRepo(t)
	commitAll(t, r, "c1")
	fresh := writeLoose(t, r, "fresh-garbage")
	packRes, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(r.packsDir(), packRes.PackFile)

	// 刚归档的包在保留期内：整包保留。
	res, err := r.GC(GCOptions{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 保留期 24h, 新归档包内垃圾保留=%v", contains(res.Kept, fresh))
	if !contains(res.Kept, fresh) {
		t.Fatal("保留期内的已归档对象不得回收")
	}
	if !r.HasObject(fresh) {
		t.Fatal("保留期内对象应仍在")
	}
	// 包年龄超出保留期后：正常回收。
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(packPath, old, old); err != nil {
		t.Fatal(err)
	}
	res2, err := r.GC(GCOptions{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res2.Removed, fresh) {
		t.Fatal("超出保留期的已归档对象应回收")
	}
	if r.HasObject(fresh) {
		t.Fatal("超期对象应已删除")
	}
	t.Log("判定: 保留期对已归档对象生效, 归档/回收顺序不影响保护")
}

// TestGCPackCorruptUntouched 场景：归档文件里混有坏数据时，
// 回收跳过损坏包（保持原样），好包照常回收，仓库照常可读。
func TestGCPackCorruptUntouched(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	g1 := writeLoose(t, r, "garbage-in-good-pack")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	g2 := writeLoose(t, r, "garbage-in-corrupt-pack")
	bad, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	// 损坏第二个包（内容篡改）。
	badPath := filepath.Join(r.packsDir(), bad.PackFile)
	data, _ := os.ReadFile(badPath)
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(badPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(badPath)
	r.invalidatePacks()

	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatalf("损坏的归档包不应让回收失败: %v", err)
	}
	t.Logf("场景: 好包垃圾回收=%v, 坏包保持原样", contains(res.Removed, g1))
	if !contains(res.Removed, g1) {
		t.Fatal("好包里的垃圾应被回收")
	}
	if contains(res.Removed, g2) {
		t.Fatal("损坏包内的对象不应被处理")
	}
	after, _ := os.ReadFile(badPath)
	if string(before) != string(after) {
		t.Fatal("损坏的归档包应保持原样不动")
	}
	// 仓库照常可读。
	if _, err := r.ReadCommit(c1); err != nil {
		t.Fatalf("损坏包不应影响正常读取: %v", err)
	}
	t.Log("判定: 损坏包隔离不动, 好包回收正常, 仓库可读")
}

// TestGCPackRewriteCrashReentrant 场景：回收重写归档包中途被强杀，
// 重启后仓库可读，重试接着做完，结果稳定。
func TestGCPackRewriteCrashReentrant(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	garbage := writeLoose(t, r, "packed-garbage")
	packRes, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	// 保存旧包副本，用于模拟“新包已落盘、旧包未删”的崩溃现场。
	saved := filepath.Join(t.TempDir(), "old.pack")
	data, _ := os.ReadFile(filepath.Join(r.packsDir(), packRes.PackFile))
	if err := os.WriteFile(saved, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.GC(GCOptions{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	if r.HasObject(garbage) {
		t.Fatal("首次回收应清掉垃圾")
	}
	// 模拟崩溃残留：旧包（仍含垃圾）又回到 packs 目录，与新包并存。
	if err := os.WriteFile(filepath.Join(r.packsDir(), packRes.PackFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// 重启打开：仓库照常可读。
	r2, err := Open(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReadCommit(c1); err != nil {
		t.Fatalf("崩溃残留不应影响读取: %v", err)
	}
	// 重试：接着把残留旧包里的垃圾清掉。
	res, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 崩溃后新旧包并存, 重试回收 removed=%v", shortIDs(res.Removed))
	if !contains(res.Removed, garbage) {
		t.Fatal("重试应清掉残留旧包里的垃圾")
	}
	if r2.HasObject(garbage) {
		t.Fatal("重试后垃圾应不存在")
	}
	if _, err := r2.ReadCommit(c1); err != nil {
		t.Fatal("重试后历史应完整可读")
	}
	// 第三次运行：幂等。
	res3, err := r2.GC(GCOptions{Grace: 0})
	if err != nil || len(res3.Removed) != 0 {
		t.Fatalf("重复回收应幂等: %v %v", res3.Removed, err)
	}
	t.Log("判定: 崩溃残留可重入清理, 重试幂等, 仓库可读")
}

// TestGCPackConcurrentWrites 场景：回收/归档与正常写入并发，结果稳定。
func TestGCPackConcurrentWrites(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "base.txt", "base")
	head := commitAll(t, r, "base")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	stop := make(chan struct{})
	// 写入协程：持续产生新对象（未挂引用的随时可能被回收，属正常）。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := r.WriteObject(TypeBlob, []byte(fmt.Sprintf("w%d-%d", n, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	// 维护协程：归档与回收交替进行。
	for i := 0; i < 15; i++ {
		if _, err := r.Pack(); err != nil {
			errs <- err
		}
		if _, err := r.GC(GCOptions{Grace: 0}); err != nil {
			errs <- err
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发错误: %v", err)
	}
	// 最终一致性：可达历史完整可读。
	if _, err := r.Reachable(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadCommit(head); err != nil {
		t.Fatalf("并发维护后历史应完整可读: %v", err)
	}
	if got := readWork(t, r, "base.txt"); got != "base" {
		t.Fatal("工作区内容不应变化")
	}
	t.Log("判定: 并发回收/归档/写入无错误, 可达历史完整")
}

func shortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shortID(id)
	}
	return out
}
