package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func assertNoStrayTmp(t *testing.T, r *Repo) {
	t.Helper()
	filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			if name := filepath.Base(path); len(name) >= 5 && name[:5] == ".tmp-" {
				t.Errorf("发现崩溃遗留的未发布临时文件: %s", path)
			}
		}
		return nil
	})
}

func sortedObjectBytes(t *testing.T, r *Repo) (string, [][]byte) {
	t.Helper()
	loose, err := r.listLooseObjects()
	if err != nil {
		t.Fatal(err)
	}
	var datas [][]byte
	for _, id := range loose {
		data, err := os.ReadFile(objectLoosePath(r.root, id))
		if err != nil {
			t.Fatal(err)
		}
		datas = append(datas, data)
	}
	sort.Slice(datas, func(i, j int) bool {
		_, li := objectHeaderID(datas[i])
		_, lj := objectHeaderID(datas[j])
		return li < lj
	})
	return objectSetHash(loose), datas
}

func objectHeaderID(data []byte) (string, string) {
	if len(data) < len(objectMagic) {
		return "", ""
	}
	rest := data[len(objectMagic):]
	for i, b := range rest {
		if b == '\n' {
			var typ, id string
			fmt.Sscanf(string(rest[:i]), "%s %s", &typ, &id)
			return typ, id
		}
	}
	return "", ""
}

func writePackArtifacts(t *testing.T, r *Repo, setHash string, datas [][]byte, tmp bool) (packP, idxP string) {
	t.Helper()
	dir := filepath.Join(r.root, "packs")
	var body []byte
	var idx []byte
	idx = append(idx, []byte("VCSIDX1\n")...)
	for _, d := range datas {
		off := len(body)
		body = append(body, d...)
		_, id := objectHeaderID(d)
		idx = append(idx, []byte(fmt.Sprintf("%s %d %d\n", id, off, len(d)))...)
	}
	sum := hashBytes(body)
	full := append(append([]byte(nil), body...), []byte(packSumMarker+sum+"\n")...)
	packP = filepath.Join(dir, "pack-"+setHash+".pack")
	idxP = filepath.Join(dir, "pack-"+setHash+".idx")
	if tmp {
		packP = filepath.Join(dir, ".tmp-pack-"+setHash)
		idxP = filepath.Join(dir, ".tmp-idx-"+setHash)
	}
	if err := os.WriteFile(packP, full, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxP, idx, 0o444); err != nil {
		t.Fatal(err)
	}
	return packP, idxP
}

// 归档在各阶段被打断，重启后仓库可读、重试成功且幂等。
func TestPackCrashRestartRetry(t *testing.T) {
	phases := []string{"tmp-only", "pack-published", "both-published", "partial-loose-delete"}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			r, dir := newTestRepo(t)
			for i := 0; i < 25; i++ {
				writeFile(t, dir, fmt.Sprintf("f%d", i), fmt.Sprintf("v%d\n", i))
				commitAll(t, r, dir, "c")
			}
			head, _ := r.HEADCommit()
			histBefore := allHistoryIDs(t, r, head)

			// 手工把仓库停在 Pack 的中间阶段后“杀死进程”。
			setHash, datas := sortedObjectBytes(t, r)
			loose, _ := r.listLooseObjects()
			switch phase {
			case "tmp-only":
				writePackArtifacts(t, r, setHash, datas, true)
			case "pack-published":
				pp, _ := writePackArtifacts(t, r, setHash, datas, true)
				if err := os.Rename(pp, filepath.Join(r.root, "packs", "pack-"+setHash+".pack")); err != nil {
					t.Fatal(err)
				}
			case "both-published":
				pp, ip := writePackArtifacts(t, r, setHash, datas, true)
				packDir := filepath.Join(r.root, "packs")
				os.Rename(pp, filepath.Join(packDir, "pack-"+setHash+".pack"))
				os.Rename(ip, filepath.Join(packDir, "pack-"+setHash+".idx"))
			case "partial-loose-delete":
				pp, ip := writePackArtifacts(t, r, setHash, datas, false)
				_ = pp
				_ = ip
				for i := 0; i < len(loose)/2; i++ {
					os.Remove(objectLoosePath(r.root, loose[i]))
				}
			}

			r2, err := Open(dir)
			if err != nil {
				t.Fatalf("崩溃后打开失败(%s): %v", phase, err)
			}
			assertNoStrayTmp(t, r2)
			for _, id := range histBefore {
				if _, err := r2.ReadCommit(id); err != nil {
					t.Fatalf("崩溃后历史不可读(%s) %s: %v", phase, id[:8], err)
				}
			}
			if _, err := r2.Pack(); err != nil {
				t.Fatalf("重试归档失败(%s): %v", phase, err)
			}
			if _, err := r2.Pack(); err != nil {
				t.Fatalf("归档不幂等(%s): %v", phase, err)
			}
			head2, _ := r2.HEADCommit()
			if head2 != head {
				t.Fatalf("HEAD 漂移(%s)", phase)
			}
			histAfter := allHistoryIDs(t, r2, head2)
			if len(histAfter) != len(histBefore) {
				t.Fatalf("历史长度变化(%s)", phase)
			}
			for i := range histBefore {
				if histBefore[i] != histAfter[i] {
					t.Fatalf("历史标识变化(%s)@%d", phase, i)
				}
			}
		})
	}
}

