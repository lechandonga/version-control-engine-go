package vcs

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// RebaseState 是在途重放现场，落盘保存：既是断点续做的依据，也是回收的可达根。
type RebaseState struct {
	Onto      string   `json:"onto"`
	Original  string   `json:"original"`
	Current   string   `json:"current"`
	Remaining []string `json:"remaining"`
}

func (r *Repo) rebaseStatePath() string { return filepath.Join(r.VCS, "REBASE_STATE") }

// ReadRebaseState 返回在途重放现场；没有在途重放时返回 nil。
func (r *Repo) ReadRebaseState() (*RebaseState, error) {
	data, err := os.ReadFile(r.rebaseStatePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st RebaseState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, &ObjectCorrupt{ID: "REBASE_STATE", Msg: "bad encoding"}
	}
	return &st, nil
}

func (r *Repo) writeRebaseState(st *RebaseState) error {
	data, _ := json.Marshal(st)
	return writeFileAtomic(r.rebaseStatePath(), data)
}

// commitsToReplay 返回需要从 onto 重放到 head 的提交（最旧在前）。
func (r *Repo) commitsToReplay(onto, head string) ([]string, error) {
	anc, err := r.ancestors(onto)
	if err != nil {
		return nil, err
	}
	var rev []string
	cur := head
	for cur != "" && !anc[cur] {
		rev = append(rev, cur)
		c, err := r.ReadCommit(cur)
		if err != nil {
			return nil, err
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// Rebase 把当前分支在 onto 之后的提交逐个重放到 onto 上。
// 冲突时保留现场并返回 RebaseConflict，可 RebaseContinue 继续或 RebaseAbort 回退。
func (r *Repo) Rebase(ontoTarget string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, err := r.ReadRebaseState(); err != nil {
		return err
	} else if st != nil {
		return ErrRebaseInProgress
	}
	onto, err := r.resolveTarget(ontoTarget)
	if err != nil {
		return err
	}
	head, err := r.CurrentCommit()
	if err != nil {
		return err
	}
	todo, err := r.commitsToReplay(onto, head)
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	st := &RebaseState{Onto: onto, Original: head, Remaining: todo}
	if err := r.writeRebaseState(st); err != nil {
		return err
	}
	r.logMoveLocked("rebase", "HEAD", head, onto, "rebase start onto "+ontoTarget)
	return r.replayLocked(st)
}

// replayLocked 逐个重放剩余提交。调用方需持有 r.mu。
func (r *Repo) replayLocked(st *RebaseState) error {
	head := st.Onto
	if cur, err := r.CurrentCommit(); err != nil {
		return err
	} else if cur != "" && cur != st.Original {
		head = cur // 续做时从当前位置继续
	}
	for len(st.Remaining) > 0 {
		cid := st.Remaining[0]
		c, err := r.ReadCommit(cid)
		if err != nil {
			return err
		}
		var baseTree map[string]string
		if len(c.Parents) > 0 {
			baseTree, err = r.commitTree(c.Parents[0])
		} else {
			baseTree = map[string]string{}
		}
		if err != nil {
			return err
		}
		ourTree, err := r.commitTree(head)
		if err != nil {
			return err
		}
		theirTree, err := r.commitTree(cid)
		if err != nil {
			return err
		}
		merged, conflicts := mergeTrees(baseTree, ourTree, theirTree)
		if len(conflicts) > 0 {
			st.Current = cid
			if err := r.writeRebaseState(st); err != nil {
				return err
			}
			// 把当前重放位置落到工作区，便于用户就地修改后继续。
			if err := r.checkoutTreeLocked(head); err != nil {
				return err
			}
			if err := r.updateHeadLocked(head, "rebase", "rebase pause at "+shortID(cid)); err != nil {
				return err
			}
			return &RebaseConflict{Commit: cid, Paths: conflicts}
		}
		treeID, err := r.buildTree(merged)
		if err != nil {
			return err
		}
		nc := &Commit{
			Tree:    treeID,
			Parents: []string{head},
			Message: c.Message,
			Author:  c.Author,
			Time:    r.Now().UTC().Format(timeRFC),
		}
		data, _ := json.Marshal(nc)
		newID, err := r.WriteObject(TypeCommit, data)
		if err != nil {
			return err
		}
		head = newID
		st.Remaining = st.Remaining[1:]
		if err := r.writeRebaseState(st); err != nil {
			return err
		}
	}
	// 全部重放完成：移动分支指针并落工作区。
	if err := r.checkoutTreeLocked(head); err != nil {
		return err
	}
	if err := r.updateHeadLocked(head, "rebase", "rebase finished"); err != nil {
		return err
	}
	return os.Remove(r.rebaseStatePath())
}

// RebaseContinue 用当前工作区内容完成冲突中的提交，并继续重放。
func (r *Repo) RebaseContinue() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.ReadRebaseState()
	if err != nil {
		return err
	}
	if st == nil {
		return ErrNoRebase
	}
	// 用当前工作区快照完成当前冲突提交。
	treeID, err := r.snapshot()
	if err != nil {
		return err
	}
	head, err := r.CurrentCommit()
	if err != nil {
		return err
	}
	c, err := r.ReadCommit(st.Current)
	if err != nil {
		return err
	}
	nc := &Commit{
		Tree:    treeID,
		Parents: []string{head},
		Message: c.Message,
		Author:  c.Author,
		Time:    r.Now().UTC().Format(timeRFC),
	}
	data, _ := json.Marshal(nc)
	newID, err := r.WriteObject(TypeCommit, data)
	if err != nil {
		return err
	}
	if err := r.updateHeadLocked(newID, "rebase", "rebase continue"); err != nil {
		return err
	}
	st.Remaining = st.Remaining[1:]
	st.Current = ""
	if err := r.writeRebaseState(st); err != nil {
		return err
	}
	return r.replayLocked(st)
}

// RebaseAbort 回退整个重放，把分支恢复到重放前位置。
func (r *Repo) RebaseAbort() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.ReadRebaseState()
	if err != nil {
		return err
	}
	if st == nil {
		return ErrNoRebase
	}
	if err := r.checkoutTreeLocked(st.Original); err != nil {
		return err
	}
	if err := r.updateHeadLocked(st.Original, "rebase", "rebase abort"); err != nil {
		return err
	}
	return os.Remove(r.rebaseStatePath())
}
