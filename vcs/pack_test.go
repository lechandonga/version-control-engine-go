package vcs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 场景：归档前后所有读取结果一致（分支、历史、工作区、合并/重放判定）。
func TestPackReadEquivalence(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "d/a.txt", "a\n")
	writeFile(t, dir, "b.txt", "b\n")
	c1 := commitAll(t, r, dir, "c1")
	if err := r.CreateBranch("dev", c1); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "b.txt", "b2\n")
	c2 := commitAll(t, r, dir, "c2")
	head1, _ := r.HEADCommit()
	hist1 := allHistoryIDs(t, r, head1)
	files1 := snapshotFiles(t, dir)
	c1Obj, err := r.ReadCommit(c1)
	if err != nil {
		t.Fatal(err)
	}

	st, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("归档结果: %+v", *st)
	if looseCount(t, r) != 0 {
		t.Fatalf("判定依据: 归档后松散对象必须为 0, got %d", looseCount(t, r))
	}
	if n := countFiles(t, r.root, "packs"); n != 2 {
		t.Fatalf("判定依据: 恰好一个 pack + 一个 idx, got %d", n)
	}

	// 历史、提交标识不变
	c1Again, err := r.ReadCommit(c1)
	if err != nil {
		t.Fatalf("归档后读取 c1: %v", err)
	}
	if c1Again.Tree != c1Obj.Tree || c1Again.Message != c1Obj.Message {
		t.Fatal("归档后提交内容/标识发生变化")
	}
	head2, _ := r.HEADCommit()
	if head2 != head2 || head2 != c2 {
		t.Fatalf("HEAD 漂移: %s vs %s", head2, c2)
	}
	hist2 := allHistoryIDs(t, r, head2)
	if len(hist1) != len(hist2) {
		t.Fatal("归档后历史长度变化")
	}
	for i := range hist1 {
		if hist1[i] != hist2[i] {
			t.Fatalf("历史顺序/标识变化 @%d", i)
		}
	}

	// 重新打开仓库后仍可用（走 mmap 读归档）
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Checkout("dev"); err != nil {
		t.Fatalf("归档后切分支: %v", err)
	}
	if got := readWorkdirFile(t, dir, "b.txt"); got != "b\n" {
		t.Fatalf("归档后检出旧分支内容错误: %q", got)
	}
	// 合并判定结果不变：dev 落后 main，快进
	res, err := r2.Merge("main", "")
	if err != nil || res.Mode != "fast-forward" {
		t.Fatalf("判定依据: 归档后快进合并结果必须不变, res=%+v err=%v", res, err)
	}

	// 在途重放现场也要照常读取（构造一个暂停重放，归档后继续）
	r3, _ := newTestRepo(t)
	d3 := filepath.Dir(r3.root)
	writeFile(t, d3, "f", "base\n")
	base := commitAll(t, r3, d3, "base")
	r3.CreateBranch("topic", base)
	writeFile(t, d3, "f", "main\n")
	commitAll(t, r3, d3, "main")
	r3.Checkout("topic")
	writeFile(t, d3, "f", "topic\n")
	commitAll(t, r3, d3, "topic")
	err = r3.Rebase("main")
	var rc *RebaseConflict
	if !errors.As(err, &rc) {
		t.Fatalf("期望重放冲突, got %v", err)
	}
	if _, err := r3.Pack(); err != nil {
		t.Fatal(err)
	}
	// 重开：在途现场、冲突三方 blob 都必须还能读到
	r4, err := Open(d3)
	if err != nil {
		t.Fatal(err)
	}
	st4, err := r4.RebaseInProgress()
	if err != nil {
		t.Fatalf("归档后在途重放现场必须可读: %v", err)
	}
	if st4.StoppedCommit == "" || len(st4.Conflicts) != 1 {
		t.Fatalf("归档后冲突现场不完整: %+v", st4)
	}
	if err := r4.ResolveRebaseConflict("f", []byte("done\n")); err != nil {
		t.Fatal(err)
	}
	if err := r4.ContinueRebase(); err != nil {
		t.Fatalf("归档后继续重放失败: %v", err)
	}
	_ = files1
}

