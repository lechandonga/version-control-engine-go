// Package vcs 实现一个完全落盘的本地内容寻址版本控制引擎。
//
// 统一对象封装格式：
//
//	header  = type SP body-length LF
//	body    = 类型特有内容，长度恰好为 body-length 字节
//	trailer = SHA-256(header || body) 的 32 字节原始摘要
//
// 对象标识（ID）即 trailer 中 SHA-256 的十六进制编码。
//
//   - blob body: 文件原始字节
//
//   - tree body: 若干条目直接相连；每个条目为
//     mode SP path NUL <32 字节原始哈希> NUL
//     mode 为 "100644" 或 "040000"，条目严格按 path 字节序升序。
//     记录定长边界由 “两个 NUL + 定长哈希” 决定，因此哈希中
//     任意字节（含 0x0A）都不会破坏解析。
//
//   - commit body:
//
//     tree SP <hex>\n
//     parent SP <hex>\n   （零行或多行）
//     author SP <name> SP <email> SP <unix-nanos> SP <tz-offset>\n
//     committer SP ...（同 author）\n
//     \n
//     message（任意字节）
package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IDLen 是十六进制对象标识的长度（SHA-256）。
const IDLen = sha256.Size * 2

// 对象类型常量。
const (
	TypeBlob   = "blob"
	TypeTree   = "tree"
	TypeCommit = "commit"
)

// FileMode 描述树条目类型。
type FileMode string

// 支持的树条目模式。
const (
	ModeRegular FileMode = "100644"
	ModeDir     FileMode = "040000"
)

// IsDir 判断是否目录模式。
func (m FileMode) IsDir() bool { return m == ModeDir }

// Hash 计算任意字节的对象标识。
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeID(s string) ([sha256.Size]byte, error) {
	var raw [sha256.Size]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return raw, fmt.Errorf("invalid object id %q", s)
	}
	copy(raw[:], b)
	return raw, nil
}

// encodeObject 给 body 加统一头与校验尾。
func encodeObject(objType string, body []byte) []byte {
	header := []byte(fmt.Sprintf("%s %d\n", objType, len(body)))
	out := make([]byte, 0, len(header)+len(body)+sha256.Size)
	out = append(out, header...)
	out = append(out, body...)
	sum := sha256.Sum256(out)
	out = append(out, sum[:]...)
	return out
}

// parsedObject 是一次完整性校验后的解码结果。
type parsedObject struct {
	id      string
	objType string
	body    []byte
}

// parseObject 执行全部完整性检查。
//
// 判定顺序（失败原因互斥、可区分）：
//  1. trailer 不足 32 字节或 header 不完整：ErrObjectTruncated
//  2. 声明长度与实际 body 不符：ErrObjectTruncated
//  3. 实际哈希与摘要不符：ErrObjectTampered
//  4. 类型未知：ErrObjectCorrupt
func parseObject(id string, raw []byte) (*parsedObject, error) {
	if len(raw) < sha256.Size {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTruncated}
	}
	content := raw[:len(raw)-sha256.Size]
	trailer := raw[len(raw)-sha256.Size:]

	nl := bytes.IndexByte(content, '\n')
	if nl < 0 {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTruncated}
	}
	headerParts := strings.SplitN(string(content[:nl]), " ", 2)
	if len(headerParts) != 2 {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectCorrupt}
	}
	objType := headerParts[0]
	declLen, err := strconv.Atoi(headerParts[1])
	if err != nil || declLen < 0 {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectCorrupt}
	}
	body := content[nl+1:]
	if len(body) != declLen {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTruncated}
	}

	sum := sha256.Sum256(content)
	if !bytes.Equal(sum[:], trailer) {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTampered}
	}

	switch objType {
	case TypeBlob, TypeTree, TypeCommit:
	default:
		return nil, &IntegrityError{ID: id, Kind: ErrObjectCorrupt}
	}
	return &parsedObject{id: id, objType: objType, body: body}, nil
}

// TreeEntry 是树中的一个条目。
type TreeEntry struct {
	Mode FileMode
	Path string
	ID   string
}

// Tree 是一棵目录树。
type Tree struct {
	Entries []TreeEntry
}

func (t *Tree) get(path string) (TreeEntry, bool) {
	i := sort.Search(len(t.Entries), func(i int) bool { return t.Entries[i].Path >= path })
	if i < len(t.Entries) && t.Entries[i].Path == path {
		return t.Entries[i], true
	}
	return TreeEntry{}, false
}

func encodeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Path)
		raw, err := decodeID(e.ID)
		if err != nil {
			panic(err)
		}
		buf.Write(raw[:])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// parseTreeBody 解析 tree body；结构异常统一返回 ErrObjectCorrupt。
func parseTreeBody(body []byte) (*Tree, error) {
	t := &Tree{}
	rest := body
	for len(rest) > 0 {
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+1+sha256.Size+1 {
			return nil, ErrObjectCorrupt
		}
		head := string(rest[:nul])
		sp := strings.IndexByte(head, ' ')
		if sp < 0 {
			return nil, ErrObjectCorrupt
		}
		mode := FileMode(head[:sp])
		path := head[sp+1:]
		if (mode != ModeRegular && mode != ModeDir) || path == "" || strings.Contains(path, "/") {
			return nil, ErrObjectCorrupt
		}
		hashStart := nul + 1
		hashEnd := hashStart + sha256.Size
		if rest[hashEnd] != 0 {
			return nil, ErrObjectCorrupt
		}
		t.Entries = append(t.Entries, TreeEntry{
			Mode: mode,
			Path: path,
			ID:   hex.EncodeToString(rest[hashStart:hashEnd]),
		})
		rest = rest[hashEnd+1:]
	}
	if !sort.SliceIsSorted(t.Entries, func(i, j int) bool { return t.Entries[i].Path < t.Entries[j].Path }) {
		return nil, ErrObjectCorrupt
	}
	return t, nil
}

