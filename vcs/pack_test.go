package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countFiles 统计目录下常规文件数量。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// snapshotAll 读取全部可达历史，用于归档前后比对。
func snapshotAll(t *testing.T, r *Repo) map[string]string {
	t.Helper()
	out := map[string]string{}
	refs, err := r.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range refs {
		out["ref:"+name] = id
	}
	reach, err := r.Reachable()
	if err != nil {
		t.Fatal(err)
	}
	for id := range reach {
		typ, payload, err := r.ReadObject(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		out["obj:"+id] = typ + ":" + string(payload)
	}
	return out
}

// TestPackReadConsistency 场景：归档前后所有读取结果完全一致。
func TestPackReadConsistency(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 30; i++ {
		writeWork(t, r, fmt.Sprintf("dir/f%02d.txt", i), fmt.Sprintf("content-%d", i))
		commitAll(t, r, fmt.Sprintf("c%d", i))
	}
	if err := r.CreateBranch("side"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "side.txt", "side")
	commitAll(t, r, "side-work")
	if err := r.Checkout("side"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "other.txt", "other")
	commitAll(t, r, "other-work")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Merge("side"); err != nil {
		t.Fatal(err)
	}

	before := snapshotAll(t, r)
	looseBefore := countFiles(t, r.objectsDir())
	res, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: %d 个松散对象归档为 %s", res.Packed, res.PackFile)
	after := snapshotAll(t, r)
	if len(before) != len(after) {
		t.Fatalf("归档前后可读集合不一致: %d vs %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("归档后内容变化: %s", k)
		}
	}
	looseAfter := countFiles(t, r.objectsDir())
	packs := countFiles(t, r.packsDir())
	t.Logf("判定: 文件数 %d -> %d(松散)+%d(归档), 读取结果 %d 项全部一致",
		looseBefore, looseAfter, packs, len(before))
	if looseAfter != 0 || packs != 1 {
		t.Fatalf("归档后应为 0 松散 + 1 包, got %d + %d", looseAfter, packs)
	}
}

// TestPackScaleReduction 场景：上万对象规模，文件数至少降一个数量级，读耗时不明显变差。
func TestPackScaleReduction(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	r := newRepo(t)
	const commits = 1200 // 约 3600+ 对象；测试机上兼顾速度
	for i := 0; i < commits; i++ {
		writeWork(t, r, "file.txt", fmt.Sprintf("v%d", i))
		if _, err := r.Commit(fmt.Sprintf("c%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	looseBefore := countFiles(t, r.objectsDir())
	t.Logf("场景: %d 次提交产生 %d 个松散文件", commits, looseBefore)

	start := time.Now()
	head, _ := r.CurrentCommit()
	if _, err := r.ReadCommit(head); err != nil {
		t.Fatal(err)
	}
	readBefore := time.Since(start)

	res, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	looseAfter := countFiles(t, r.objectsDir())
	packs := countFiles(t, r.packsDir())
	total := looseAfter + packs
	t.Logf("判定: 文件数 %d -> %d (压缩比 %.0fx)", looseBefore, total, float64(looseBefore)/float64(total))
	if total*10 > looseBefore {
		t.Fatalf("文件数应至少降一个数量级: %d -> %d", looseBefore, total)
	}
	if res.Packed < commits*2 {
		t.Fatalf("归档对象数异常: %d", res.Packed)
	}

	start = time.Now()
	if _, err := r.ReadCommit(head); err != nil {
		t.Fatal(err)
	}
	readAfter := time.Since(start)
	t.Logf("判定: 单对象读耗时 归档前 %v, 归档后 %v", readBefore, readAfter)
	if readAfter > readBefore*10+50*time.Millisecond {
		t.Fatalf("归档后读取明显变慢: %v -> %v", readBefore, readAfter)
	}
	// 全量历史遍历验证。
	reach, err := r.Reachable()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("判定: 归档后 %d 个可达对象全部可读", len(reach))
}

// TestPackIdempotent 场景：同一批内容反复归档幂等，不越归档越大。
func TestPackIdempotent(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 20; i++ {
		writeWork(t, r, "f.txt", fmt.Sprintf("v%d", i))
		commitAll(t, r, fmt.Sprintf("c%d", i))
	}
	res1, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	size1 := dirSize(t, r.packsDir())
	files1 := countFiles(t, r.packsDir())
	for i := 0; i < 3; i++ {
		res, err := r.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if res.Packed != 0 {
			t.Fatalf("第 %d 次重复归档不应产生新对象, packed=%d", i+2, res.Packed)
		}
	}
	size2 := dirSize(t, r.packsDir())
	files2 := countFiles(t, r.packsDir())
	t.Logf("判定: 首次归档 %d 对象/%d 字节/%d 文件, 重复归档后 %d 字节/%d 文件",
		res1.Packed, size1, files1, size2, files2)
	if size1 != size2 || files1 != files2 {
		t.Fatalf("重复归档不应增长: %dB/%df -> %dB/%df", size1, files1, size2, files2)
	}
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if st, err := d.Info(); err == nil {
				n += st.Size()
			}
		}
		return nil
	})
	return n
}

// TestPackCorruptionClassification 场景：归档文件被截断/篡改时按原因分类报错。
func TestPackCorruptionClassification(t *testing.T) {
	setup := func(t *testing.T) (*Repo, string) {
		r := newRepo(t)
		writeWork(t, r, "a.txt", "hello")
		head := commitAll(t, r, "c1")
		if _, err := r.Pack(); err != nil {
			t.Fatal(err)
		}
		return r, head
	}
	packPath := func(t *testing.T, r *Repo) string {
		entries, err := os.ReadDir(r.packsDir())
		if err != nil || len(entries) != 1 {
			t.Fatalf("expect 1 pack, err=%v", err)
		}
		return filepath.Join(r.packsDir(), entries[0].Name())
	}

	t.Run("truncated", func(t *testing.T) {
		r, head := setup(t)
		p := packPath(t, r)
		data, _ := os.ReadFile(p)
		if err := os.WriteFile(p, data[:len(data)/2], 0o644); err != nil {
			t.Fatal(err)
		}
		r2, _ := Open(r.Root)
		_, _, err := r2.ReadObject(head)
		tr, ok := err.(*ObjectTruncated)
		t.Logf("场景: 包文件截断一半, 读取报错 %v", err)
		if !ok {
			t.Fatalf("截断应报 ObjectTruncated, got %T %v", err, err)
		}
		if tr.ID != head {
			t.Fatalf("错误应带对象 ID")
		}
	})

	t.Run("tampered", func(t *testing.T) {
		r, head := setup(t)
		p := packPath(t, r)
		data, _ := os.ReadFile(p)
		data[len(data)/2] ^= 0xff // 篡改内容但不改长度
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		r2, _ := Open(r.Root)
		_, _, err := r2.ReadObject(head)
		t.Logf("场景: 包内容篡改一字节, 读取报错 %v", err)
		if _, ok := err.(*ObjectTampered); !ok {
			t.Fatalf("篡应报 ObjectTampered, got %T %v", err, err)
		}
	})

	t.Run("bad magic", func(t *testing.T) {
		r, head := setup(t)
		p := packPath(t, r)
		data, _ := os.ReadFile(p)
		copy(data, "GARBAGE!!")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		r2, _ := Open(r.Root)
		_, _, err := r2.ReadObject(head)
		t.Logf("场景: 包头损坏, 读取报错 %v", err)
		if _, ok := err.(*ObjectCorrupt); !ok {
			t.Fatalf("头部损坏应报 ObjectCorrupt, got %T %v", err, err)
		}
	})
}

// TestPackCrashRecovery 场景：归档中途被强杀，重启可用且重试成功。
func TestPackCrashRecovery(t *testing.T) {
	r := newRepo(t)
	for i := 0; i < 10; i++ {
		writeWork(t, r, "f.txt", fmt.Sprintf("v%d", i))
		commitAll(t, r, fmt.Sprintf("c%d", i))
	}
	// 模拟崩溃现场：packs 目录里留下未 rename 的临时文件。
	if err := os.MkdirAll(r.packsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(r.packsDir(), "pack-12345.tmp")
	if err := os.WriteFile(tmp, []byte("partial pack data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 重启打开：仓库照常可读。
	r2, err := Open(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := r2.CurrentCommit()
	if _, err := r2.ReadCommit(head); err != nil {
		t.Fatalf("崩溃残留不应影响读取: %v", err)
	}
	before := snapshotAll(t, r2)
	// 重试归档成功，临时文件被清理。
	res, err := r2.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("重试应清理崩溃残留的临时文件")
	}
	after := snapshotAll(t, r2)
	if len(before) != len(after) {
		t.Fatal("重试归档前后读取应一致")
	}
	t.Logf("判定: 崩溃残留清理, 重试归档 %d 对象成功, 读取一致", res.Packed)
}

// TestPackConcurrentWrites 场景：归档与正常写入并发，结果稳定。
func TestPackConcurrentWrites(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "base.txt", "base")
	commitAll(t, r, "base")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	// 写入协程：持续产生新对象。
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
				payload := fmt.Sprintf("w%d-%d", n, i)
				id, err := r.WriteObject(TypeBlob, []byte(payload))
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
	// 归档协程：反复归档。
	for i := 0; i < 20; i++ {
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
	// 最终一致性：再做一次归档，全部对象可读。
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reachable(); err != nil {
		t.Fatal(err)
	}
	t.Log("判定: 并发归档+写入无错误, 仓库一致")
}

// TestPackSkipsCorruptLoose 场景：损坏的松散对象不入包，错误分类保留。
func TestPackSkipsCorruptLoose(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "ok")
	commitAll(t, r, "c1")
	// 手工制造一个损坏的松散对象。
	badID := strings.Repeat("deadbeef", 8)
	p := r.loosePath(badID)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("garbage"), 0o644)

	res, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("场景: 混入损坏松散对象, 归档跳过 %v", res.Skipped)
	if len(res.Skipped) != 1 || res.Skipped[0] != badID {
		t.Fatalf("损坏对象应被跳过并报告, got %v", res.Skipped)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("损坏对象应留在原地")
	}
	if _, _, err := r.ReadObject(badID); err == nil {
		t.Fatal("损坏对象读取仍应报错")
	}
	t.Log("判定: 损坏对象不入包、不丢失、读取仍分类报错")
}
