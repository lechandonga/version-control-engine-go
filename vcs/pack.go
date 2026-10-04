package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 归档包落盘格式：
// magic  "VCSPACK1\n"（9 字节）
// count  uint64 BE（条目数）
// entry  { 64 字节十六进制 ID, 2 字节类型长度, 类型, 8 字节负载长度, 负载 }
// footer 32 字节（以上全部内容的 SHA-256）
//
// 包文件名 pack-<内容哈希>.pack，内容哈希由排序后的对象 ID 列表决定，
// 因此同一批内容反复归档得到同一个包名，天然幂等。
const packMagic = "VCSPACK1\n"

type packEntry struct {
	typ    string
	offset int64 // 负载在文件内的偏移
	length int64
}

type packFile struct {
	path    string
	index   map[string]packEntry
	loadErr error  // 解析失败时的分类错误（模板，读时补上对象 ID）
	data    []byte // 首次读取后缓存整包内容
}

func (r *Repo) packsDir() string { return filepath.Join(r.VCS, "packs") }

// loadPacks 扫描 packs 目录并解析索引；结果缓存，归档/回收后失效重载。
func (r *Repo) loadPacks() {
	r.packMu.Lock()
	defer r.packMu.Unlock()
	if r.packsLoaded {
		return
	}
	r.packsLoaded = true
	r.packs = nil
	entries, err := os.ReadDir(r.packsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".pack") {
			continue // 忽略 .tmp 等残留
		}
		r.packs = append(r.packs, parsePack(filepath.Join(r.packsDir(), name)))
	}
}

// getPacks 返回当前已加载的归档包列表（并发安全）。
func (r *Repo) getPacks() []*packFile {
	r.loadPacks()
	r.packMu.Lock()
	defer r.packMu.Unlock()
	return r.packs
}

// parsePack 解析归档包；结构损坏时保留部分索引并记录分类错误。
func parsePack(path string) *packFile {
	pf := &packFile{path: path, index: map[string]packEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		pf.loadErr = &ObjectCorrupt{Msg: "pack unreadable"}
		return pf
	}
	if len(data) < len(packMagic)+8+32 {
		pf.loadErr = &ObjectTruncated{}
		return pf
	}
	if string(data[:len(packMagic)]) != packMagic {
		pf.loadErr = &ObjectCorrupt{Msg: "bad pack magic"}
		return pf
	}
	body := data[:len(data)-32]
	wantSum := data[len(data)-32:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], wantSum) {
		// 内容被篡改：索引仍尽力解析，读取时按篡改报错。
		pf.loadErr = &ObjectTampered{}
	}
	count := binary.BigEndian.Uint64(body[len(packMagic):])
	off := len(packMagic) + 8
	for i := uint64(0); i < count; i++ {
		if off+64 > len(body) {
			pf.loadErr = &ObjectTruncated{}
			return pf
		}
		id := string(body[off : off+64])
		off += 64
		if off+2 > len(body) {
			pf.loadErr = &ObjectTruncated{}
			return pf
		}
		tl := int(binary.BigEndian.Uint16(body[off:]))
		off += 2
		if off+tl+8 > len(body) {
			pf.loadErr = &ObjectTruncated{}
			return pf
		}
		typ := string(body[off : off+tl])
		off += tl
		pl := int(binary.BigEndian.Uint64(body[off:]))
		off += 8
		if off+pl > len(body) {
			pf.loadErr = &ObjectTruncated{}
			return pf
		}
		pf.index[id] = packEntry{typ: typ, offset: int64(off), length: int64(pl)}
		off += pl
	}
	return pf
}

// classifyPackErr 把包级错误模板补上对象 ID。
func classifyPackErr(template error, id string) error {
	switch template.(type) {
	case *ObjectTruncated:
		return &ObjectTruncated{ID: id}
	case *ObjectTampered:
		return &ObjectTampered{ID: id}
	default:
		return &ObjectCorrupt{ID: id, Msg: "pack corrupt"}
	}
}

