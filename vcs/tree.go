package vcs

import (
	"bufio"
	"bytes"
	"sort"
	"strings"
)

// TreeEntry 为树中的一项（文件或子目录）。
type TreeEntry struct {
	Name string
	// Mode 目前只用 "100644"（文件）与 "40000"（目录）。
	Mode string
	ID   string
}

// tree 负载为规范化的文本行（按 Name 排序）：
//
//	<mode> <name>\n<blob-or-tree-id>\n
//
// 选文本格式是为了归档截断 / 篡改时错误定位直观。

func encodeTree(entries []TreeEntry) ([]byte, error) {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, e := range sorted {
		if e.Name == "" || strings.ContainsAny(e.Name, "\n/") || e.Name == "." || e.Name == ".." {
			return nil, &ObjectCorrupt{Msg: "bad tree entry name: " + e.Name}
		}
		if (e.Mode != "100644" && e.Mode != "40000") || !isHexID(e.ID) {
			return nil, &ObjectCorrupt{ID: e.ID, Msg: "bad tree entry"}
		}
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte('\n')
		b.WriteString(e.ID)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

func parseTree(id string, payload []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	sc := bufio.NewScanner(bytes.NewReader(payload))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		meta := sc.Text()
		if !sc.Scan() {
			return nil, &ObjectCorrupt{ID: id, Msg: "tree entry missing id line"}
		}
		sp := strings.IndexByte(meta, ' ')
		if sp <= 0 {
			return nil, &ObjectCorrupt{ID: id, Msg: "bad tree entry header"}
		}
		mode, name := meta[:sp], meta[sp+1:]
		entryID := sc.Text()
		if (mode != "100644" && mode != "40000") || name == "" || !isHexID(entryID) {
			return nil, &ObjectCorrupt{ID: id, Msg: "bad tree entry"}
		}
		out = append(out, TreeEntry{Name: name, Mode: mode, ID: entryID})
	}
	if err := sc.Err(); err != nil {
		return nil, &ObjectCorrupt{ID: id, Msg: err.Error()}
	}
	return out, nil
}

// writeTree 写入树对象，返回对象 ID。
func (r *Repo) writeTree(entries []TreeEntry) (string, error) {
	payload, err := encodeTree(entries)
	if err != nil {
		return "", err
	}
	return r.putObject(ObjectTree, payload)
}

// readTree 读取并解析树对象；非树对象报 Corrupt。
func (r *Repo) readTree(id string) ([]TreeEntry, error) {
	t, payload, err := r.readObject(id)
	if err != nil {
		return nil, err
	}
	if t != ObjectTree {
		return nil, &ObjectCorrupt{ID: id, Msg: "expected tree, got " + string(t)}
	}
	return parseTree(id, payload)
}

func treeMap(entries []TreeEntry) map[string]TreeEntry {
	m := make(map[string]TreeEntry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m
}