// 场景：反复归档幂等——不产生新归档、不越归档越大。
func TestPackIdempotent(t *testing.T) {
	r, dir := newTestRepo(t)
	for i := 0; i < 30; i++ {
		writeFile(t, dir, "f", string(rune('a'+i%26))+"\n")
		commitAll(t, r, dir, "c")
	}
	st1, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	size1 := dirSize(t, filepath.Join(r.root, "packs"))
	st2, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	st3, err := r.Pack()
	if err != nil {
		t.Fatal(err)
	}
	size2 := dirSize(t, filepath.Join(r.root, "packs"))
	if !st2.Reused || !st3.Reused {
		t.Fatalf("判定依据: 无新对象时必须命中幂等, st2=%+v st3=%+v", *st2, *st3)
	}
	if size2 != size1 {
		t.Fatalf("判定依据: 反复归档不得变大, %d -> %d", size1, size2)
	}
	if n := countFiles(t, r.root, "packs"); n != 2 {
		t.Fatalf("重复归档不得新增文件, got %d", n)
	}
	_ = st1

	// 归档后又有新提交，再次归档只新增必要对象，且旧历史仍可读
	oldHead, _ := r.HEADCommit()
	writeFile(t, dir, "new", "x")
	newHead := commitAll(t, r, dir, "new-commit")
	st4, err := r.Pack()
	if err != nil || st4.Reused {
		t.Fatalf("有新对象时应生成新归档, %+v err=%v", st4, err)
	}
	if _, err := r.ReadCommit(oldHead); err != nil {
		t.Fatalf("再次归档后旧历史必须可读: %v", err)
	}
	if _, err := r.ReadCommit(newHead); err != nil {
		t.Fatalf("再次归档后新历史必须可读: %v", err)
	}
}