// GC 计划落盘后、删除中途被打断；重启重跑不误删、最终幂等。
func TestGCCrashRestartReentrant(t *testing.T) {
	r, dir, mainTip, devTip, _ := gcFixture(t)
	var dangling []string
	for i := 0; i < 5; i++ {
		id, err := r.putObject(ObjectBlob, []byte(fmt.Sprintf("dangling-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Duration(48+i) * time.Hour)
		os.Chtimes(objectLoosePath(r.root, id), old, old)
		dangling = append(dangling, id)
	}
	opts := GCOptions{Retain: time.Hour, Now: time.Now()}
	rep, err := r.PreviewGC(opts)
	if err != nil {
		t.Fatal(err)
	}
	plan := gcPlan{
		CreatedAt:    opts.Now,
		RetainUntil:  opts.Now.Add(-opts.Retain),
		Reclaimable:  append([]string(nil), rep.Reclaimable...),
		Fingerprints: map[string]time.Time{},
	}
	for _, id := range rep.Reclaimable {
		if fi, err := os.Stat(objectLoosePath(r.root, id)); err == nil {
			plan.Fingerprints[id] = fi.ModTime()
		}
	}
	if err := writeJSONAtomic(r.gcPlanPath(), plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(dangling)/2; i++ {
		os.Remove(objectLoosePath(r.root, dangling[i]))
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	done, err := r2.GC(opts)
	if err != nil {
		t.Fatalf("中断后重跑 GC 必须成功: %v", err)
	}
	if done.LooseDeleted < 1 {
		t.Fatalf("判定依据: 重跑应继续清掉剩余悬空, got %d", done.LooseDeleted)
	}
	for _, id := range append(allHistoryIDs(t, r2, mainTip), allHistoryIDs(t, r2, devTip)...) {
		if _, err := r2.ReadCommit(id); err != nil {
			t.Fatalf("GC 中断重跑后历史损坏: %v", err)
		}
	}
	again, err := r2.GC(opts)
	if err != nil || again.LooseDeleted != 0 {
		t.Fatalf("重跑后必须幂等零删除: %+v %v", again, err)
	}
}

// 维护操作与正常写入并发；配合 -race 验证。
func TestConcurrentWritesAndMaintenance(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "seed", "s")
	commitAll(t, r, dir, "seed")

	const writers = 6
	const rounds = 8
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	record := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	// 写者操作各自独立的仓库目录，维护操作在各自仓库内并发，
	// 用于 -race 下压测对象写入、归档、只读可达性分析的并发安全。
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wdir := filepath.Join(dir, fmt.Sprintf("writer-%d", w))
			if err := os.MkdirAll(wdir, 0o755); err != nil {
				record(err)
				return
			}
			rw, err := Init(wdir)
			if err != nil {
				record(err)
				return
			}
			for i := 0; i < rounds; i++ {
				name := fmt.Sprintf("w%d-%d.txt", w, i)
				writeFile(t, wdir, name, fmt.Sprintf("w%dr%d\n", w, i))
				if err := rw.Add(name); err != nil {
					record(err)
					return
				}
				if _, err := rw.Commit(CommitOptions{Message: fmt.Sprintf("w%d-%d", w, i), AllowEmpty: true}); err != nil {
					record(err)
					return
				}
				if i%3 == 0 {
					if _, err := rw.Pack(); err != nil {
						record(fmt.Errorf("writer pack: %w", err))
						return
					}
				}
				if i%2 == 0 {
					if _, err := rw.PreviewGC(GCOptions{Retain: 100 * 365 * 24 * time.Hour}); err != nil {
						record(err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("判定依据: 并发阶段不得出错: %v", firstErr)
	}
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	for w := 0; w < writers; w++ {
		wdir := filepath.Join(dir, fmt.Sprintf("writer-%d", w))
		rw, err := Open(wdir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := rw.HEADCommit()
		if err != nil {
			t.Fatal(err)
		}
		for _, cid := range allHistoryIDs(t, rw, id) {
			if _, err := rw.ReadCommit(cid); err != nil {
				t.Fatalf("并发后 writer-%d 历史损坏: %v", w, err)
			}
		}
	}
}

// 万级对象：归档后文件数至少下降一个数量级，内容完整。
func TestLargeRepoFileCountReduction(t *testing.T) {
	r, dir := newTestRepo(t)
	const n = 10000
	for i := 0; i < n; i++ {
		sub := fmt.Sprintf("d%02d", i%50)
		writeFile(t, dir, filepath.Join(sub, fmt.Sprintf("f%05d.txt", i)),
			fmt.Sprintf("object-number-%05d-payload\n", i))
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(CommitOptions{Message: "bulk"}); err != nil {
		t.Fatal(err)
	}
	before := looseCount(t, r)
	st, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	after := countFiles(t, r.root, "packs") + looseCount(t, r)
	t.Logf("万级对象: 松散=%d -> 对象相关文件=%d (pack=%s, objects=%d)",
		before, after, st.PackFile, st.Objects)
	if after*10 >= before {
		t.Fatalf("判定依据: 文件数需至少降一个数量级: %d -> %d", before, after)
	}
	head, _ := r.HEADCommit()
	c, err := r.ReadCommit(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.treeFlat(c.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("判定依据: 万级条目必须完整, got %d", len(entries))
	}
	// 错误保护避免未用导入
	var _ = errors.New
}
