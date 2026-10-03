package vcs

import (
	"errors"
	"strings"
)

// 重放（rebase）现场持久化在 rebase-state/state.json：
//
//	{"onto":...,"original_head":...,"branch":...,"queue":[待重放提交...],
//	 "applied":[新提交...],"stopped_commit":"冲突暂停提交或空"}

type rebaseState struct {
	Onto         string   `json:"onto"`
	OriginalHead string   `json:"original_head"`
	Branch       string   `json:"branch"`
	Queue        []string `json:"queue"`
	Applied      []string `json:"applied"`
	// StoppedCommit 非空表示暂停在该提交的冲突上。
	StoppedCommit    string          `json:"stopped_commit,omitempty"`
	StoppedConflicts []ConflictEntry `json:"stopped_conflicts,omitempty"`
}

// Rebase 把当前分支相对 upstream 独有的提交，按原顺序在 upstream 上重放。
func (r *Repo) Rebase(upstream string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.rebaseInProgress(); err == nil {
		return ErrRebaseInProgress
	}
	if _, err := r.mergeInProgress(); err == nil {
		return ErrMergeInProgress
	}
	onto, err := r.ResolveBranch(upstream)
	if err != nil {
		return err
	}
	headID, err := r.headTarget()
	if err != nil {
		return err
	}
	base, err := r.mergeBase(headID, onto)
	if err != nil {
		return err
	}
	queue, err := r.commitsBetween(base, headID)
	if err != nil {
		return err
	}
	branch := "HEAD"
	if ref, symbolic := r.headSymbolicRef(); symbolic {
		branch = ref
	}
	st := &rebaseState{Onto: onto, OriginalHead: headID, Branch: branch, Queue: queue}
	// 若存在崩溃后残留的旧重放，其 OriginalHead 可能已失效；以当前分支
	// 实际指向为准，保证 Abort 回到“本次重放前”的位置。
	_ = r.clearRebaseState()
	if err := r.appendReflog("HEAD", OpRebaseAdvance, headID, onto,
		"rebase start onto "+shortID(onto)); err != nil {
		return err
	}
	if err := r.detachHEAD(onto); err != nil {
		return err
	}
	if err := r.resetWorktreeToCommit(onto); err != nil {
		return err
	}
	if err := r.writeRebaseState(st); err != nil {
		return err
	}
	return r.continueRebaseLocked(st)
}

