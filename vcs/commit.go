package vcs

import (
	"bufio"
	"bytes"
	"strings"
	"time"
)

// Commit 为提交对象。
type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Timestamp time.Time
	Message   string
}

// commit 负载：头部若干 "key value\n" 行（tree 一行、parent 零至多行、
// author/time 各一行），随后一个空行，其余全部为提交信息原文。

func encodeCommit(c Commit) ([]byte, error) {
	if !isHexID(c.Tree) {
		return nil, &ObjectCorrupt{Msg: "commit with bad tree id"}
	}
	for _, p := range c.Parents {
		if !isHexID(p) {
			return nil, &ObjectCorrupt{ID: p, Msg: "commit with bad parent id"}
		}
	}
	if c.Timestamp.IsZero() {
		c.Timestamp = time.Now()
	}
	var b bytes.Buffer
	b.WriteString("tree ")
	b.WriteString(c.Tree)
	b.WriteByte('\n')
	for _, p := range c.Parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(strings.ReplaceAll(c.Author, "\n", " "))
	b.WriteByte('\n')
	b.WriteString("time ")
	b.WriteString(c.Timestamp.UTC().Format(time.RFC3339Nano))
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString(c.Message)
	return b.Bytes(), nil
}

func parseCommit(id string, payload []byte) (*Commit, error) {
	c := &Commit{}
	sc := bufio.NewScanner(bytes.NewReader(payload))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	sawBlank := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			sawBlank = true
			break
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			return nil, &ObjectCorrupt{ID: id, Msg: "bad commit header"}
		}
		switch key {
		case "tree":
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author = val
		case "time":
			t, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				return nil, &ObjectCorrupt{ID: id, Msg: "bad commit time"}
			}
			c.Timestamp = t
		default:
			return nil, &ObjectCorrupt{ID: id, Msg: "unknown commit header " + key}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &ObjectCorrupt{ID: id, Msg: err.Error()}
	}
	if !sawBlank || !isHexID(c.Tree) {
		return nil, &ObjectCorrupt{ID: id, Msg: "commit missing tree"}
	}
	for _, p := range c.Parents {
		if !isHexID(p) {
			return nil, &ObjectCorrupt{ID: id, Msg: "commit with bad parent"}
		}
	}
	var msg bytes.Buffer
	for sc.Scan() {
		if msg.Len() > 0 {
			msg.WriteByte('\n')
		}
		msg.WriteString(sc.Text())
	}
	c.Message = msg.String()
	return c, nil
}

func (r *Repo) writeCommit(c Commit) (string, error) {
	payload, err := encodeCommit(c)
	if err != nil {
		return "", err
	}
	return r.putObject(ObjectCommit, payload)
}

// readCommit 读取并解析提交对象。
func (r *Repo) readCommit(id string) (*Commit, error) {
	t, payload, err := r.readObject(id)
	if err != nil {
		return nil, err
	}
	if t != ObjectCommit {
		return nil, &ObjectCorrupt{ID: id, Msg: "expected commit, got " + string(t)}
	}
	return parseCommit(id, payload)
}

// ReadCommit 为 readCommit 的导出包装，供 CLI / 外部工具查看提交。
func (r *Repo) ReadCommit(id string) (*Commit, error) { return r.readCommit(id) }

// readBlob 读取文件对象负载。
func (r *Repo) readBlob(id string) ([]byte, error) {
	t, payload, err := r.readObject(id)
	if err != nil {
		return nil, err
	}
	if t != ObjectBlob {
		return nil, &ObjectCorrupt{ID: id, Msg: "expected blob, got " + string(t)}
	}
	return payload, nil
}
