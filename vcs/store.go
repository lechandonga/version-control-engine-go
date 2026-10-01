package vcs

import (
	"os"
	"path/filepath"
)

// objectStore 是内容寻址对象库。
//
// 落盘布局：<root>/objects/ab/cdef...（前 2 位十六进制做分桶）。
// 写入是 “临时文件 + rename”，且先写满后再改名，因此：
//   - 相同内容重复写入只占一份（路径相同，rename 覆盖即等同）；
//   - 读方永远不会观察到写了一半的对象文件；
//   - 中断后可能残留 .tmp-* 临时文件，读取路径完全忽略它们。
type objectStore struct {
	dir string
}

func newObjectStore(root string) *objectStore {
	return &objectStore{dir: filepath.Join(root, "objects")}
}

func (s *objectStore) objectPath(id string) string {
	return filepath.Join(s.dir, id[:2], id[2:])
}

// write 编码并写入对象；已存在则直接复用（去重）。
func (s *objectStore) write(objType string, body []byte) (string, error) {
	raw := encodeObject(objType, body)
	sum := raw[len(raw)-32:]
	id := hexEncode(sum)
	path := s.objectPath(id)
	if fileExists(path) {
		return id, nil
	}
	if err := writeFileAtomic(path, raw, 0o444); err != nil {
		return "", err
	}
	return id, nil
}

// read 读取对象并执行全部完整性检查（见 parseObject）。
func (s *objectStore) read(id string) (*parsedObject, error) {
	if len(id) != IDLen {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectNotFound}
	}
	for _, c := range id {
		if !isHex(c) {
			return nil, &IntegrityError{ID: id, Kind: ErrObjectNotFound}
		}
	}
	path := s.objectPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &IntegrityError{ID: id, Kind: ErrObjectNotFound}
		}
		return nil, err
	}
	obj, perr := parseObject(id, raw)
	if perr != nil {
		if ie, ok := perr.(*IntegrityError); ok {
			ie.Path = path
		}
		return nil, perr
	}
	return obj, nil
}

func hexEncode(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0xf]
	}
	return string(out)
}

func isHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// objectIter 返回对象库中全部（按 ID 排序的）对象原始字节，临时文件被忽略。
type objectIter struct {
	ids   []string
	pos   int
	store *objectStore
}

func (s *objectStore) iter() (*objectIter, error) {
	var ids []string
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &objectIter{store: s}, nil
		}
		return nil, err
	}
	for _, bucket := range entries {
		if len(bucket.Name()) != 2 || !allHex(bucket.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, bucket.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			name := f.Name()
			if len(name) != IDLen-2 || !allHex(name) {
				continue
			}
			ids = append(ids, bucket.Name()+name)
		}
	}
	return &objectIter{ids: ids, store: s}, nil
}

func allHex(s string) bool {
	for _, c := range s {
		if !isHex(c) {
			return false
		}
	}
	return true
}
