package vcs

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
)

// rewritePacks 重写所有包含垃圾对象的归档：保留对象读出后按新集合
// 生成新归档，pack/idx 临时文件 + rename 原子发布，最后删除旧归档。
// 不含垃圾对象的归档原样保留。返回被重写的归档数。
func (r *Repo) rewritePacks(drop map[string]bool) (int, error) {
	r.packsMu.RLock()
	packs := append([]*packFile(nil), r.packs...)
	r.packsMu.RUnlock()

	rewritten := 0
	for _, p := range packs {
		var keepIDs []string
		for id := range p.idx {
			if !drop[id] {
				keepIDs = append(keepIDs, id)
			}
		}
		sort.Strings(keepIDs)
		if len(keepIDs) == len(p.idx) {
			continue // 无垃圾
		}

		var body bytes.Buffer
		var idx bytes.Buffer
		idx.WriteString("VCSIDX1\n")
		for _, id := range keepIDs {
			rec := p.idx[id]
			raw, err := p.readRecord(rec)
			if err != nil {
				return rewritten, err
			}
			if _, _, err := parseObject(id, raw); err != nil {
				return rewritten, err
			}
			off := body.Len()
			body.Write(raw)
			idxWrite(&idx, id, off, len(raw))
		}

		packDir := filepath.Join(r.root, "packs")
		if len(keepIDs) == 0 {
			// 归档整体不可达：直接删除 pack 与 idx（旧归档的对象均在 drop 中）。
			if err := removeIfExists(p.path); err != nil {
				return rewritten, err
			}
			if err := removeIfExists(filepath.Join(packDir, p.name+".idx")); err != nil {
				return rewritten, err
			}
			rewritten++
			continue
		}

		setHash := objectSetHash(keepIDs)
		newName := "pack-" + setHash
		newPackPath := filepath.Join(packDir, newName+".pack")
		if r.packByName(newName) == nil {
			if _, err := os.Stat(newPackPath); os.IsNotExist(err) {
				var full bytes.Buffer
				full.Write(body.Bytes())
				full.WriteString(packSumMarker)
				full.WriteString(hashBytes(body.Bytes()))
				full.WriteByte('\n')
				tmpPack := filepath.Join(packDir, ".tmp-gcpack-"+setHash)
				tmpIdx := filepath.Join(packDir, ".tmp-gcidx-"+setHash)
				if err := writeFileAtomic(tmpPack, full.Bytes(), 0o444); err != nil {
					return rewritten, err
				}
				if err := writeFileAtomic(tmpIdx, idx.Bytes(), 0o444); err != nil {
					return rewritten, err
				}
				if err := os.Rename(tmpPack, newPackPath); err != nil {
					return rewritten, err
				}
				if err := os.Rename(tmpIdx, filepath.Join(packDir, newName+".idx")); err != nil {
					return rewritten, err
				}
			}
		}
		// 旧归档中可能还有被保留对象的唯一副本——新归档发布后才安全删除。
		if err := r.refreshPacks(); err != nil {
			return rewritten, err
		}
		newPF := r.packByName(newName)
		keptElsewhere := true
		for _, id := range keepIDs {
			// 新归档必须覆盖全部保留对象，否则保守保留旧归档。
			if newPF == nil {
				keptElsewhere = false
				break
			}
			if _, ok := newPF.indexOf(id); !ok {
				if !r.hasObjectLoose(id) {
					keptElsewhere = false
					break
				}
			}
		}
		if !keptElsewhere {
			return rewritten, &ObjectCorrupt{Msg: "gc: new pack missing kept objects, aborting rewrite of " + p.name}
		}
		if err := removeIfExists(p.path); err != nil {
			return rewritten, err
		}
		if err := removeIfExists(filepath.Join(packDir, p.name+".idx")); err != nil {
			return rewritten, err
		}
		if err := r.refreshPacks(); err != nil {
			return rewritten, err
		}
		rewritten++
	}
	return rewritten, nil
}

func (r *Repo) hasObjectLoose(id string) bool {
	if !isHexID(id) {
		return false
	}
	_, err := os.Stat(objectLoosePath(r.root, id))
	return err == nil
}

func idxWrite(idx *bytes.Buffer, id string, off, length int) {
	idx.WriteString(id)
	idx.WriteByte(' ')
	idx.WriteString(itoa(off))
	idx.WriteByte(' ')
	idx.WriteString(itoa(length))
	idx.WriteByte('\n')
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