// packRead 从归档包读取对象。
func (r *Repo) packRead(id string) (string, []byte, error) {
	var packErr error
	for _, pf := range r.getPacks() {
		if pf.loadErr != nil {
			if packErr == nil {
				packErr = classifyPackErr(pf.loadErr, id)
			}
			if _, ok := pf.index[id]; ok {
				return "", nil, classifyPackErr(pf.loadErr, id)
			}
			continue
		}
		ent, ok := pf.index[id]
		if !ok {
			continue
		}
		r.packMu.Lock()
		if pf.data == nil {
			pf.data, _ = os.ReadFile(pf.path)
		}
		data := pf.data
		r.packMu.Unlock()
		if data == nil {
			return "", nil, &ObjectMissing{ID: id}
		}
		if ent.offset+ent.length > int64(len(data)) {
			return "", nil, &ObjectTruncated{ID: id}
		}
		payload := data[ent.offset : ent.offset+ent.length]
		if hashBytes(payload) != id {
			return "", nil, &ObjectTampered{ID: id}
		}
		return ent.typ, payload, nil
	}
	if packErr != nil {
		return "", nil, packErr
	}
	return "", nil, &ObjectMissing{ID: id}
}

func (r *Repo) packHas(id string) bool {
	for _, pf := range r.getPacks() {
		if pf.loadErr == nil {
			if _, ok := pf.index[id]; ok {
				return true
			}
		}
	}
	return false
}

