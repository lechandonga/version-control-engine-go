package vcs

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// hashBlob 只计算 blob ID，不落盘。
func hashBlob(data []byte) string {
	raw := encodeObject(TypeBlob, data)
	return hexEncode(raw[len(raw)-32:])
}

// StatusKind 描述单条工作区状态。
type StatusKind string

// 状态分类：staged-* 表示相对 HEAD 的暂存差异，
// worktree-* 表示相对索引的未暂存差异，untracked 表示未跟踪。
const (
	StatusAdded            StatusKind = "staged-add"
	StatusModified         StatusKind = "staged-modify"
	StatusDeleted          StatusKind = "staged-delete"
	StatusWorktreeModified StatusKind = "worktree-modify"
	StatusWorktreeDeleted  StatusKind = "worktree-delete"
	StatusUntracked        StatusKind = "untracked"
)

// StatusEntry 是一条路径级状态。
type StatusEntry struct {
	Path string
	Kind StatusKind
}

// headTreeMap 返回 HEAD 提交的扁平树；未出生分支返回空映射。
func (r *Repository) headTreeMap() (BlobMap, error) {
	id, err := r.HeadCommit()
	if err != nil {
		if errors.Is(err, ErrRefNotFound) {
			return BlobMap{}, nil
		}
		return nil, err
	}
	c, err := r.ReadCommit(id)
	if err != nil {
		return nil, err
	}
	return r.flattenTree(c.Tree)
}

// Status 计算工作区状态（判定依据完全来自 HEAD 树、索引与工作区内容）。
func (r *Repository) Status() ([]StatusEntry, error) {
	return r.statusLocked()
}

