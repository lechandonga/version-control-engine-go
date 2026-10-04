package vcs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Tree 对象负载：有序条目列表，JSON 编码，键稳定保证内容寻址确定。
type Tree struct {
	Entries []TreeEntry `json:"entries"`
}

type TreeEntry struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Commit 对象负载。
type Commit struct {
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
	Message string   `json:"message"`
	Author  string   `json:"author"`
	Time    string   `json:"time"`
}

func (r *Repo) writeTree(t *Tree) (string, error) {
	data, _ := json.Marshal(t)
	return r.WriteObject(TypeTree, data)
}

// ReadTree 读取并解析 tree 对象。
func (r *Repo) ReadTree(id string) (*Tree, error) {
	typ, payload, err := r.ReadObject(id)
	if err != nil {
		return nil, err
	}
	if typ != TypeTree {
		return nil, &ObjectCorrupt{ID: id, Msg: "expected tree, got " + typ}
	}
	var t Tree
	if err := json.Unmarshal(payload, &t); err != nil {
		return nil, &ObjectCorrupt{ID: id, Msg: "bad tree encoding"}
	}
	return &t, nil
}

// ReadCommit 读取并解析 commit 对象。
func (r *Repo) ReadCommit(id string) (*Commit, error) {
	typ, payload, err := r.ReadObject(id)
	if err != nil {
		return nil, err
	}
	if typ != TypeCommit {
		return nil, &ObjectCorrupt{ID: id, Msg: "expected commit, got " + typ}
	}
	var c Commit
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, &ObjectCorrupt{ID: id, Msg: "bad commit encoding"}
	}
	return &c, nil
}

// flattenTree 将 tree 展开为 路径->blobID 的映射。
func (r *Repo) flattenTree(treeID string) (map[string]string, error) {
	out := map[string]string{}
	var walk func(id, prefix string) error
	walk = func(id, prefix string) error {
		t, err := r.ReadTree(id)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			if strings.HasSuffix(e.Name, "/") {
				if err := walk(e.ID, prefix+e.Name); err != nil {
					return err
				}
			} else {
				out[prefix+e.Name] = e.ID
			}
		}
		return nil
	}
	if treeID == "" {
		return out, nil
	}
	return out, walk(treeID, "")
}

// workPath 将仓库相对路径映射到工作区路径。
func (r *Repo) workPath(rel string) string { return filepath.Join(r.Root, filepath.FromSlash(rel)) }

// listWorkFiles 列出工作区文件（排除 .vcs），返回 相对路径->blobID。
func (r *Repo) listWorkFiles() (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(r.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".vcs" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(r.Root, p)
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = hashBytes(data)
		return nil
	})
	return out, err
}

// snapshot 把工作区写入对象库，返回根 tree ID。
func (r *Repo) snapshot() (string, error) {
	files, err := r.listWorkFiles()
	if err != nil {
		return "", err
	}
	// 先写 blob，再自底向上写 tree。
	dirs := map[string][]TreeEntry{"": {}}
	for _, rel := range sortedKeys(files) {
		data, err := os.ReadFile(r.workPath(rel))
		if err != nil {
			return "", err
		}
		id, err := r.WriteObject(TypeBlob, data)
		if err != nil {
			return "", err
		}
		dir, name := splitPath(rel)
		dirs[dir] = append(dirs[dir], TreeEntry{Name: name, ID: id})
	}
	// 自底向上聚合目录。
	depths := make([]string, 0, len(dirs))
	for d := range dirs {
		depths = append(depths, d)
	}
	sort.Slice(depths, func(i, j int) bool { return strings.Count(depths[i], "/") > strings.Count(depths[j], "/") })
	treeIDs := map[string]string{}
	for _, d := range depths {
		t := &Tree{Entries: dirs[d]}
		id, err := r.writeTree(t)
		if err != nil {
			return "", err
		}
		treeIDs[d] = id
		if d == "" {
			continue
		}
		parent, name := splitPath(strings.TrimSuffix(d, "/"))
		dirs[parent] = append(dirs[parent], TreeEntry{Name: name + "/", ID: id})
	}
	return treeIDs[""], nil
}

// splitPath 拆分 "a/b/c" 为目录前缀 "a/b/" 与末段 "c"。
func splitPath(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i+1], p[i+1:]
}