// rewritePacks 重写归档包，剔除 remove 集合中的对象。
// 损坏的包整体不动；健康包通过“写临时文件 → fsync → 原子 rename”替换：
// 新包落盘前旧包仍完整可读，任何时刻崩溃仓库都照常可读，重试结果一致。
// 调用方需持有 r.mu。
func (r *Repo) rewritePacks(remove map[string]bool) error {
	// 清理上次归档/回收崩溃残留的临时包文件。
	if tmps, _ := filepath.Glob(filepath.Join(r.packsDir(), "*.tmp")); len(tmps) > 0 {
		for _, t := range tmps {
			os.Remove(t)
		}
		r.packMu.Lock()
		r.packsLoaded = false
		r.packMu.Unlock()
		r.loadPacks()
	}
	for _, pf := range r.getPacks() {
		if pf.loadErr != nil {
			continue // 损坏包：不丢任何数据，也不阻断其它包回收
		}
		var kept []string
		for id := range pf.index {
			if !remove[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) == len(pf.index) {
			continue // 该包没有可剔除对象
		}
		data, err := os.ReadFile(pf.path)
		if err != nil {
			return err
		}
		sort.Strings(kept)
		var buf bytes.Buffer
		buf.WriteString(packMagic)
		var tmp8 [8]byte
		binary.BigEndian.PutUint64(tmp8[:], uint64(len(kept)))
		buf.Write(tmp8[:])
		for _, id := range kept {
			ent := pf.index[id]
			if ent.offset+ent.length > int64(len(data)) {
				return &ObjectTruncated{ID: id}
			}
			payload := data[ent.offset : ent.offset+ent.length]
			if hashBytes(payload) != id {
				return &ObjectTampered{ID: id}
			}
			buf.WriteString(id)
			var tmp2 [2]byte
			binary.BigEndian.PutUint16(tmp2[:], uint16(len(ent.typ)))
			buf.Write(tmp2[:])
			buf.WriteString(ent.typ)
			binary.BigEndian.PutUint64(tmp8[:], uint64(ent.length))
			buf.Write(tmp8[:])
			buf.Write(payload)
		}
		sum := sha256.Sum256(buf.Bytes())
		buf.Write(sum[:])
		if err := os.MkdirAll(r.packsDir(), 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(r.packsDir(), "pack-*.tmp")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		cleanup := func() { tmp.Close(); os.Remove(tmpName) }
		if _, err := tmp.Write(buf.Bytes()); err != nil {
			cleanup()
			return err
		}
		if err := tmp.Sync(); err != nil {
			cleanup()
			return err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, pf.path); err != nil {
			os.Remove(tmpName)
			return err
		}
		if len(kept) == 0 {
			os.Remove(pf.path) // 整包都是垃圾：替换后删除空包
		}
	}
	// 归档集合发生变化，强制下次重读。
	r.packMu.Lock()
	r.packsLoaded = false
	r.packMu.Unlock()
	r.loadPacks()
	return nil
}

// PackResult 描述一次归档的结果。
type PackResult struct {
	Packed   int      // 本次收拢的对象数
	PackFile string   // 生成的包文件名（已存在则复用）
	Skipped  []string // 因损坏无法归档、保留原样的松散对象
}

// Pack 把松散对象收拢进一个归档包，然后删除已入包的松散文件。
// 崩溃安全：包先写临时文件再 rename；松散文件只在包落盘后才删除。
// 幂等：同一批对象算出的包名相同，已存在时只做松散文件清理。
func (r *Repo) Pack() (*PackResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 清理上次崩溃残留的临时包文件。
	if tmps, _ := filepath.Glob(filepath.Join(r.packsDir(), "*.tmp")); true {
		for _, t := range tmps {
			os.Remove(t)
		}
	}
	ids, err := r.listLoose()
	if err != nil {
		return nil, err
	}
	res := &PackResult{}
	type item struct {
		id      string
		typ     string
		payload []byte
	}
	var items []item
	for _, id := range ids {
		data, err := os.ReadFile(r.loosePath(id))
		if err != nil {
			continue // 与并发回收/写入竞争：对象已消失，跳过
		}
		typ, payload, err := decodeObject(id, data)
		if err != nil {
			res.Skipped = append(res.Skipped, id) // 损坏对象留在原地，错误分类不丢失
			continue
		}
		items = append(items, item{id: id, typ: typ, payload: payload})
	}
	if len(items) == 0 {
		return res, nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	idList := make([]string, len(items))
	for i, it := range items {
		idList[i] = it.id
	}
	name := "pack-" + hashBytes([]byte(strings.Join(idList, "\n")))[:32] + ".pack"
	final := filepath.Join(r.packsDir(), name)
	res.Packed = len(items)
	res.PackFile = name
	if _, err := os.Stat(final); err != nil {
		var buf bytes.Buffer
		buf.WriteString(packMagic)
		var tmp8 [8]byte
		binary.BigEndian.PutUint64(tmp8[:], uint64(len(items)))
		buf.Write(tmp8[:])
		for _, it := range items {
			buf.WriteString(it.id)
			var tmp2 [2]byte
			binary.BigEndian.PutUint16(tmp2[:], uint16(len(it.typ)))
			buf.Write(tmp2[:])
			buf.WriteString(it.typ)
			binary.BigEndian.PutUint64(tmp8[:], uint64(len(it.payload)))
			buf.Write(tmp8[:])
			buf.Write(it.payload)
		}
		sum := sha256.Sum256(buf.Bytes())
		buf.Write(sum[:])
		if err := os.MkdirAll(r.packsDir(), 0o755); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(r.packsDir(), "pack-*.tmp")
		if err != nil {
			return nil, err
		}
		if _, err := tmp.Write(buf.Bytes()); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return nil, err
		}
		if err := os.Rename(tmp.Name(), final); err != nil {
			os.Remove(tmp.Name())
			return nil, err
		}
	}
	// 包已落盘：刷新缓存后再删除松散文件，保证任何时刻对象都可读。
	r.packMu.Lock()
	r.packsLoaded = false
	r.packMu.Unlock()
	r.loadPacks()
	for _, it := range items {
		os.Remove(r.loosePath(it.id))
	}
	// 清理空的分片目录。
	if subs, _ := os.ReadDir(r.objectsDir()); true {
		for _, s := range subs {
			if s.IsDir() {
				os.Remove(filepath.Join(r.objectsDir(), s.Name()))
			}
		}
	}
	return res, nil
}
