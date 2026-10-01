package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// 索引落盘格式（小端序）：
//
//	magic  = "VCSIDX\x01"（8 字节）
//	record = pathLen u32 | path bytes | blobID 32 字节
//	...记录按 path 字节序升序...
//	checksum = SHA-256(magic || 全部 record)（32 字节）
//
// 校验和保证索引不会被截断/篡改后当作有效状态使用。

const indexMagicV1 = "VCSIDX\x01"

// IndexEntry 是暂存区条目：相对路径（正斜杠）与对应 blob ID。
type IndexEntry struct {
	Path string
	Blob string
}

func encodeIndex(entries []IndexEntry) []byte {
	sorted := append([]IndexEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var body bytes.Buffer
	body.WriteString(indexMagicV1)
	for _, e := range sorted {
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(e.Path)))
		body.Write(l[:])
		body.WriteString(e.Path)
		raw, err := decodeID(e.Blob)
		if err != nil {
			panic(err)
		}
		body.Write(raw[:])
	}
	sum := sha256.Sum256(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// emptyIndex 是零条目索引的规范字节（魔数 + 空记录的校验和）。
func emptyIndex() []byte { return encodeIndex(nil) }

func decodeIndex(data []byte) ([]IndexEntry, error) {
	if len(data) < len(indexMagicV1)+sha256.Size ||
		string(data[:len(indexMagicV1)]) != indexMagicV1 {
		return nil, errors.New("vcs: index corrupt: bad magic")
	}
	content := data[:len(data)-sha256.Size]
	trailer := data[len(data)-sha256.Size:]
	sum := sha256.Sum256(content)
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("vcs: index corrupt: checksum mismatch")
	}

	var entries []IndexEntry
	rest := content[len(indexMagicV1):]
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, errors.New("vcs: index corrupt: truncated")
		}
		plen := int(binary.LittleEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if len(rest) < plen+sha256.Size {
			return nil, errors.New("vcs: index corrupt: truncated")
		}
		path := string(rest[:plen])
		blob := hexEncode(rest[plen : plen+sha256.Size])
		rest = rest[plen+sha256.Size:]
		entries = append(entries, IndexEntry{Path: path, Blob: blob})
	}
	if !sort.SliceIsSorted(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path }) {
		return nil, errors.New("vcs: index corrupt: entries not sorted")
	}
	return entries, nil
}

func (r *Repository) indexPath() string { return filepath.Join(r.root, fileIndex) }

func (r *Repository) readIndex() ([]IndexEntry, error) {
	data, err := os.ReadFile(r.indexPath())
	if err != nil {
		return nil, err
	}
	return decodeIndex(data)
}

// writeIndexAtomic 原子写入索引。
func (r *Repository) writeIndexAtomic(entries []IndexEntry) error {
	return writeFileAtomic(r.indexPath(), encodeIndex(entries), 0o644)
}

// indexAsMap 把索引条目转为 path->blob 映射。
func indexAsMap(entries []IndexEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Path] = e.Blob
	}
	return m
}
