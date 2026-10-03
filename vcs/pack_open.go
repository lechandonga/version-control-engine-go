package vcs

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// refreshPacks 重新扫描 packs 目录并建立内存索引。正式 .pack 损坏时
// 不阻断仓库打开：坏包登记为空索引（按缺失处理），读取其中对象会
// 在 verify 阶段得到明确的分类错误，而不是把坏数据当好数据。
func (r *Repo) refreshPacks() error {
	dir := filepath.Join(r.root, "packs")
	// 清理崩溃遗留临时文件需要与正在进行的 Pack 互斥；锁拿不到时
	// （有归档正在写）跳过清理，临时文件留给下一轮。
	cleanLock := newFileLock(r.root, "pack.lock")
	if err := cleanLock.TryLock(); err == nil {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".tmp-") {
					_ = os.Remove(filepath.Join(dir, e.Name()))
				}
			}
		}
		_ = cleanLock.Unlock()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var packs []*packFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pack") {
			continue
		}
		pf, err := openPack(filepath.Join(dir, e.Name()))
		if err != nil {
			// 打开失败：登记为不可用包（携带 idx 线索），仓库其余功能照常；
			// 访问其中对象时报 ObjectCorrupt。
			var bpe badPackError
			if errors.As(err, &bpe) {
				pf = bpe.pf
			} else {
				pf = &packFile{
					name: strings.TrimSuffix(e.Name(), ".pack"),
					path: filepath.Join(dir, e.Name()),
					idx:  map[string]packRecord{},
				}
				pf.openErr = err.Error()
			}
		}
		packs = append(packs, pf)
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].name < packs[j].name })
	r.packsMu.Lock()
	old := r.packs
	r.packs = packs
	r.packsMu.Unlock()
	for _, p := range old {
		p.unmap()
	}
	return nil
}

func openPack(path string) (pf *packFile, err error) {
	name := strings.TrimSuffix(filepath.Base(path), ".pack")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, errors.New("empty pack file")
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	pf = &packFile{name: name, path: path, data: data, idx: map[string]packRecord{}}
	defer func() {
		if err != nil {
			// 调用方需要 pf（带 openErr）做“坏包隔离登记”，保留映射。
			pf.openErr = err.Error()
			err = badPackError{pf: pf}
		}
	}()
	pf.parseTrailer()
	// 即使归档体魔数损坏，也尽量从同名 idx 恢复“哪些对象曾在此包中”，
	// 使读请求能得到 ObjectCorrupt 而非 ObjectMissing。
	if err := pf.loadIndex(); err != nil {
		pf.scanRecords() // 索引缺失/损坏：顺序扫描重建
	}
	if !bytes.HasPrefix(data, []byte(objectMagic)) {
		return pf, errors.New("bad pack magic")
	}
	return pf, nil
}

type badPackError struct{ pf *packFile }

func (badPackError) Error() string { return "bad pack" }

func (p *packFile) probablyContains(id string) bool {
	if _, ok := p.idx[id]; ok {
		return true
	}
	// 无 idx 可用时无法确定，交给 Missing 分类（不臆测）。
	return false
}

func (p *packFile) parseTrailer() {
	marker := []byte("\n" + packSumMarker)
	at := bytes.LastIndex(p.data, marker)
	if at < 0 {
		return
	}
	rest := p.data[at+1:]
	lineEnd := bytes.IndexByte(rest, '\n')
	if lineEnd < 0 || at+1+lineEnd != len(p.data)-1 {
		return // 校验和不是最后一行，结构非法，不采信
	}
	bodyEnd := int64(at + 1)
	sumField := strings.TrimSpace(string(rest[len(packSumMarker):lineEnd]))
	p.bodyEnd = bodyEnd
	p.sumOK = isHexID(sumField) && sumField == hashBytes(p.data[:bodyEnd])
}

func (p *packFile) loadIndex() error {
	idxData, err := os.ReadFile(strings.TrimSuffix(p.path, ".pack") + ".idx")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(idxData, []byte("VCSIDX1\n")) {
		return errors.New("bad idx magic")
	}
	sc := bufio.NewScanner(bytes.NewReader(idxData[len("VCSIDX1\n"):]))
	sc.Buffer(make([]byte, 0, 4096), 64*1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || !isHexID(f[0]) {
			return errors.New("bad idx line: " + sc.Text())
		}
		off, err1 := strconv.ParseInt(f[1], 10, 64)
		length, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || off < 0 || length <= 0 {
			return errors.New("bad idx numbers")
		}
		p.idx[f[0]] = packRecord{offset: off, length: length}
	}
	if len(p.idx) == 0 {
		return errors.New("empty idx")
	}
	return sc.Err()
}

// scanRecords 顺序扫描对象容器头建立索引；遇到第一个非法/截断记录即停。
func (p *packFile) scanRecords() {
	p.idx = map[string]packRecord{}
	limit := int64(len(p.data))
	if p.bodyEnd > 0 {
		limit = p.bodyEnd
	}
	var off int64
	for off < limit {
		data := p.data[off:]
		if !bytes.HasPrefix(data, []byte(objectMagic)) {
			return
		}
		rest := data[len(objectMagic):]
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return
		}
		fields := strings.Fields(string(rest[:nl]))
		if len(fields) != 3 || !isHexID(fields[1]) {
			return
		}
		declared, err := strconv.Atoi(fields[2])
		if err != nil || declared <= 0 {
			return
		}
		recLen := len(objectMagic) + nl + 1 + declared
		if int64(recLen) > limit-off {
			return // 记录被截断
		}
		p.idx[fields[1]] = packRecord{offset: off, length: recLen}
		off += int64(recLen)
	}
}

func (p *packFile) unmap() {
	if p.data != nil {
		_ = syscall.Munmap(p.data)
		p.data = nil
	}
}