// Commit 把当前工作区快照提交到当前分支，返回新 commit ID。
func (r *Repo) Commit(message string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 老仓库可能已处于“HEAD 指向被删分支”的状态（早期版本允许删除当前分支）。
	// 此时直接提交会产生没有父提交的新根、悄悄断开历史；明确拒绝并给出恢复指引。
	if ref, err := r.HeadRef(); err != nil {
		return "", err
	} else if ref != "" {
		if _, err := r.ReadRef(ref); err != nil {
			if _, ok := err.(*RefNotFound); !ok {
				return "", err
			}
			// 新仓库首次提交时引用尚未创建（正常）；只有操作记录表明
			// HEAD/该分支曾经移动过，才是“当前分支被删”的悬空状态。
			if r.hasHistoryLocked(ref) {
				return "", &OrphanHead{Name: ref}
			}
		}
	}
	treeID, err := r.snapshot()
	if err != nil {
		return "", err
	}
	var parents []string
	if head, err := r.CurrentCommit(); err != nil {
		return "", err
	} else if head != "" {
		parents = append(parents, head)
	}
	op := "commit"
	// 有未完成的合并现场时，本次提交即合并提交。
	if st, err := r.readMergeState(); err != nil {
		return "", err
	} else if st != nil {
		parents = append(parents, st.Theirs)
		op = "merge"
	}
	c := &Commit{
		Tree:    treeID,
		Parents: parents,
		Message: message,
		Author:  "vcs",
		Time:    r.Now().UTC().Format(time.RFC3339Nano),
	}
	data, _ := json.Marshal(c)
	id, err := r.WriteObject(TypeCommit, data)
	if err != nil {
		return "", err
	}
	if err := r.updateHeadLocked(id, op, message); err != nil {
		return "", err
	}
	if op == "merge" {
		os.Remove(r.mergeStatePath())
	}
	return id, nil
}

// CreateBranch 在当前位置创建分支。
func (r *Repo) CreateBranch(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	head, err := r.CurrentCommit()
	if err != nil {
		return err
	}
	if head == "" {
		return &RefNotFound{Name: "HEAD"}
	}
	ref := "refs/heads/" + name
	if _, err := r.ReadRef(ref); err == nil {
		return &WouldOverwrite{Paths: []string{name}}
	}
	return r.writeRefLocked(ref, head, "branch", "create "+name)
}

// DeleteBranch 删除分支；操作日志保留其最后位置，可据此找回。
func (r *Repo) DeleteBranch(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := "refs/heads/" + name
	// 当前检出位置所在的分支不允许删除：否则 HEAD 会悬空，下一次提交变成无父新根。
	// 拒绝时不改动分支、HEAD、工作区与操作记录。
	if headRef, err := r.HeadRef(); err != nil {
		return err
	} else if headRef == ref {
		return &BranchCheckedOut{Name: name}
	}
	old, err := r.ReadRef(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(r.refPath(ref)); err != nil {
		return err
	}
	r.logMoveLocked("branch-delete", ref, old, "", "delete "+name)
	return nil
}

// Checkout 切换到分支或提交，更新工作区与 HEAD。
func (r *Repo) Checkout(target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := "refs/heads/" + target
	var commitID, headTarget string
	if id, err := r.ReadRef(ref); err == nil {
		commitID, headTarget = id, ref
	} else if isHexID(target) && r.HasObject(target) {
		commitID, headTarget = target, target
	} else {
		return &RefNotFound{Name: target}
	}
	if err := r.checkoutTreeLocked(commitID); err != nil {
		return err
	}
	return r.setHeadLocked(headTarget, "switch", "switch to "+target)
}

// checkoutTreeLocked 把指定 commit 的内容落到工作区，并保护本地修改。调用方需持有 r.mu。
func (r *Repo) checkoutTreeLocked(commitID string) error {
	c, err := r.ReadCommit(commitID)
	if err != nil {
		return err
	}
	target, err := r.flattenTree(c.Tree)
	if err != nil {
		return err
	}
	var current map[string]string
	if head, err := r.CurrentCommit(); err != nil {
		return err
	} else if head != "" {
		hc, err := r.ReadCommit(head)
		if err != nil {
			return err
		}
		current, err = r.flattenTree(hc.Tree)
		if err != nil {
			return err
		}
	} else {
		current = map[string]string{}
	}
	work, err := r.listWorkFiles()
	if err != nil {
		return err
	}
	// 覆盖保护：本地有修改或未跟踪文件会被目标版本覆盖时拒绝。
	var conflicts []string
	for path, tid := range target {
		wid, tracked := work[path]
		cid, wasTracked := current[path]
		if !tracked || wid == tid {
			continue
		}
		if wasTracked && wid == cid {
			continue // 未本地修改，安全覆盖
		}
		conflicts = append(conflicts, path)
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return &WouldOverwrite{Paths: conflicts}
	}
	// 删除不再跟踪的文件，写入新内容。
	for path := range current {
		if _, ok := target[path]; !ok {
			os.Remove(r.workPath(path))
		}
	}
	for path, id := range target {
		typ, payload, err := r.ReadObject(id)
		if err != nil {
			return err
		}
		if typ != TypeBlob {
			return &ObjectCorrupt{ID: id, Msg: "expected blob"}
		}
		if err := os.MkdirAll(filepath.Dir(r.workPath(path)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(r.workPath(path), payload, 0o644); err != nil {
			return err
		}
	}
	return nil
}
