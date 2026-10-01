package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 引用落盘格式：<root>/refs/heads/<name> 文件内容为
// "<64 位十六进制提交 ID>\n"。更新通过临时文件 + rename 原子完成，
// 读方只能看到更新前或更新后的完整内容。

func validateRefName(name string) error {
	if name == "" || name == "HEAD" {
		return fmt.Errorf("vcs: invalid ref name %q", name)
	}
	for _, c := range name {
		if c == 0 || c == '/' || c == '\\' || c == ':' || c == ' ' ||
			c == '~' || c == '^' || c == '?' || c == '*' || c == '[' {
			return fmt.Errorf("vcs: invalid ref name %q", name)
		}
	}
	if strings.HasPrefix(name, ".") || strings.Contains(name, "..") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("vcs: invalid ref name %q", name)
	}
	return nil
}

func (r *Repository) refPath(name string) string {
	return filepath.Join(r.root, dirRefs, dirHeads, filepath.FromSlash(name))
}

// refExists 报告分支引用是否已存在（不做内容校验）。
func (r *Repository) refExists(name string) bool {
	return fileExists(r.refPath(name))
}

// readRef 读取引用并分类失败原因：
//   - ErrRefNotFound：文件缺失
//   - ErrRefCorrupt：内容为空/含非法字符/长度不对
//   - ErrRefDangling：指向的提交对象缺失
//   - ErrRefType：指向的对象不是提交
func (r *Repository) readRef(name string) (string, error) {
	data, err := os.ReadFile(r.refPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", &RefError{Name: name, Kind: ErrRefNotFound}
		}
		return "", err
	}
	id := strings.TrimSuffix(string(data), "\n")
	if len(id) != IDLen || !allHex(id) || strings.ContainsAny(string(data), "\r") {
		return "", &RefError{Name: name, Kind: ErrRefCorrupt}
	}
	obj, err := r.objects.read(id)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return "", &RefError{Name: name, Kind: ErrRefDangling}
		}
		return "", err
	}
	if obj.objType != TypeCommit {
		return "", &RefError{Name: name, Kind: ErrRefType}
	}
	return id, nil
}

// verifyRef 供 Fsck 使用。
func (r *Repository) verifyRef(name string) error {
	_, err := r.readRef(name)
	return err
}

// writeRefAtomic 原子写入引用。
func (r *Repository) writeRefAtomic(name, id string) error {
	if err := validateRefName(name); err != nil {
		return err
	}
	if len(id) != IDLen || !allHex(id) {
		return fmt.Errorf("vcs: invalid commit id %q", id)
	}
	return writeFileAtomic(r.refPath(name), []byte(id+"\n"), 0o644)
}

func (r *Repository) deleteRef(name string) error {
	path := r.refPath(name)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return &RefError{Name: name, Kind: ErrRefNotFound}
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

// listRefs 按名称排序列出全部本地分支引用。
func (r *Repository) listRefs() ([]string, error) {
	base := filepath.Join(r.root, dirRefs, dirHeads)
	var names []string
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
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
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// HEAD 格式：
//   - 符号引用："ref: refs/heads/<name>\n"（指向分支，允许未出生）
//   - 分离头：  "<64 hex>\n"

func (r *Repository) headPath() string { return filepath.Join(r.root, fileHEAD) }

// readHEADRaw 返回 (symRef 或 "", detachedID 或 "", err)。
func (r *Repository) readHEADRaw() (string, string, error) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", "", err
	}
	s := strings.TrimSuffix(string(data), "\n")
	if strings.HasPrefix(s, "ref: refs/heads/") {
		name := strings.TrimPrefix(s, "ref: refs/heads/")
		if err := validateRefName(name); err != nil {
			return "", "", err
		}
		return name, "", nil
	}
	if len(s) == IDLen && allHex(s) {
		return "", s, nil
	}
	return "", "", errors.New("vcs: HEAD corrupt")
}

// CurrentBranch 返回 HEAD 当前指向的分支名；分离头返回 ""。
func (r *Repository) CurrentBranch() (string, error) {
	name, id, err := r.readHEADRaw()
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	_ = id
	return "", nil
}

func (r *Repository) writeHEADSymbolic(name string) error {
	if err := validateRefName(name); err != nil {
		return err
	}
	return writeFileAtomic(r.headPath(), []byte("ref: refs/heads/"+name+"\n"), 0o644)
}

func (r *Repository) writeHEADDetached(id string) error {
	if len(id) != IDLen || !allHex(id) {
		return fmt.Errorf("vcs: invalid commit id %q", id)
	}
	return writeFileAtomic(r.headPath(), []byte(id+"\n"), 0o644)
}

// HeadCommit 返回 HEAD 解析出的提交 ID。
// 未出生分支与空仓库返回 ErrRefNotFound。
func (r *Repository) HeadCommit() (string, error) {
	name, detached, err := r.readHEADRaw()
	if err != nil {
		return "", err
	}
	if detached != "" {
		obj, err := r.objects.read(detached)
		if err != nil {
			return "", err
		}
		if obj.objType != TypeCommit {
			return "", &RefError{Name: "HEAD", Kind: ErrRefType}
		}
		return detached, nil
	}
	id, err := r.readRef(name)
	if err != nil {
		return "", err
	}
	return id, nil
}

// resolveCommit 解析分支名或裸提交 ID 为提交 ID。
func (r *Repository) resolveCommit(ref string) (string, error) {
	if len(ref) == IDLen && allHex(ref) {
		obj, err := r.objects.read(ref)
		if err != nil {
			return "", err
		}
		if obj.objType != TypeCommit {
			return "", &IntegrityError{ID: ref, Kind: ErrObjectTypeMismatch}
		}
		return ref, nil
	}
	return r.readRef(ref)
}