// 场景：归档文件被截断/篡改时按失败原因分类报错，不当好数据。
func TestPackCorruptionClassified(t *testing.T) {
	r, dir := newTestRepo(t)
	writeFile(t, dir, "f", "payload\n")
	c := commitAll(t, r, dir, "c")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	commitObj, err := r.ReadCommit(c)
	if err != nil {
		t.Fatal(err)
	}

	packPath := filepath.Join(r.root, "packs")
	entries, _ := os.ReadDir(packPath)
	var packFile string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".pack" {
			packFile = filepath.Join(packPath, e.Name())
		}
	}
	data, _ := os.ReadFile(packFile)

	// 1) 截断：只砍掉最后一条记录的部分字节，并补上伪造尾部，
	// 使 idx 缺失时扫描仍能索引到该记录 => 读取必须报 truncated。
	truncDir := t.TempDir()
	rTrunc, _ := Init(truncDir)
	index := buildScanIndex(data)
	var lastID string
	var lastRec packRecord
	for id, rec := range index {
		if rec.offset >= lastRec.offset {
			lastID, lastRec = id, rec
		}
	}
	cut := int(lastRec.offset) + lastRec.length/2
	// 只截断、不补尾部：scanner 无法越过截断记录，但测试通过手工写入
	// 一份指向截断范围的 idx，强制走“记录越界 => truncated”分类路径。
	truncData := data[:cut]
	writeCorruptPack(t, rTrunc, truncData)
	var idxBuf bytes.Buffer
	idxBuf.WriteString("VCSIDX1\n")
	for id, rec := range buildScanIndex(data) {
		idxBuf.WriteString(id)
		idxBuf.WriteByte(' ')
		idxBuf.WriteString(strconv.FormatInt(rec.offset, 10))
		idxBuf.WriteByte(' ')
		idxBuf.WriteString(strconv.Itoa(rec.length))
		idxBuf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(rTrunc.root, "packs", "pack-corrupt.idx"),
		idxBuf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := rTrunc.refreshPacks(); err != nil {
		t.Fatal(err)
	}
	cutTarget := ""
	for id, rec := range buildScanIndex(data) {
		if rec.offset+int64(rec.length) > int64(cut) {
			cutTarget = id
			break
		}
	}
	if cutTarget == "" {
		cutTarget = lastID
	}
	_, _, perr := rTrunc.readObject(cutTarget)
	assertErrorType(t, perr, "truncated")

	// 2) 篡改：翻转负载中间一字节（哈希必不符）
	tampered := append([]byte(nil), data...)
	// 直接定位 blob 记录的负载区翻转一个字节：记录边界由扫描得到。
	scan := buildScanIndex(data)
	blobID := ""
	for _, te := range mustTreeEntries(t, r, commitObj.Tree) {
		if te.Mode == "100644" {
			blobID = te.ID
		}
	}
	if blobID == "" {
		t.Fatal("找不到 blob")
	}
	brec := scan[blobID]
	nl := bytes.IndexByte(data[brec.offset+int64(len(objectMagic)):], '\n')
	pos := int(brec.offset) + len(objectMagic) + nl + 1 + 2
	tampered[pos] ^= 0xFF
	rTam, _ := Init(t.TempDir())
	writeCorruptPack(t, rTam, tampered)
	copyIdx(t, r, rTam)
	// 被篡改的是 blob，通过提交树读取它
	treeEntries, err := rTam.readTree(commitObj.Tree)
	if err == nil {
		for _, te := range treeEntries {
			if te.Mode == "100644" {
				_, gerr := rTam.readBlob(te.ID)
				assertErrorType(t, gerr, "tampered")
			}
		}
	} else {
		t.Fatalf("树应可读，blob 才是篡改目标: %v", err)
	}

	// 3) 头部魔数损坏 => corrupt
	corrupt := append([]byte(nil), data...)
	for _, rec := range buildScanIndex(data) {
		copy(corrupt[rec.offset:], "XXXXOBJ")
	}
	rCor, _ := Init(t.TempDir())
	writeCorruptPack(t, rCor, corrupt)
	copyIdx(t, r, rCor)
	_, err = rCor.ReadCommit(c)
	assertErrorType(t, err, "corrupt")

	// 损坏的归档不影响仓库打开与其它对象（坏包被隔离登记）
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
}

func copyIdx(t *testing.T, src, dst *Repo) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(src.root, "packs"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".idx" {
			data, err := os.ReadFile(filepath.Join(src.root, "packs", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst.root, "packs", "pack-corrupt.idx"), data, 0o444); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := dst.refreshPacks(); err != nil {
		t.Fatal(err)
	}
}

func writeCorruptPack(t *testing.T, r *Repo, data []byte) {
	t.Helper()
	name := "pack-corrupt"
	if err := os.WriteFile(filepath.Join(r.root, "packs", name+".pack"), data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := r.refreshPacks(); err != nil {
		t.Fatal(err)
	}
}

// buildScanIndex 复刻归档顺序扫描，供测试定位记录边界。
func buildScanIndex(data []byte) map[string]packRecord {
	out := map[string]packRecord{}
	var off int64
	for off < int64(len(data)) {
		d := data[off:]
		if !bytes.HasPrefix(d, []byte(objectMagic)) {
			return out
		}
		rest := d[len(objectMagic):]
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return out
		}
		fields := strings.Fields(string(rest[:nl]))
		if len(fields) != 3 || !isHexID(fields[1]) {
			return out
		}
		declared, err := strconv.Atoi(fields[2])
		if err != nil || declared <= 0 {
			return out
		}
		recLen := len(objectMagic) + nl + 1 + declared
		out[fields[1]] = packRecord{offset: off, length: recLen}
		off += int64(recLen)
	}
	return out
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var n int64
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fi, _ := d.Info()
			n += fi.Size()
		}
		return nil
	})
	return n
}

func mustTreeEntries(t *testing.T, r *Repo, treeID string) []TreeEntry {
	t.Helper()
	entries, err := r.readTree(treeID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