func (r *Repository) statusLocked() ([]StatusEntry, error) {
	head, err := r.headTreeMap()
	if err != nil {
		return nil, err
	}
	idxEntries, err := r.readIndex()
	if err != nil {
		return nil, err
	}
	idx := indexAsMap(idxEntries)
	work, err := r.readWorkdir()
	if err != nil {
		return nil, err
	}

	var out []StatusEntry
	pathSeen := map[string]bool{}
	add := func(p string, k StatusKind) {
		pathSeen[p] = true
		out = append(out, StatusEntry{Path: p, Kind: k})
	}

	// 索引相对 HEAD：暂存差异。
	for _, p := range unionKeys(head, idx) {
		hb, inHead := head[p]
		ib, inIdx := idx[p]
		switch {
		case !inHead && inIdx:
			add(p, StatusAdded)
		case inHead && !inIdx:
			add(p, StatusDeleted)
		case inHead && inIdx && hb != ib:
			add(p, StatusModified)
		}
	}

	// 工作区相对索引：未暂存差异与未跟踪文件。
	tracked := map[string]bool{}
	for _, e := range idxEntries {
		tracked[e.Path] = true
	}
	for p := range head {
		tracked[p] = true
	}
	for _, p := range unionPaths(idx, work) {
		ib, inIdx := idx[p]
		wb, inWork := work[p]
		switch {
		case inIdx && !inWork:
			if !pathSeen[p] {
				add(p, StatusWorktreeDeleted)
			}
		case inIdx && inWork && ib != hashBlob(wb):
			if !pathSeen[p] {
				add(p, StatusWorktreeModified)
			}
		case !inIdx && inWork && !tracked[p]:
			add(p, StatusUntracked)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func unionPaths(a map[string]string, b map[string][]byte) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unionKeys 返回两个映射键的并集（排序）。
func unionKeys[V any](maps ...map[string]V) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasPrefixPath(p, prefix string) bool {
	if p == prefix {
		return true
	}
	return len(p) > len(prefix)+1 && p[:len(prefix)+1] == prefix+"/"
}

// Add 暂存指定路径（相对工作区）；支持目录前缀与 "."。
// 文件已删除时从索引移除。
func (r *Repository) Add(paths []string) error {
	return r.withLock(func() error {
		idxEntries, err := r.readIndex()
		if err != nil {
			return err
		}
		idx := indexAsMap(idxEntries)
		work, err := r.readWorkdir()
		if err != nil {
			return err
		}
		all := false
		for _, p := range paths {
			if p == "." {
				all = true
			}
		}
		match := func(p string) bool {
			if all {
				return true
			}
			for _, pat := range paths {
				if p == pat || hasPrefixPath(p, pat) {
					return true
				}
			}
			return false
		}
		changed := false
		for p, data := range work {
			if match(p) {
				id, err := r.writeBlob(data)
				if err != nil {
					return err
				}
				if idx[p] != id {
					idx[p] = id
					changed = true
				}
			}
		}
		for p := range idx {
			if match(p) {
				if _, ok := work[p]; !ok {
					delete(idx, p)
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}
		entries := make([]IndexEntry, 0, len(idx))
		for p, b := range idx {
			entries = append(entries, IndexEntry{Path: p, Blob: b})
		}
		return r.writeIndexAtomic(entries)
	})
}

// CommitOptions 控制提交元数据。
type CommitOptions struct {
	Message string
	Author  Signature
	// When 为零值时使用当前时间；测试可注入固定时钟保证可复现。
	When time.Time
}

// Commit 基于当前索引创建提交并原子更新当前分支或分离 HEAD。
func (r *Repository) Commit(opts CommitOptions) (string, error) {
	var newID string
	err := r.withLock(func() error {
		idxEntries, err := r.readIndex()
		if err != nil {
			return err
		}
		files := BlobMap{}
		for _, e := range idxEntries {
			files[e.Path] = e.Blob
		}
		treeID, err := r.buildTree(files)
		if err != nil {
			return err
		}
		if opts.Author.Name == "" {
			opts.Author.Name = "default"
		}
		c := &Commit{Tree: treeID, Message: opts.Message, Author: opts.Author, Committer: opts.Author}
		if !opts.When.IsZero() {
			c.Author.When = opts.When
			c.Committer.When = opts.When
		} else {
			c.Author.When = time.Now()
			c.Committer.When = c.Author.When
		}
		if headID, err := r.HeadCommit(); err == nil {
			c.Parents = []string{headID}
		} else if !errors.Is(err, ErrRefNotFound) {
			return err
		}
		newID, err = r.writeCommit(c)
		if err != nil {
			return err
		}
		return r.updateHEADTo(newID)
	})
	return newID, err
}

// updateHEADTo 把 HEAD（符号引用或分离头）原子推进到 id。
func (r *Repository) updateHEADTo(id string) error {
	name, detached, err := r.readHEADRaw()
	if err != nil {
		return err
	}
	if detached != "" {
		return r.writeHEADDetached(id)
	}
	return r.writeRefAtomic(name, id)
}

// Branch 基于当前 HEAD 创建新分支。
func (r *Repository) Branch(name string) (string, error) {
	var id string
	err := r.withLock(func() error {
		if err := validateRefName(name); err != nil {
			return err
		}
		if r.refExists(name) {
			return fmt.Errorf("vcs: branch %q already exists", name)
		}
		head, err := r.HeadCommit()
		if err != nil {
			return err
		}
		id = head
		return r.writeRefAtomic(name, id)
	})
	return id, err
}

// DeleteBranch 删除分支；禁止删除当前检出分支。
func (r *Repository) DeleteBranch(name string) error {
	return r.withLock(func() error {
		if err := validateRefName(name); err != nil {
			return err
		}
		cur, err := r.CurrentBranch()
		if err != nil {
			return err
		}
		if cur == name {
			return fmt.Errorf("vcs: cannot delete checked-out branch %q", name)
		}
		if _, err := r.readRef(name); err != nil {
			return err
		}
		return r.deleteRef(name)
	})
}

// ListBranches 返回全部分支名与当前分支名。
func (r *Repository) ListBranches() ([]string, string, error) {
	cur, err := r.CurrentBranch()
	if err != nil {
		return nil, "", err
	}
	names, err := r.listRefs()
	return names, cur, err
}

// Set 是字符串集合。
type Set map[string]bool

func blobMapKeys(m BlobMap) Set {
	s := Set{}
	for k := range m {
		s[k] = true
	}
	return s
}

// UnsafeOverwriteError 表示切换/合并被拒绝：本地改动会被覆盖。
type UnsafeOverwriteError struct {
	Paths []string
}

func (e *UnsafeOverwriteError) Error() string {
	return fmt.Sprintf("vcs: refusing operation; local changes would be overwritten: %v", e.Paths)
}

// checkSafeUpdate 判定从 old 快照切换到 target 是否会覆盖本地改动。
//
// 规则：
//   - 已跟踪路径在 old 与 target 间有差异时，工作区文件内容必须等于 old
//     （本地无修改）或等于 target（结果相同），否则拒绝；
//   - old 中没有、target 中新增的路径若在工作区作为未跟踪文件已存在，拒绝。
func checkSafeUpdate(old, target BlobMap, work FileSet, tracked Set) []string {
	var blocked []string
	for _, p := range unionKeys(old, target) {
		o, inOld := old[p]
		t, inTarget := target[p]
		if inOld == inTarget && o == t {
			continue
		}
		w, inWork := work[p]
		switch {
		case !inOld && inTarget:
			if inWork && !tracked[p] {
				blocked = append(blocked, p)
			} else if inWork && tracked[p] && hashBlob(w) != t {
				blocked = append(blocked, p)
			}
		case inOld && !inTarget:
			if inWork && hashBlob(w) != o {
				blocked = append(blocked, p)
			}
		default:
			if !inWork {
				blocked = append(blocked, p)
			} else if wid := hashBlob(w); wid != o && wid != t {
				blocked = append(blocked, p)
			}
		}
	}
	sort.Strings(blocked)
	return blocked
}

// Checkout 切换到已有分支；会覆盖本地改动时明确拒绝。
func (r *Repository) Checkout(branch string) error {
	return r.withLock(func() error {
		id, err := r.readRef(branch)
		if err != nil {
			return err
		}
		return r.switchToCommit(id, false, branch)
	})
}

// checkoutDetach 切换到裸提交（分离 HEAD），供 rebase 等内部流程使用。
func (r *Repository) checkoutDetach(id string) error {
	return r.withLock(func() error {
		if _, err := r.ReadCommit(id); err != nil {
			return err
		}
		return r.switchToCommit(id, true, "")
	})
}

// detachToLocked 在已持有仓库锁时切换到裸提交。
func (r *Repository) detachToLocked(id string) error {
	if _, err := r.ReadCommit(id); err != nil {
		return err
	}
	return r.switchToCommit(id, true, "")
}

func (r *Repository) switchToCommit(id string, detach bool, branch string) error {
	c, err := r.ReadCommit(id)
	if err != nil {
		return err
	}
	target, err := r.flattenTree(c.Tree)
	if err != nil {
		return err
	}
	old, err := r.headTreeMap()
	if err != nil {
		return err
	}
	work, err := r.readWorkdir()
	if err != nil {
		return err
	}
	tracked := blobMapKeys(old)
	if idxEntries, idxErr := r.readIndex(); idxErr == nil {
		for _, e := range idxEntries {
			tracked[e.Path] = true
		}
	}
	if blocked := checkSafeUpdate(old, target, work, tracked); len(blocked) > 0 {
		return &UnsafeOverwriteError{Paths: blocked}
	}
	if err := r.materialize(target, old); err != nil {
		return err
	}
	entries := make([]IndexEntry, 0, len(target))
	for p, b := range target {
		entries = append(entries, IndexEntry{Path: p, Blob: b})
	}
	if err := r.writeIndexAtomic(entries); err != nil {
		return err
	}
	if detach {
		return r.writeHEADDetached(id)
	}
	// HEAD 在工作区/索引全部落定后再切换，保证失败时 HEAD 仍指向旧分支。
	return r.writeHEADSymbolic(branch)
}
