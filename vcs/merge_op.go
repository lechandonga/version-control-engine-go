package vcs

import (
	"errors"
	"fmt"
	"time"
)

// MergeResult 描述一次合并的结果类型。
type MergeResult struct {
	Kind        string // "fast-forward" | "merge-commit" | "up-to-date"
	CommitID    string // 合并后 HEAD 提交
	FastForward bool
}

// Merge 合并给定分支到当前分支。
//
//   - 当前已是对方祖先：快进（如实快进，不造合并提交）
//   - 双方同一提交：up-to-date
//   - 真正分叉：按确定性虚拟基做三方合并；无冲突则生成双亲合并提交
//
// 会覆盖本地未提交修改/未跟踪文件时，与 Checkout 同样拒绝。
// 冲突时不改动 HEAD 与分支引用，冲突信息通过 MergeConflictError 返回。
func (r *Repository) Merge(branch string, opts CommitOptions) (*MergeResult, error) {
	var result *MergeResult
	err := r.withLock(func() error {
		theirID, err := r.readRef(branch)
		if err != nil {
			return err
		}
		ourID, err := r.HeadCommit()
		if err != nil {
			return err
		}
		if ourID == theirID {
			result = &MergeResult{Kind: "up-to-date", CommitID: ourID}
			return nil
		}
		ourAncestor, err := r.IsAncestor(ourID, theirID)
		if err != nil {
			return err
		}
		theirAncestor, err := r.IsAncestor(theirID, ourID)
		if err != nil {
			return err
		}

		ourCommit, err := r.ReadCommit(ourID)
		if err != nil {
			return err
		}
		ours, err := r.flattenTree(ourCommit.Tree)
		if err != nil {
			return err
		}
		theirCommit, err := r.ReadCommit(theirID)
		if err != nil {
			return err
		}
		theirs, err := r.flattenTree(theirCommit.Tree)
		if err != nil {
			return err
		}

		if theirAncestor {
			result = &MergeResult{Kind: "up-to-date", CommitID: ourID}
			return nil
		}

		// 快进：ours 是 theirs 的祖先。
		if ourAncestor {
			if err := r.guardSwitch(ours, theirs); err != nil {
				return err
			}
			if err := r.applyMergedTree(theirs, ours); err != nil {
				return err
			}
			if err := r.updateHEADTo(theirID); err != nil {
				return err
			}
			result = &MergeResult{Kind: "fast-forward", CommitID: theirID, FastForward: true}
			return nil
		}

		// 真正分叉：多共同祖先 -> 确定性虚拟基。
		bases, err := r.MergeBases(ourID, theirID)
		if err != nil {
			return err
		}
		base, err := r.virtualBase(bases)
		if err != nil {
			return err
		}
		merged, conflicts := threeWayMerge(base, ours, theirs)
		if len(conflicts) > 0 {
			return &MergeConflictError{Conflicts: conflicts}
		}
		if err := r.guardSwitch(ours, merged); err != nil {
			return err
		}
		treeID, err := r.buildTree(merged)
		if err != nil {
			return err
		}
		author := opts.Author
		if author.Name == "" {
			author.Name = "default"
		}
		c := &Commit{
			Tree:      treeID,
			Parents:   []string{ourID, theirID},
			Author:    author,
			Committer: author,
			Message:   opts.Message,
		}
		when := opts.When
		if when.IsZero() {
			when = time.Now()
		}
		c.Author.When = when
		c.Committer.When = when
		newID, err := r.writeCommit(c)
		if err != nil {
			return err
		}
		if err := r.applyMergedTree(merged, ours); err != nil {
			return err
		}
		if err := r.updateHEADTo(newID); err != nil {
			return err
		}
		result = &MergeResult{Kind: "merge-commit", CommitID: newID}
		return nil
	})
	return result, err
}

// guardSwitch 检查把工作区从 old 更新到 target 是否会覆盖本地内容。
func (r *Repository) guardSwitch(old, target BlobMap) error {
	work, err := r.readWorkdir()
	if err != nil {
		return err
	}
	tracked := Set{}
	if idxEntries, idxErr := r.readIndex(); idxErr == nil {
		for _, e := range idxEntries {
			tracked[e.Path] = true
		}
	}
	for p := range old {
		tracked[p] = true
	}
	if blocked := checkSafeUpdate(old, target, work, tracked); len(blocked) > 0 {
		return &UnsafeOverwriteError{Paths: blocked}
	}
	return nil
}

// applyMergedTree 落地合并结果并同步索引（HEAD 由调用方更新）。
func (r *Repository) applyMergedTree(target, old BlobMap) error {
	if err := r.materialize(target, old); err != nil {
		return err
	}
	entries := make([]IndexEntry, 0, len(target))
	for p, b := range target {
		entries = append(entries, IndexEntry{Path: p, Blob: b})
	}
	return r.writeIndexAtomic(entries)
}

// explainBases 供测试/日志输出判定依据。
func (r *Repository) explainBases(a, b string) (string, error) {
	bases, err := r.MergeBases(a, b)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("merge-bases(%s,%s)=%v", shortID(a), shortID(b), shortIDs(bases)), nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func shortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shortID(id)
	}
	return out
}

// IsMergeConflict 判断错误是否为合并冲突。
func IsMergeConflict(err error) bool {
	var e *MergeConflictError
	return errors.As(err, &e)
}
