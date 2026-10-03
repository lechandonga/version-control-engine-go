package vcs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 引用文件仅存放一行：提交对象 ID（64 hex）加换行。HEAD 另支持
// "ref: refs/heads/<name>\n" 的符号形式。所有写引用路径都走
// 临时文件 + rename，保证读者不会读到半行；所有移动都先追加留痕。

func (r *Repo) headPath() string { return filepath.Join(r.root, "HEAD") }

func branchPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") ||
		strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return "", &RefCorrupt{Name: name, Msg: "illegal branch name"}
	}
	return filepath.Join("refs", "heads", name), nil
}

// readRefFile 读取并校验引用文件。
func readRefFile(path, display string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", &RefNotFound{Name: display}
		}
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !isHexID(id) {
		return "", &RefCorrupt{Name: display, Msg: "not an object id"}
	}
	return id, nil
}

// ResolveBranch 返回分支指向的提交 ID。
func (r *Repo) ResolveBranch(name string) (string, error) {
	rel, err := branchPath(name)
	if err != nil {
		return "", err
	}
	return readRefFile(filepath.Join(r.root, rel), branchRefName(name))
}

// Branches 列出所有分支名（排序）。
func (r *Repo) Branches() ([]string, error) {
	base := filepath.Join(r.root, "refs", "heads")
	var names []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// CurrentBranch 返回当前检出分支名；分离 HEAD 时返回 ("", true, nil)。
func (r *Repo) CurrentBranch() (name string, detached bool, err error) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref: ") {
		return strings.TrimPrefix(strings.TrimPrefix(line, "ref: "), "refs/heads/"), false, nil
	}
	if isHexID(line) {
		return "", true, nil
	}
	return "", false, &RefCorrupt{Name: "HEAD", Msg: "unrecognized content"}
}

// headTarget 解析 HEAD 当前最终提交 ID（分离时直接是该 ID）。
func (r *Repo) headTarget() (string, error) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref: ") {
		ref := strings.TrimPrefix(line, "ref: ")
		name := strings.TrimPrefix(ref, "refs/heads/")
		return r.ResolveBranch(name)
	}
	if isHexID(line) {
		return line, nil
	}
	return "", &RefCorrupt{Name: "HEAD", Msg: "unrecognized content"}
}

func (r *Repo) headSymbolicRef() (string, bool) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	if strings.HasPrefix(line, "ref: ") {
		return strings.TrimPrefix(line, "ref: "), true
	}
	return "", false
}

func (r *Repo) writeHEADSymbolic(ref string) error {
	return writeFileAtomic(r.headPath(), []byte("ref: "+ref+"\n"), 0o644)
}

func (r *Repo) detachHEAD(id string) error {
	return writeFileAtomic(r.headPath(), []byte(id+"\n"), 0o644)
}

func (r *Repo) writeBranchPointer(name, id string) error {
	rel, err := branchPath(name)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(r.root, rel), []byte(id+"\n"), 0o644)
}

func (r *Repo) deleteBranchPointer(name string) error {
	rel, err := branchPath(name)
	if err != nil {
		return err
	}
	return removeIfExists(filepath.Join(r.root, rel))
}

// CreateBranch 在现有提交上创建分支并留痕；已存在时报错。
func (r *Repo) CreateBranch(name, start string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.ResolveBranch(name); err == nil {
		return fmt.Errorf("branch already exists: %s", name)
	}
	if _, err := r.readCommit(start); err != nil {
		return err
	}
	if err := r.appendReflog(branchRefName(name), OpBranchCreate, "", start,
		"branch created at "+shortID(start)); err != nil {
		return err
	}
	return r.writeBranchPointer(name, start)
}

// DeleteBranch 删除分支并留痕（记录旧位置，可据此恢复）。
func (r *Repo) DeleteBranch(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, detached, err := r.CurrentBranch()
	if err != nil {
		return err
	}
	if !detached && cur == name {
		return fmt.Errorf("cannot delete currently checked-out branch %q", name)
	}
	id, err := r.ResolveBranch(name)
	if err != nil {
		return err
	}
	if err := r.appendReflog(branchRefName(name), OpBranchDelete, id, "",
		"branch deleted"); err != nil {
		return err
	}
	return r.deleteBranchPointer(name)
}
