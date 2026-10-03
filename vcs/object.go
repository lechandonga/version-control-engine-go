package vcs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 对象类型。
const (
	TypeBlob   = "blob"
	TypeTree   = "tree"
	TypeCommit = "commit"
)

const objMagic = "vcs1"

// encodeObject 生成松散对象容器：头部声明类型与负载长度，便于识别截断。
func encodeObject(typ string, payload []byte) []byte {
	head := objMagic + " " + typ + " " + strconv.Itoa(len(payload)) + "\n"
	out := make([]byte, 0, len(head)+len(payload))
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

// decodeObject 校验容器并按失败原因分类报错。
func decodeObject(id string, data []byte) (string, []byte, error) {
	nl := -1
	for i, b := range data {
		if b == '\n' {
			nl = i
			break
		}
	}
	if nl < 0 {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "missing header"}
	}
	fields := strings.Split(string(data[:nl]), " ")
	if len(fields) != 3 || fields[0] != objMagic {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad header"}
	}
	typ := fields[1]
	switch typ {
	case TypeBlob, TypeTree, TypeCommit:
	default:
		return "", nil, &ObjectCorrupt{ID: id, Msg: "unknown type " + typ}
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad length in header"}
	}
	payload := data[nl+1:]
	if len(payload) != size {
		return "", nil, &ObjectTruncated{ID: id}
	}
	if hashBytes(payload) != id {
		return "", nil, &ObjectTampered{ID: id}
	}
	return typ, payload, nil
}

func (r *Repo) objectsDir() string { return filepath.Join(r.VCS, "objects") }

func (r *Repo) loosePath(id string) string {
	return filepath.Join(r.objectsDir(), id[:2], id[2:])
}

// WriteObject 写入对象并返回内容地址。写入为临时文件+rename，崩溃不会留下半文件。
func (r *Repo) WriteObject(typ string, payload []byte) (string, error) {
	id := hashBytes(payload)
	p := r.loosePath(id)
	if _, err := os.Stat(p); err == nil {
		return id, nil
	}
	if r.packHas(id) {
		return id, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, encodeObject(typ, payload)); err != nil {
		return "", err
	}
	return id, nil
}

// ReadObject 先查松散对象，再查归档包；按失败原因分类报错。
func (r *Repo) ReadObject(id string) (string, []byte, error) {
	if data, err := os.ReadFile(r.loosePath(id)); err == nil {
		return decodeObject(id, data)
	}
	return r.packRead(id)
}

// HasObject 报告对象是否可读（松散或已归档）。
func (r *Repo) HasObject(id string) bool {
	if _, err := os.Stat(r.loosePath(id)); err == nil {
		return true
	}
	return r.packHas(id)
}

// listLoose 返回全部松散对象 ID。
func (r *Repo) listLoose() ([]string, error) {
	var ids []string
	root := r.objectsDir()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 2 && len(parts[0]) == 2 && isHexID(parts[0]+parts[1]) {
			ids = append(ids, parts[0]+parts[1])
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return ids, err
}

// writeFileAtomic 写临时文件、fsync 后 rename，保证要么旧内容要么新内容。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