// Signature 标识提交作者/提交者。
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

func (s Signature) encode() string {
	_, offset := s.When.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s <%s> %d %s%02d%02d",
		s.Name, s.Email, s.When.UnixNano(), sign, offset/3600, (offset%3600)/60)
}

func parseSignature(s string) (Signature, error) {
	var sig Signature
	lt := strings.IndexByte(s, '<')
	gt := strings.LastIndexByte(s, '>')
	if lt <= 0 || gt < lt+1 {
		return sig, ErrObjectCorrupt
	}
	sig.Name = strings.TrimSpace(s[:lt])
	sig.Email = s[lt+1 : gt]
	fields := strings.Fields(strings.TrimSpace(s[gt+1:]))
	if len(fields) != 2 {
		return sig, ErrObjectCorrupt
	}
	nanos, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return sig, ErrObjectCorrupt
	}
	tz := fields[1]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return sig, ErrObjectCorrupt
	}
	hours, err1 := strconv.Atoi(tz[1:3])
	mins, err2 := strconv.Atoi(tz[3:5])
	if err1 != nil || err2 != nil {
		return sig, ErrObjectCorrupt
	}
	offsetSec := hours*3600 + mins*60
	if tz[0] == '-' {
		offsetSec = -offsetSec
	}
	sig.When = time.Unix(0, nanos).In(time.FixedZone("", offsetSec))
	return sig, nil
}

// Commit 是一次提交。
type Commit struct {
	Tree      string
	Parents   []string
	Author    Signature
	Committer Signature
	Message   string
}

func encodeCommit(c *Commit) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s\n", c.Author.encode())
	fmt.Fprintf(&buf, "committer %s\n", c.Committer.encode())
	buf.WriteByte('\n')
	buf.WriteString(c.Message)
	return buf.Bytes()
}

func parseCommitBody(body []byte) (*Commit, error) {
	c := &Commit{}
	sep := bytes.Index(body, []byte("\n\n"))
	var headerBlock, message []byte
	if sep < 0 {
		headerBlock = body
	} else {
		headerBlock = body[:sep]
		message = body[sep+2:]
	}
	treeSeen, authorSeen, committerSeen := false, false, false
	for _, line := range strings.Split(string(headerBlock), "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			return nil, ErrObjectCorrupt
		}
		switch key {
		case "tree":
			if treeSeen || len(val) != IDLen {
				return nil, ErrObjectCorrupt
			}
			c.Tree = val
			treeSeen = true
		case "parent":
			if len(val) != IDLen {
				return nil, ErrObjectCorrupt
			}
			c.Parents = append(c.Parents, val)
		case "author":
			if authorSeen {
				return nil, ErrObjectCorrupt
			}
			sig, err := parseSignature(val)
			if err != nil {
				return nil, err
			}
			c.Author = sig
			authorSeen = true
		case "committer":
			if committerSeen {
				return nil, ErrObjectCorrupt
			}
			sig, err := parseSignature(val)
			if err != nil {
				return nil, err
			}
			c.Committer = sig
			committerSeen = true
		default:
			return nil, ErrObjectCorrupt
		}
	}
	if !treeSeen || !authorSeen || !committerSeen {
		return nil, ErrObjectCorrupt
	}
	c.Message = string(message)
	return c, nil
}

// writeBlob 写入文件内容对象。
func (r *Repository) writeBlob(data []byte) (string, error) {
	return r.objects.write(TypeBlob, data)
}

// writeTree 写入目录树。
func (r *Repository) writeTree(t *Tree) (string, error) {
	return r.objects.write(TypeTree, encodeTree(t.Entries))
}

// writeCommit 写入提交。
func (r *Repository) writeCommit(c *Commit) (string, error) {
	return r.objects.write(TypeCommit, encodeCommit(c))
}

// readTree 读取并校验一棵树（仅校验自身结构，递归完整性见 Fsck）。
func (r *Repository) readTree(id string) (*Tree, error) {
	raw, err := r.objects.read(id)
	if err != nil {
		return nil, err
	}
	if raw.objType != TypeTree {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTypeMismatch}
	}
	t, err := parseTreeBody(raw.body)
	if err != nil {
		return nil, &IntegrityError{ID: id, Kind: errors.Join(ErrObjectCorrupt, err)}
	}
	return t, nil
}

// ReadCommit 读取并校验一个提交对象。
func (r *Repository) ReadCommit(id string) (*Commit, error) {
	raw, err := r.objects.read(id)
	if err != nil {
		return nil, err
	}
	if raw.objType != TypeCommit {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTypeMismatch}
	}
	c, err := parseCommitBody(raw.body)
	if err != nil {
		return nil, &IntegrityError{ID: id, Kind: errors.Join(ErrObjectCorrupt, err)}
	}
	return c, nil
}

// readBlob 读取文件对象内容。
func (r *Repository) readBlob(id string) ([]byte, error) {
	raw, err := r.objects.read(id)
	if err != nil {
		return nil, err
	}
	if raw.objType != TypeBlob {
		return nil, &IntegrityError{ID: id, Kind: ErrObjectTypeMismatch}
	}
	return raw.body, nil
}