// commitsBetween 返回 base 之后、head 及以下的提交（不含 base），旧 -> 新。
func (r *Repo) commitsBetween(base, head string) ([]string, error) {
	var chain []string
	cur := head
	for cur != base {
		chain = append(chain, cur)
		c, err := r.readCommit(cur)
		if err != nil {
			return nil, err
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

func (r *Repo) resetWorktreeToCommit(commitID string) error {
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
	return r.writeIndex(entries)
}

// rebaseOne 在 current 上重放 commitID；冲突时返回 (空ID, conflicts)。
func (r *Repo) rebaseOne(current, commitID string) (string, []ConflictEntry, error) {
	c, err := r.readCommit(commitID)
	if err != nil {
		return "", nil, err
	}
	baseTree := ""
	if len(c.Parents) > 0 {
		pc, perr := r.readCommit(c.Parents[0])
		if perr != nil {
			return "", nil, perr
		}
		baseTree = pc.Tree
	}
	currentTree, err := r.commitTree(current)
	if err != nil {
		return "", nil, err
	}
	// ours=被重放提交、theirs=onto(current)，冲突视角与 RebaseConflict 一致。
	merged, conflicts, err := r.mergeTrees(baseTree, c.Tree, currentTree)
	if err != nil {
		return "", nil, err
	}
	if len(conflicts) > 0 {
		// 重放视角：被重放提交为 ours，onto 方向为 theirs。
		for i := range conflicts {
			conflicts[i].Ours, conflicts[i].Theirs = conflicts[i].Theirs, conflicts[i].Ours
		}
		return "", conflicts, nil
	}
	entries := flatFromMerged(merged)
	treeID, err := r.indexToTree(entries)
	if err != nil {
		return "", nil, err
	}
	newID, err := r.writeCommit(Commit{
		Tree:      treeID,
		Parents:   []string{current},
		Author:    c.Author,
		Timestamp: c.Timestamp,
		Message:   c.Message,
	})
	return newID, nil, err
}

func (r *Repo) continueRebaseLocked(st *rebaseState) error {
	for len(st.Queue) > 0 {
		current := st.Onto
		if n := len(st.Applied); n > 0 {
			current = st.Applied[n-1]
		}
		commitID := st.Queue[0]
		newID, conflicts, err := r.rebaseOne(current, commitID)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			st.StoppedCommit = commitID
			st.StoppedConflicts = conflicts
			// 冲突路径必须从暂存区移除：只有显式 Resolve 后才重新入暂存，
			// 这样“未消解就 Continue”才能稳定再次报冲突。
			staged, _ := r.readIndex()
			kept := staged[:0]
			conflictSet := map[string]bool{}
			for _, c := range conflicts {
				conflictSet[c.Path] = true
			}
			for _, e := range staged {
				if !conflictSet[e.Path] {
					kept = append(kept, e)
				}
			}
			if err := r.writeIndex(kept); err != nil {
				return err
			}
			if err := r.writeRebaseState(st); err != nil {
				return err
			}
			if err := r.appendReflog("HEAD", OpRebasePause, current, current,
				"rebase paused at "+shortID(commitID)); err != nil {
				return err
			}
			return &RebaseConflict{Commit: commitID, Paths: conflicts}
		}
		if err := r.appendReflog("HEAD", OpRebaseAdvance, current, newID,
			"rebase replay "+shortID(commitID)); err != nil {
			return err
		}
		if err := r.detachHEAD(newID); err != nil {
			return err
		}
		st.Applied = append(st.Applied, newID)
		st.Queue = st.Queue[1:]
		st.StoppedCommit = ""
		st.StoppedConflicts = nil
		if err := r.resetWorktreeToCommit(newID); err != nil {
			return err
		}
		// 每重放一个提交就落盘现场，崩溃后可从下一提交续跑。
		if err := r.writeRebaseState(st); err != nil {
			return err
		}
	}
	return r.finishRebase(st)
}

func (r *Repo) finishRebase(st *rebaseState) error {
	final := st.Onto
	if n := len(st.Applied); n > 0 {
		final = st.Applied[n-1]
	}
	old := st.OriginalHead
	if st.Branch != "HEAD" {
		name := strings.TrimPrefix(st.Branch, "refs/heads/")
		if err := r.appendReflog(st.Branch, OpRebaseAdvance, old, final, "rebase finished"); err != nil {
			return err
		}
		if err := r.writeBranchPointer(name, final); err != nil {
			return err
		}
		if err := r.writeHEADSymbolic(st.Branch); err != nil {
			return err
		}
	} else {
		if err := r.detachHEAD(final); err != nil {
			return err
		}
	}
	if err := r.appendReflog("HEAD", OpRebaseAdvance, old, final, "rebase finished"); err != nil {
		return err
	}
	return r.clearRebaseState()
}

// ContinueRebase 在冲突暂停后继续；调用方应已消解全部冲突。
func (r *Repo) ContinueRebase() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.rebaseInProgress()
	if err != nil {
		return err
	}
	if st.StoppedCommit == "" {
		return r.continueRebaseLocked(st)
	}
	var remaining []ConflictEntry
	staged, err := r.readIndex()
	if err != nil {
		return err
	}
	stagedM := indexMap(staged)
	for _, c := range st.StoppedConflicts {
		if _, ok := stagedM[c.Path]; !ok {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) > 0 {
		return &RebaseConflict{Commit: st.StoppedCommit, Paths: remaining}
	}
	treeID, err := r.indexToTree(staged)
	if err != nil {
		return err
	}
	c, err := r.readCommit(st.StoppedCommit)
	if err != nil {
		return err
	}
	current := st.Onto
	if n := len(st.Applied); n > 0 {
		current = st.Applied[n-1]
	}
	newID, err := r.writeCommit(Commit{
		Tree:      treeID,
		Parents:   []string{current},
		Author:    c.Author,
		Timestamp: c.Timestamp,
		Message:   c.Message,
	})
	if err != nil {
		return err
	}
	if len(st.Queue) == 0 || st.Queue[0] != st.StoppedCommit {
		return &ObjectCorrupt{Msg: "rebase queue out of sync with stopped commit"}
	}
	if err := r.appendReflog("HEAD", OpRebaseAdvance, current, newID,
		"rebase replay (resolved) "+shortID(st.StoppedCommit)); err != nil {
		return err
	}
	st.Applied = append(st.Applied, newID)
	st.Queue = st.Queue[1:]
	st.StoppedCommit = ""
	st.StoppedConflicts = nil
	if err := r.detachHEAD(newID); err != nil {
		return err
	}
	if err := r.resetWorktreeToCommit(newID); err != nil {
		return err
	}
	if err := r.writeRebaseState(st); err != nil {
		return err
	}
	return r.continueRebaseLocked(st)
}

// ResolveRebaseConflict 用给定内容消解一个重放冲突并暂存。
func (r *Repo) ResolveRebaseConflict(path string, content []byte) error {
	st, err := r.rebaseInProgress()
	if err != nil {
		return err
	}
	if st.StoppedCommit == "" {
		return ErrNoRebase
	}
	found := false
	for _, c := range st.StoppedConflicts {
		if c.Path == path {
			found = true
		}
	}
	if !found {
		return errors.New("not a rebase-conflicted path: " + path)
	}
	id, err := r.putObject(ObjectBlob, content)
	if err != nil {
		return err
	}
	entries, err := r.readIndex()
	if err != nil {
		return err
	}
	entries = append(entries, IndexEntry{Mode: "100644", ID: id, Path: path})
	return r.writeIndex(entries)
}

// SkipRebaseCommit 跳过当前暂停的提交。
func (r *Repo) SkipRebaseCommit() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.rebaseInProgress()
	if err != nil {
		return err
	}
	if st.StoppedCommit == "" || len(st.Queue) == 0 || st.Queue[0] != st.StoppedCommit {
		return ErrNoRebase
	}
	current := st.Onto
	if n := len(st.Applied); n > 0 {
		current = st.Applied[n-1]
	}
	if err := r.appendReflog("HEAD", OpRebasePause, current, current,
		"rebase skipped "+shortID(st.StoppedCommit)); err != nil {
		return err
	}
	st.Queue = st.Queue[1:]
	st.StoppedCommit = ""
	st.StoppedConflicts = nil
	return r.continueRebaseLocked(st)
}

// AbortRebase 整体回退一次重放：分支/HEAD 与工作区回到重放前并留痕。
func (r *Repo) AbortRebase() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.rebaseInProgress()
	if err != nil {
		return err
	}
	cur, _ := r.headTarget()
	if err := r.appendReflog("HEAD", OpRebaseAbort, cur, st.OriginalHead, "rebase aborted"); err != nil {
		return err
	}
	if st.Branch != "HEAD" {
		name := strings.TrimPrefix(st.Branch, "refs/heads/")
		if err := r.writeBranchPointer(name, st.OriginalHead); err != nil {
			return err
		}
		if err := r.writeHEADSymbolic(st.Branch); err != nil {
			return err
		}
		if err := r.appendReflog(st.Branch, OpRebaseAbort, cur, st.OriginalHead, "rebase aborted"); err != nil {
			return err
		}
	} else if err := r.detachHEAD(st.OriginalHead); err != nil {
		return err
	}
	if err := r.resetWorktreeToCommit(st.OriginalHead); err != nil {
		return err
	}
	return r.clearRebaseState()
}

// RebaseStatus 描述在途重放的进度。
type RebaseStatus struct {
	Onto          string
	OriginalHead  string
	Queued        int
	Applied       int
	StoppedCommit string
	Conflicts     []ConflictEntry
}

// RebaseInProgress 返回在途重放状态；无在途重放返回 ErrNoRebase。
func (r *Repo) RebaseInProgress() (*RebaseStatus, error) {
	st, err := r.rebaseInProgress()
	if err != nil {
		return nil, err
	}
	return &RebaseStatus{
		Onto:          st.Onto,
		OriginalHead:  st.OriginalHead,
		Queued:        len(st.Queue),
		Applied:       len(st.Applied),
		StoppedCommit: st.StoppedCommit,
		Conflicts:     st.StoppedConflicts,
	}, nil
}
