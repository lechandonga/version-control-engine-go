package vcs

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// 三方合并按路径对 blob 做内容级合并：
//   - 三方相同 / 任一侧未改动：取对应内容；
//   - 双方都改且结果一致：取一致结果；
//   - 双方都改且不一致：冲突，现场写入 merge-state/，返回 *MergeConflict。

// MergeResult 为快进合并成功时的结果信息。
type MergeResult struct {
	Mode     string // "fast-forward" 或 "merge-commit"
	CommitID string
}

// Merge 把 other 合并进当前分支。冲突时保留 merge-state 现场并返回
// *MergeConflict；无冲突时产生合并提交（可快进则快进）并留痕。
func (r *Repo) Merge(other, message string) (*MergeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.rebaseInProgress(); err == nil {
		return nil, ErrRebaseInProgress
	}
	if _, err := r.mergeInProgress(); err == nil {
		return nil, ErrMergeInProgress
	}

	headID, err := r.headTarget()
	if err != nil {
		return nil, err
	}
	otherID, err := r.ResolveBranch(other)
	if err != nil {
		return nil, err
	}
	base, err := r.mergeBase(headID, otherID)
	if err != nil {
		return nil, err
	}
	headTree, err := r.commitTree(headID)
	if err != nil {
		return nil, err
	}
	otherTree, err := r.commitTree(otherID)
	if err != nil {
		return nil, err
	}
	baseTree, err := r.commitTree(base)
	if err != nil {
		return nil, err
	}
	merged, conflicts, err := r.mergeTrees(baseTree, headTree, otherTree)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		if err := r.writeMergeState(mergeState{
			Head:      headID,
			Other:     otherID,
			OtherName: other,
			Base:      base,
			Conflicts: conflicts,
		}); err != nil {
			return nil, err
		}
		return nil, &MergeConflict{Paths: conflicts}
	}

	old := headID
	mode := "merge-commit"
	finalID := otherID
	if base == headID {
		mode = "fast-forward"
	} else {
		entries := flatFromMerged(merged)
		treeID, err := r.indexToTree(entries)
		if err != nil {
			return nil, err
		}
		if message == "" {
			message = "merge branch " + other
		}
		finalID, err = r.writeCommit(Commit{
			Tree:    treeID,
			Parents: []string{headID, otherID},
			Message: message,
		})
		if err != nil {
			return nil, err
		}
	}
	if err := r.advanceAfterMerge(old, finalID, "merge "+other+": "+mode); err != nil {
		return nil, err
	}
	return &MergeResult{Mode: mode, CommitID: finalID}, nil
}

func (r *Repo) advanceAfterMerge(old, newID, reason string) error {
	if err := r.appendReflog("HEAD", OpMerge, old, newID, reason); err != nil {
		return err
	}
	if ref, symbolic := r.headSymbolicRef(); symbolic {
		name := strings.TrimPrefix(ref, "refs/heads/")
		if err := r.writeBranchPointer(name, newID); err != nil {
			return err
		}
		if err := r.appendReflog(ref, OpMerge, old, newID, reason); err != nil {
			return err
		}
	} else {
		if err := r.detachHEAD(newID); err != nil {
			return err
		}
	}
	finalCommit, err := r.readCommit(newID)
	if err != nil {
		return err
	}
	entries, err := r.treeFlat(finalCommit.Tree)
	if err != nil {
		return err
	}
	if err := r.replaceWorktree(indexMap(entries), r.workdir()); err != nil {
		return err
	}
	return r.writeIndex(entries)
}

func (r *Repo) commitTree(commitID string) (string, error) {
	c, err := r.readCommit(commitID)
	if err != nil {
		return "", err
	}
	return c.Tree, nil
}

// mergeBase 取最近共同祖先；无共同祖先返回 *UnrelatedHistories。
func (r *Repo) mergeBase(a, b string) (string, error) {
	anc := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if anc[id] {
			continue
		}
		anc[id] = true
		c, err := r.readCommit(id)
		if err != nil {
			return "", err
		}
		stack = append(stack, c.Parents...)
	}
	stack = []string{b}
	visited := map[string]bool{}
	var found string
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		if anc[id] {
			found = id
			break
		}
		c, err := r.readCommit(id)
		if err != nil {
			return "", err
		}
		stack = append(stack, c.Parents...)
	}
	if found == "" {
		return "", &UnrelatedHistories{A: a, B: b}
	}
	return found, nil
}

type mergedFile struct {
	id string
}

