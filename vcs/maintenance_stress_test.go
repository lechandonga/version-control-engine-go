package vcs

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestGCConcurrentWrites 场景：回收（含归档对象回收）与正常写入并发进行，
// 重启打开后仓库一致，重试结果稳定。
func TestGCConcurrentWrites(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "base.txt", "base")
	commitAll(t, r, "base")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 128)
	stop := make(chan struct{})
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
				payload := []byte("w" + string(rune('0'+n)) + "-" + itoaStress(i))
				id, err := r.WriteObject(TypeBlob, payload)
				if err != nil {
					errs <- err
					return
				}
				if _, _, err := r.ReadObject(id); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for i := 0; i < 10; i++ {
		if _, err := r.GC(GCOptions{Grace: 0}); err != nil {
			errs <- err
		}
		if _, err := r.Pack(); err != nil {
			errs <- err
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发错误: %v", err)
	}

	// 重启后仓库可读，可达集合稳定，重复维护结果不变。
	r2, err := Open(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r2.CurrentCommit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReadCommit(head); err != nil {
		t.Fatalf("并发维护后历史必须可读: %v", err)
	}
	if _, err := r2.Reachable(); err != nil {
		t.Fatalf("并发维护后可达性必须可计算: %v", err)
	}
	res1, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Removed) != 0 {
		t.Fatalf("重启后重试回收应幂等: 首次清 %d, 二次清 %d", len(res1.Removed), len(res2.Removed))
	}
	if _, err := r2.ReadCommit(head); err != nil {
		t.Fatalf("回收后历史仍必须可读: %v", err)
	}
	t.Logf("判定: 并发 gc/pack/写入无错误, 重启可读, 首次清 %d 后重试稳定", len(res1.Removed))
}

// TestGCInterruptReentrantPacked 场景：回收重写归档包途中被强杀，
// 现场残留临时文件；重启后仓库照常可读，重试接着做完，结果稳定。
func TestGCInterruptReentrantPacked(t *testing.T) {
	r, c1, c2 := setupHistory(t)
	if err := r.DeleteBranch("other"); err != nil {
		t.Fatal(err)
	}
	dangling1 := writeLoose(t, r, "dangling-one")
	dangling2 := writeLoose(t, r, "dangling-two")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	// 模拟“回收重写包时被强杀”：packs 目录留下半成品临时包。
	if err := os.MkdirAll(r.packsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(r.packsDir(), "pack-crash.tmp")
	if err := os.WriteFile(tmp, []byte("partial rewritten pack"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotAll(t, r2)
	if !r2.HasObject(dangling1) || !r2.HasObject(dangling2) {
		t.Fatal("崩溃后对象仍应可读（旧包未被替换）")
	}
	res, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatalf("重试回收不应失败: %v", err)
	}
	if !contains(res.Removed, dangling1) || !contains(res.Removed, dangling2) {
		t.Fatalf("重试应清掉两个已归档垃圾, got %v", res.Removed)
	}
	if r2.HasObject(dangling1) || r2.HasObject(dangling2) {
		t.Fatal("重试后垃圾应真正消失")
	}
	if !r2.HasObject(c1) || !r2.HasObject(c2) {
		t.Fatal("可达对象必须完好")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("回收重入应清理崩溃残留的临时包")
	}
	after := snapshotAll(t, r2)
	if len(before) != len(after) {
		// snapshotAll 只含可达对象，必须与崩溃前一致。
		t.Fatalf("可达集合崩溃前后不一致: %d vs %d", len(before), len(after))
	}
	res2, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Removed) != 0 {
		t.Fatalf("二次回收应无对象可清, got %v", res2.Removed)
	}
	t.Log("判定: 回收中断残留清理, 重试完成且幂等, 可达历史完好")
}

func itoaStress(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
