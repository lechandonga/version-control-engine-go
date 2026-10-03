package vcs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 暂存区（.vcs/index）同样是一行式文本：
//
//	<mode> <blob-id> <path>\n
//
// 只记录普通文件；路径保证相对、不含 ".." 段。

// IndexEntry 为暂存区一项。
type IndexEntry struct {
	Mode string
	ID   string
	Path string
}

func safeRelPath(p string) (string, bool) {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." || strings.HasPrefix(p, "../") || p == ".." ||
		strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == ".." || seg == "." {
			return "", false
		}
	}
	return p, true
}

func (r *Repo) indexPath() string { return filepath.Join(r.root, "index") }

func (r *Repo) readIndex() ([]IndexEntry, error) {
	data, err := os.ReadFile(r.indexPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []IndexEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			return nil, &RefCorrupt{Name: "index", Msg: "bad index line: " + sc.Text()}
		}
		if (fields[0] != "100644" && fields[0] != "40000") || !isHexID(fields[1]) {
			return nil, &RefCorrupt{Name: "index", Msg: "bad index entry"}
		}
		p, ok := safeRelPath(fields[2])
		if !ok {
			return nil, &RefCorrupt{Name: "index", Msg: "bad path: " + fields[2]}
		}
		out = append(out, IndexEntry{Mode: fields[0], ID: fields[1], Path: p})
	}
	return out, sc.Err()
}

func (r *Repo) writeIndex(entries []IndexEntry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s\n", e.Mode, e.ID, e.Path)
	}
	return writeFileAtomic(r.indexPath(), b.Bytes(), 0o644)
}

// indexToTree 递归地把扁平暂存项构建为树对象，返回根树 ID。
func (r *Repo) indexToTree(entries []IndexEntry) (string, error) {
	root := map[string]interface{}{}
	for _, e := range entries {
		parts := strings.Split(e.Path, "/")
		dir := root
		for _, seg := range parts[:len(parts)-1] {
			next, ok := dir[seg].(map[string]interface{})
			if !ok {
				next = map[string]interface{}{}
				dir[seg] = next
			}
			dir = next
		}
		dir[parts[len(parts)-1]] = e.ID
	}
	var write func(map[string]interface{}) (string, error)
	write = func(nodes map[string]interface{}) (string, error) {
		var treeEntries []TreeEntry
		names := make([]string, 0, len(nodes))
		for n := range nodes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			switch v := nodes[n].(type) {
			case string:
				treeEntries = append(treeEntries, TreeEntry{Name: n, Mode: "100644", ID: v})
			case map[string]interface{}:
				id, err := write(v)
				if err != nil {
					return "", err
				}
				treeEntries = append(treeEntries, TreeEntry{Name: n, Mode: "40000", ID: id})
			}
		}
		return r.writeTree(treeEntries)
	}
	return write(root)
}

// treeToIndex 展开树为扁平暂存项。
func (r *Repo) treeToIndex(treeID, prefix string, out *[]IndexEntry) error {
	entries, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			if err := r.treeToIndex(e.ID, p, out); err != nil {
				return err
			}
		} else {
			*out = append(*out, IndexEntry{Mode: e.Mode, ID: e.ID, Path: p})
		}
	}
	return nil
}

func (r *Repo) treeFlat(treeID string) ([]IndexEntry, error) {
	var out []IndexEntry
	err := r.treeToIndex(treeID, "", &out)
	return out, err
}

// Add 把工作区文件写入对象并加入暂存区。
func (r *Repo) Add(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := safeRelPath(path)
	if !ok {
		return fmt.Errorf("bad path: %s", path)
	}
	entries, err := r.readIndex()
	if err != nil {
		return err
	}
	abs := filepath.Join(r.workdir(), filepath.FromSlash(p))
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	id, err := r.putObject(ObjectBlob, data)
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.Path != p {
			kept = append(kept, e)
		}
	}
	kept = append(kept, IndexEntry{Mode: "100644", ID: id, Path: p})
	return r.writeIndex(kept)
}

// AddAll 暂存工作区下全部普通文件（忽略 .vcs）。
func (r *Repo) AddAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var entries []IndexEntry
	err := filepath.WalkDir(r.workdir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.workdir(), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".vcs" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		id, err := r.putObject(ObjectBlob, data)
		if err != nil {
			return err
		}
		entries = append(entries, IndexEntry{Mode: "100644", ID: id, Path: rel})
		return nil
	})
	if err != nil {
		return err
	}
	return r.writeIndex(entries)
}

// CommitOptions 控制提交行为。
type CommitOptions struct {
	Message string
	Author  string
	// AllowEmpty 允许无相对 HEAD 的内容变化。
	AllowEmpty bool
}

// Commit 用当前暂存区创建提交并原子前进当前分支 / HEAD，同时留痕。
func (r *Repo) Commit(opts CommitOptions) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if opts.Message == "" {
		return "", errors.New("empty commit message")
	}
	entries, err := r.readIndex()
	if err != nil {
		return "", err
	}
	treeID, err := r.indexToTree(entries)
	if err != nil {
		return "", err
	}
	var parents []string
	old, err := r.headTarget()
	switch {
	case err == nil:
		parents = []string{old}
		if !opts.AllowEmpty {
			oldCommit, cerr := r.readCommit(old)
			if cerr == nil && oldCommit.Tree == treeID {
				return "", errors.New("nothing to commit")
			}
		}
	default:
		var rc *RefCorrupt
		if errors.As(err, &rc) {
			return "", err
		}
		// HEAD 指向不存在的分支：首个提交。
	}
	id, err := r.writeCommit(Commit{
		Tree:      treeID,
		Parents:   parents,
		Author:    opts.Author,
		Timestamp: time.Now(),
		Message:   opts.Message,
	})
	if err != nil {
		return "", err
	}
	if err := r.appendReflog("HEAD", OpCommit, old, id, opts.Message); err != nil {
		return "", err
	}
	if ref, symbolic := r.headSymbolicRef(); symbolic {
		name := strings.TrimPrefix(ref, "refs/heads/")
		if err := r.writeBranchPointer(name, id); err != nil {
			return "", err
		}
		if ref != "HEAD" {
			if err := r.appendReflog(ref, OpCommit, old, id, opts.Message); err != nil {
				return "", err
			}
		}
	} else {
		if err := r.detachHEAD(id); err != nil {
			return "", err
		}
	}
	return id, nil
}

// HEADCommit 返回当前提交；空仓库返回 ("", nil)。
func (r *Repo) HEADCommit() (string, error) {
	id, err := r.headTarget()
	if err != nil {
		var nf *RefNotFound
		if errors.As(err, &nf) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// workdir 返回工作区根目录（.vcs 的上一级）。
func (r *Repo) workdir() string { return filepath.Dir(r.root) }
