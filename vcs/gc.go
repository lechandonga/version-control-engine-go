package vcs

import (
	"os"
	"sort"
	"time"
)

// GCOptions 控制回收行为。
type GCOptions struct {
	DryRun bool          // 只预览，不删除
	Grace  time.Duration // 保留期：更年轻的对象一律保留
}

// GCResult 描述一次回收的结果。
type GCResult struct {
	Reachable int      // 可达对象数（松散）
	Removed   []string // 被删除（或预览将删除）的对象
	Kept      []string // 不可达但因保留期保留的对象
}

// Reachable 计算可达对象集合。可达根包括：
// 全部分支引用、HEAD 当前位置、在途重放现场、在途合并现场、操作记录中的位置。
func (r *Repo) Reachable() (map[string]bool, error) {
	roots := map[string]bool{}
	refs, err := r.ListRefs()
	if err != nil {
		return nil, err
	}
	for _, id := range refs {
		roots[id] = true
	}
	if head, err := r.CurrentCommit(); err != nil {
		return nil, err
	} else if head != "" {
		roots[head] = true
	}
	if st, err := r.ReadRebaseState(); err != nil {
		return nil, err
	} else if st != nil {
		for _, id := range append([]string{st.Onto, st.Original, st.Current}, st.Remaining...) {
			if isHexID(id) {
				roots[id] = true
			}
		}
	}
	if st, err := r.readMergeState(); err != nil {
		return nil, err
	} else if st != nil {
		for _, id := range []string{st.Theirs, st.Base} {
			if isHexID(id) {
				roots[id] = true
			}
		}
	}
	logIDs, err := r.reflogRootIDs()
	if err != nil {
		return nil, err
	}
	for id := range logIDs {
		roots[id] = true
	}
	// 从根出发遍历 commit -> tree -> blob。
	reach := map[string]bool{}
	queue := make([]string, 0, len(roots))
	for id := range roots {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if reach[id] || !isHexID(id) {
			continue
		}
		typ, payload, err := r.ReadObject(id)
		if err != nil {
			if _, ok := err.(*ObjectMissing); ok {
				continue // 根指向的对象本就不存在（如空仓库日志），忽略
			}
			return nil, err
		}
		reach[id] = true
		switch typ {
		case TypeCommit:
			c, err := r.ReadCommit(id)
			if err != nil {
				return nil, err
			}
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case TypeTree:
			t, err := r.ReadTree(id)
			if err != nil {
				return nil, err
			}
			for _, e := range t.Entries {
				queue = append(queue, e.ID)
			}
		}
		_ = payload
	}
	return reach, nil
}

// GC 回收不可达且超出保留期的松散对象。
// 中断可重入：每次运行都重新计算可达性，逐个文件删除，重试不会误删。
func (r *Repo) GC(opts GCOptions) (*GCResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reach, err := r.Reachable()
	if err != nil {
		return nil, err
	}
	loose, err := r.listLoose()
	if err != nil {
		return nil, err
	}
	res := &GCResult{}
	now := r.Now()
	for _, id := range loose {
		if reach[id] {
			res.Reachable++
			continue
		}
		if opts.Grace > 0 {
			if st, err := os.Stat(r.loosePath(id)); err == nil {
				if now.Sub(st.ModTime()) < opts.Grace {
					res.Kept = append(res.Kept, id)
					continue
				}
			}
		}
		res.Removed = append(res.Removed, id)
		if !opts.DryRun {
			os.Remove(r.loosePath(id)) // 单个删除失败可下次重试，不中断整体
		}
	}
	sort.Strings(res.Removed)
	sort.Strings(res.Kept)
	return res, nil
}
