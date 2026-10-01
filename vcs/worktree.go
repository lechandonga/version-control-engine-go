package vcs

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileSet 是路径（相对工作区、正斜杠）到文件内容的映射。
type FileSet map[string][]byte

// BlobMap 是路径到 blob ID 的映射（树的扁平表示）。
type BlobMap map[string]string

func validRelPath(p string) bool {
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return false
	}
	if strings.HasPrefix(p, ".vcs/") || p == ".vcs" || strings.Contains(p, "/.vcs/") || strings.HasSuffix(p, "/.vcs") {
		return false
	}
	return true
}

// flattenTree 递归展开一棵树为 BlobMap。
func (r *Repository) flattenTree(treeID string) (BlobMap, error) {
	out := BlobMap{}
	if err := r.flattenInto(treeID, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) flattenInto(treeID, prefix string, out BlobMap) error {
	t, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		p := e.Path
		if prefix != "" {
			p = prefix + "/" + e.Path
		}
		if e.Mode.IsDir() {
			if err := r.flattenInto(e.ID, p, out); err != nil {
				return err
			}
		} else {
			out[p] = e.ID
		}
	}
	return nil
}

// buildTree 把扁平 BlobMap 构建为嵌套树，返回根树 ID。
// 空映射表示空目录，得到一棵空树。
func (r *Repository) buildTree(files BlobMap) (string, error) {
	root := &dirNode{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		parts := strings.Split(p, "/")
		node := root
		for _, part := range parts[:len(parts)-1] {
			if part == "" || part == "." || part == ".." {
				return "", &PathError{Path: p, Err: ErrInvalidPath}
			}
			next := node.dirs[part]
			if next == nil {
				next = &dirNode{}
				if node.dirs == nil {
					node.dirs = map[string]*dirNode{}
				}
				node.dirs[part] = next
			}
			node = next
		}
		name := parts[len(parts)-1]
		if name == "" || name == "." || name == ".." {
			return "", &PathError{Path: p, Err: ErrInvalidPath}
		}
		if node.files == nil {
			node.files = map[string]string{}
		}
		node.files[name] = files[p]
	}
	return r.writeDirNode(root)
}

type dirNode struct {
	dirs  map[string]*dirNode
	files map[string]string // name -> blob id
}

func (r *Repository) writeDirNode(n *dirNode) (string, error) {
	var entries []TreeEntry
	for name, child := range n.dirs {
		id, err := r.writeDirNode(child)
		if err != nil {
			return "", err
		}
		entries = append(entries, TreeEntry{Mode: ModeDir, Path: name, ID: id})
	}
	for name, blob := range n.files {
		entries = append(entries, TreeEntry{Mode: ModeRegular, Path: name, ID: blob})
	}
	return r.writeTree(&Tree{Entries: entries})
}

// ErrInvalidPath 表示工作区内出现非法路径。
var ErrInvalidPath = bytesError("vcs: invalid path")

// PathError 携带具体非法路径。
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string { return e.Err.Error() + ": " + e.Path }
func (e *PathError) Unwrap() error { return e.Err }

func bytesError(s string) error { return &stringErr{s: s} }

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

// readWorkdir 扫描工作区，返回所有非 .vcs 普通文件的内容。
func (r *Repository) readWorkdir() (FileSet, error) {
	files := FileSet{}
	err := filepath.Walk(r.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.workDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".vcs" || strings.HasPrefix(rel, ".vcs/") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return &PathError{Path: rel, Err: ErrInvalidPath}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// materialize 把目标文件集完整落地到工作区：
// 删除 oldTracked 中不再属于目标的文件/空目录，写入目标内容。
// 调用方必须保证不会覆盖未跟踪文件（见 Checkout 的安全检查）。
func (r *Repository) materialize(target BlobMap, oldTracked BlobMap) error {
	// 1. 删除旧跟踪、目标不存在的文件。
	oldPaths := make([]string, 0, len(oldTracked))
	for p := range oldTracked {
		oldPaths = append(oldPaths, p)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(oldPaths)))
	for _, p := range oldPaths {
		if _, ok := target[p]; ok {
			continue
		}
		abs := filepath.Join(r.workDir, filepath.FromSlash(p))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// 2. 清理变空的目录（从深到浅），保留工作区根与 .vcs。
	r.removeEmptyDirs(oldPaths)

	// 3. 写入目标文件（内容变化才写）。
	paths := make([]string, 0, len(target))
	for p := range target {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		abs := filepath.Join(r.workDir, filepath.FromSlash(p))
		data, err := r.readBlob(target[p])
		if err != nil {
			return err
		}
		if existing, err := os.ReadFile(abs); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(abs, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) removeEmptyDirs(filePaths []string) {
	dirs := map[string]bool{}
	for _, p := range filePaths {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			dirs[strings.Join(parts[:i], "/")] = true
		}
	}
	var list []string
	for d := range dirs {
		list = append(list, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(list)))
	for _, d := range list {
		if d == ".vcs" || strings.HasPrefix(d, ".vcs/") {
			continue
		}
		abs := filepath.Join(r.workDir, filepath.FromSlash(d))
		// 只在目录为空时移除，忽略错误（目录非空等情况）。
		_ = os.Remove(abs)
	}
}