// mergeTrees 在扁平路径空间上做三方合并（本引擎无目录内容概念，
// 路径即文件标识）。
func (r *Repo) mergeTrees(baseTree, oursTree, theirsTree string) (map[string]string, []ConflictEntry, error) {
	var base map[string]string
	var err error
	if baseTree != "" {
		base, err = r.flatMap(baseTree)
		if err != nil {
			return nil, nil, err
		}
	} else {
		base = map[string]string{}
	}
	ours, err := r.flatMap(oursTree)
	if err != nil {
		return nil, nil, err
	}
	theirs, err := r.flatMap(theirsTree)
	if err != nil {
		return nil, nil, err
	}
	paths := map[string]bool{}
	for _, m := range []map[string]string{base, ours, theirs} {
		for p := range m {
			paths[p] = true
		}
	}
	out := map[string]string{}
	var conflicts []ConflictEntry
	for p := range paths {
		b, bo := base[p]
		o, oo := ours[p]
		t, to := theirs[p]
		switch {
		case o == t:
			// 双方一致（含同时删除）。
			if oo && to {
				out[p] = o
			}
		case b == o:
			// 只有对方改了。
			if to {
				out[p] = t
			}
		case b == t:
			// 只有自己改了。
			if oo {
				out[p] = o
			}
		default:
			// 双方相对共同祖先都做了不同修改：删除/修改的任意组合
			// 只要结果不一致就报冲突，禁止静默选择某一方。
			conflicts = append(conflicts, ConflictEntry{
				Path: p, Base: optionalID(b, bo), Ours: optionalID(o, oo), Theirs: optionalID(t, to),
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return out, conflicts, nil
}

func optionalID(id string, ok bool) string {
	if !ok {
		return ""
	}
	return id
}

func (r *Repo) flatMap(treeID string) (map[string]string, error) {
	entries, err := r.treeFlat(treeID)
	if err != nil {
		return nil, err
	}
	return indexMap(entries), nil
}

func flatFromMerged(m map[string]string) []IndexEntry {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]IndexEntry, 0, len(paths))
	for _, p := range paths {
		out = append(out, IndexEntry{Mode: "100644", ID: m[p], Path: p})
	}
	return out
}

// ResolveConflict 用给定文件内容消解一个冲突路径并加入暂存区。
func (r *Repo) ResolveConflict(path string, content []byte) error {
	st, err := r.mergeInProgress()
	if err != nil {
		return err
	}
	remaining := st.Conflicts[:0]
	found := false
	for _, c := range st.Conflicts {
		if c.Path == path {
			found = true
			continue
		}
		remaining = append(remaining, c)
	}
	if !found {
		return errors.New("not a conflicted path: " + path)
	}
	st.Conflicts = remaining
	id, err := r.putObject(ObjectBlob, content)
	if err != nil {
		return err
	}
	entries, err := r.readIndex()
	if err != nil {
		return err
	}
	entries = append(entries, IndexEntry{Mode: "100644", ID: id, Path: path})
	if err := r.writeIndex(entries); err != nil {
		return err
	}
	if len(remaining) == 0 {
		return r.finishMerge(*st)
	}
	return r.writeMergeState(*st)
}

func (r *Repo) finishMerge(st mergeState) error {
	entries, err := r.readIndex()
	if err != nil {
		return err
	}
	treeID, err := r.indexToTree(entries)
	if err != nil {
		return err
	}
	id, err := r.writeCommit(Commit{
		Tree:    treeID,
		Parents: []string{st.Head, st.Other},
		Message: "merge " + st.OtherName,
	})
	if err != nil {
		return err
	}
	if err := r.advanceAfterMerge(st.Head, id, "merge "+st.OtherName+": resolved"); err != nil {
		return err
	}
	return r.clearMergeState()
}

// AbortMerge 放弃在途合并，现场清理，HEAD 与留痕回到合并前。
func (r *Repo) AbortMerge() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.mergeInProgress()
	if err != nil {
		return err
	}
	cur, _ := r.headTarget()
	if err := r.appendReflog("HEAD", OpRebaseAbort, cur, st.Head, "merge aborted"); err != nil {
		return err
	}
	if err := r.resetToCommit(st.Head); err != nil {
		return err
	}
	return r.clearMergeState()
}

func (r *Repo) resetToCommit(commitID string) error {
	c, err := r.readCommit(commitID)
	if err != nil {
		return err
	}
	entries, err := r.treeFlat(c.Tree)
	if err != nil {
		return err
	}
	if err := r.replaceWorktree(indexMap(entries), r.workdir()); err != nil {
		return err
	}
	if err := r.writeIndex(entries); err != nil {
		return err
	}
	if ref, symbolic := r.headSymbolicRef(); symbolic {
		name := strings.TrimPrefix(ref, "refs/heads/")
		return r.writeBranchPointer(name, commitID)
	}
	return r.detachHEAD(commitID)
}

// Conflicts 返回在途合并的冲突列表（无在途合并时返回 ErrNoMerge）。
func (r *Repo) Conflicts() ([]ConflictEntry, error) {
	st, err := r.mergeInProgress()
	if err != nil {
		return nil, err
	}
	return st.Conflicts, nil
}

// merge-state 持久化（JSON，整体原子写）。

type mergeState struct {
	Head      string          `json:"head"`
	Other     string          `json:"other"`
	OtherName string          `json:"other_name"`
	Base      string          `json:"base"`
	Conflicts []ConflictEntry `json:"conflicts"`
}

func (r *Repo) mergeStatePath() string { return filepath.Join(r.root, "merge-state", "state.json") }

func (r *Repo) writeMergeState(st mergeState) error {
	return writeJSONAtomic(r.mergeStatePath(), st)
}

func (r *Repo) mergeInProgress() (*mergeState, error) {
	var st mergeState
	if err := readJSONFile(r.mergeStatePath(), &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoMerge
		}
		return nil, err
	}
	return &st, nil
}

func (r *Repo) clearMergeState() error {
	return removeIfExists(r.mergeStatePath())
}
