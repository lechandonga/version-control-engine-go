package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 对象容器（松散对象与归档内记录共用同一物理格式）：
//
//	VCSOBJ1\n
//	<type> <hexid> <declaredLen>\n
//	<payload, declaredLen 字节>
//
//	id 恒为 payload 的 SHA-256。读取时按头部非法 / 长度不符 / 哈希不符 /
// 负载结构非法分别归类为 Corrupt / Truncated / Tampered / Corrupt。

const objectMagic = "VCSOBJ1\n"

// ObjectType 为对象负载类型。
type ObjectType string

const (
	ObjectBlob   ObjectType = "blob"
	ObjectTree   ObjectType = "tree"
	ObjectCommit ObjectType = "commit"
)

func validObjectType(t ObjectType) bool {
	return t == ObjectBlob || t == ObjectTree || t == ObjectCommit
}

func objectLoosePath(root, id string) string {
	return filepath.Join(root, "objects", id[:2], id[2:])
}

// encodeObject 生成对象容器字节。
func encodeObject(t ObjectType, payload []byte) (id string, data []byte) {
	id = hashBytes(payload)
	header := fmt.Sprintf("%s%s %s %d\n", objectMagic, t, id, len(payload))
	return id, append([]byte(header), payload...)
}

// parseObject 校验并解析对象容器，返回负载；失败按四类原因报错。
func parseObject(id string, data []byte) (ObjectType, []byte, error) {
	if len(data) < len(objectMagic) || string(data[:len(objectMagic)]) != objectMagic {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad magic"}
	}
	rest := data[len(objectMagic):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "unterminated header"}
	}
	fields := strings.Fields(string(rest[:nl]))
	if len(fields) != 3 {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad header fields"}
	}
	t := ObjectType(fields[0])
	if !validObjectType(t) {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "unknown type " + fields[0]}
	}
	declaredID := fields[1]
	if !isHexID(declaredID) {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad header id"}
	}
	declaredLen, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || declaredLen < 0 {
		return "", nil, &ObjectCorrupt{ID: id, Msg: "bad declared length"}
	}
	payload := rest[nl+1:]
	if int64(len(payload)) != declaredLen {
		// 容器长度与声明不符：典型的写入中断 / 文件截断。
		return "", nil, &ObjectTruncated{ID: id}
	}
	gotID := hashBytes(payload)
	if gotID != declaredID {
		// 头部声明的哈希与负载不符：容器被截断后补齐或被篡改。
		return "", nil, &ObjectTampered{ID: id}
	}
	if id != "" && isHexID(id) && id != declaredID {
		// 读取地址与内容地址不一致同样属于篡改。
		return "", nil, &ObjectTampered{ID: id}
	}
	return t, payload, nil
}

// putObject 计算内容地址并原子写入松散对象；已存在且内容一致时幂等成功。
func (r *Repo) putObject(t ObjectType, payload []byte) (string, error) {
	id, data := encodeObject(t, payload)
	path := objectLoosePath(r.root, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return id, nil
		}
		// 同地址不同内容：哈希碰撞在 SHA-256 下视为篡改。
		return "", &ObjectTampered{ID: id}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := writeFileAtomic(path, data, 0o444); err != nil {
		return "", err
	}
	return id, nil
}

// hasObject 判断对象是否存在于松散目录或任一归档中（不做内容校验）。
func (r *Repo) hasObject(id string) bool {
	if !isHexID(id) {
		return false
	}
	if _, err := os.Stat(objectLoosePath(r.root, id)); err == nil {
		return true
	}
	r.packsMu.RLock()
	defer r.packsMu.RUnlock()
	for _, p := range r.packs {
		if _, ok := p.indexOf(id); ok {
			return true
		}
	}
	return false
}

// readObject 按内容地址读取对象，先查松散对象再查归档。
// 查不到返回 *ObjectMissing，容器损坏按 Truncated/Tampered/Corrupt 分类。
func (r *Repo) readObject(id string) (ObjectType, []byte, error) {
	if !isHexID(id) {
		return "", nil, &ObjectMissing{ID: id}
	}
	if data, err := os.ReadFile(objectLoosePath(r.root, id)); err == nil {
		return parseObject(id, data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	r.packsMu.RLock()
	packs := make([]*packFile, 0, len(r.packs))
	packs = append(packs, r.packs...)
	r.packsMu.RUnlock()
	for _, p := range packs {
		if rec, ok := p.indexOf(id); ok {
			data, perr := p.readRecord(rec)
			if perr != nil {
				// 归档记录越界 = 归档被截断；错误归类到被请求对象。
				var trunc *ObjectTruncated
				if errors.As(perr, &trunc) {
					return "", nil, &ObjectTruncated{ID: id}
				}
				return "", nil, perr
			}
			// 归档整包校验和失配时仍做记录级甄别：该记录自身哈希合法
			// 则内容必然未被篡改（SHA-256 内容寻址），否则分类报错。
			if !p.sumOK {
				if _, _, verr := parseObject(id, data); verr != nil {
					return "", nil, verr
				}
			}
			return parseObject(id, data)
		}
	}
	// 对象不在任何健康归档中，但可能落在一个“打不开/魔数损坏”的归档里：
	// 这属于归档损坏，应报 Corrupt 而不是 Missing，避免把坏数据当好数据。
	for _, p := range packs {
		if p.openErr != "" && p.probablyContains(id) {
			return "", nil, &ObjectCorrupt{ID: id, Msg: "pack " + p.name + " unreadable: " + p.openErr}
		}
	}
	return "", nil, &ObjectMissing{ID: id}
}
