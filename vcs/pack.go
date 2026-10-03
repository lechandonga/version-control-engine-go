package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 归档把零散的对象容器顺序拼接进一个 .pack 文件：
//
//	<record1: VCSOBJ1... 容器><record2>...<recordN>
//	VCSSUM256 <sha256(以上全部字节)>\n
//
// 每条 record 与松散对象物理格式完全一致，因此截断/篡改的归类规则
// 与松散对象一致：读不到整段 => ObjectTruncated；负载哈希不符 =>
// ObjectTampered；头部非法 => ObjectCorrupt。
//
// 同名 .idx 为加速索引（可缺失，缺失时顺序扫描重建，不影响正确性）：
//
//	VCSIDX1\n
//	<id> <offset> <length>\n ...
//
// 归档文件名中的 <id> = sha256(对象 id 排序后拼接)，对象集合相同则
// 文件名相同，反复归档幂等。

const packSumMarker = "VCSSUM256 "

type packRecord struct {
	offset int64
	length int
}

type packFile struct {
	name string
	path string
	data []byte // mmap

	idx     map[string]packRecord
	bodyEnd int64 // 校验和标记之前的字节数；0 表示无有效尾部
	sumOK   bool
	openErr string
}

func (p *packFile) indexOf(id string) (packRecord, bool) {
	rec, ok := p.idx[id]
	return rec, ok
}

// readRecord 切出一条对象容器；范围超出归档 => 归档被截断。
func (p *packFile) readRecord(rec packRecord) ([]byte, error) {
	end := rec.offset + int64(rec.length)
	if rec.offset < 0 || end > int64(len(p.data)) {
		return nil, &ObjectTruncated{ID: ""}
	}
	return p.data[rec.offset:end], nil
}

// PackStats 汇报一次归档结果。
type PackStats struct {
	PackFile   string
	Objects    int
	LooseFiles int
	Reused     bool // 命中已有同内容归档，未生成新文件
}

// Pack 把当前全部松散对象收拢进归档。可与正常写入并发：归档期间新写入
// 的对象保持松散，留待下次归档。
func (r *Repo) Pack() (*PackStats, error) {
	lock := newFileLock(r.root, "pack.lock")
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	if err := r.refreshPacks(); err != nil {
		return nil, err
	}
	loose, err := r.listLooseObjects()
	if err != nil {
		return nil, err
	}
	if len(loose) == 0 {
		return &PackStats{Reused: true}, nil
	}

	type obj struct {
		id   string
		data []byte
	}
	objs := make([]obj, 0, len(loose))
	ids := make([]string, 0, len(loose))
	for _, id := range loose {
		data, err := os.ReadFile(objectLoosePath(r.root, id))
		if err != nil {
			return nil, err
		}
		if _, _, perr := parseObject(id, data); perr != nil {
			return nil, perr
		}
		objs = append(objs, obj{id: id, data: data})
		ids = append(ids, id)
	}

	setHash := objectSetHash(ids)
	packName := "pack-" + setHash
	packPath := filepath.Join(r.root, "packs", packName+".pack")
	if pf := r.packByName(packName); pf != nil {
		// 同集合归档已存在（含崩溃前刚发布的情形）：幂等收尾，
		// 只删除归档中确实存在的松散副本。
		if err := r.deleteLooseContainedIn(pf, ids); err != nil {
			return nil, err
		}
		n, _ := countLoose(r.root)
		return &PackStats{PackFile: packName + ".pack", Objects: len(ids), LooseFiles: n, Reused: true}, nil
	}

	sort.Slice(objs, func(i, j int) bool { return objs[i].id < objs[j].id })
	var body bytes.Buffer
	var idx bytes.Buffer
	idx.WriteString("VCSIDX1\n")
	for _, o := range objs {
		off := body.Len()
		body.Write(o.data)
		fmt.Fprintf(&idx, "%s %d %d\n", o.id, off, len(o.data))
	}
	sum := hashBytes(body.Bytes())
	var full bytes.Buffer
	full.Write(body.Bytes())
	full.WriteString(packSumMarker)
	full.WriteString(sum)
	full.WriteByte('\n')

	packDir := filepath.Join(r.root, "packs")
	tmpPack := filepath.Join(packDir, ".tmp-pack-"+setHash)
	tmpIdx := filepath.Join(packDir, ".tmp-idx-"+setHash)
	if err := writeFileAtomic(tmpPack, full.Bytes(), 0o444); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(tmpIdx, idx.Bytes(), 0o444); err != nil {
		return nil, err
	}
	// 先发布 pack 再发布 idx；读者缺 idx 会顺序扫描，结果一致。
	if err := os.Rename(tmpPack, packPath); err != nil {
		return nil, err
	}
	idxPath := filepath.Join(packDir, packName+".idx")
	if err := os.Rename(tmpIdx, idxPath); err != nil {
		return nil, err
	}

	if err := r.refreshPacks(); err != nil {
		return nil, err
	}
	pf := r.packByName(packName)
	if pf == nil {
		return nil, &ObjectCorrupt{Msg: "pack vanished after publish: " + packName}
	}
	if err := r.deleteLooseContainedIn(pf, ids); err != nil {
		return nil, err
	}
	n, _ := countLoose(r.root)
	return &PackStats{PackFile: packName + ".pack", Objects: len(ids), LooseFiles: n}, nil
}

func objectSetHash(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return hashBytes([]byte(strings.Join(sorted, "")))
}

func (r *Repo) deleteLooseContainedIn(pf *packFile, ids []string) error {
	for _, id := range ids {
		if _, ok := pf.indexOf(id); !ok {
			continue
		}
		if err := os.Remove(objectLoosePath(r.root, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return r.removeEmptyObjectDirs()
}

func (r *Repo) removeEmptyObjectDirs() error {
	objRoot := filepath.Join(r.root, "objects")
	dirs, err := os.ReadDir(objRoot)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		p := filepath.Join(objRoot, d.Name())
		children, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		if len(children) == 0 {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (r *Repo) listLooseObjects() ([]string, error) {
	objRoot := filepath.Join(r.root, "objects")
	var ids []string
	entries, err := os.ReadDir(objRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, d := range entries {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		children, err := os.ReadDir(filepath.Join(objRoot, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			id := d.Name() + c.Name()
			if isHexID(id) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func countLoose(root string) (int, error) {
	n := 0
	objRoot := filepath.Join(root, "objects")
	err := filepath.WalkDir(objRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n, err
}

func (r *Repo) packByName(name string) *packFile {
	r.packsMu.RLock()
	defer r.packsMu.RUnlock()
	for _, p := range r.packs {
		if p.name == name {
			return p
		}
	}
	return nil
}
