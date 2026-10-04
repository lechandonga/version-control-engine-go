package vcs

import (
	"os"
	"path/filepath"
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
	Reachable int      // 可达对象数（松散 + 归档）
	Removed   []string // 被删除（或预览将删除）的对象（含已归档对象）
	Kept      []string // 不可达但因保留期保留的对象
}

// listCandidates 枚举可参与回收判定的全部对象：
// 松散对象 + 可正常解析的归档包条目。损坏归档包中的条目不参与回收，
// 保证坏数据永远不会因为维护操作被静默丢掉。
func (r *Repo) listCandidates() ([]string, map[string]time.Time, error) {
	ages := map[string]time.Time{}
	loose, err := r.listLoose()
	if err != nil {
		return nil, nil, err
	}
	for _, id := range loose {
		p := r.loosePath(id)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		// 损坏/篡改的松散对象不参与回收：无法证明其身份时绝不删除。
		if data, err := os.ReadFile(p); err == nil {
			if _, _, err := decodeObject(id, data); err != nil {
				continue
			}
			ages[id] = st.ModTime()
		}
	}
	for _, pf := range r.getPacks() {
		if pf.loadErr != nil {
			continue // 损坏的归档包整体保留，不参与回收
		}
		st, err := os.Stat(pf.path)
		if err != nil {
			continue
		}
		for id := range pf.index {
			if _, ok := ages[id]; !ok {
				ages[id] = st.ModTime() // 包内对象年龄以归档时间为准
			}
		}
	}
	ids := make([]string, 0, len(ages))
	for id := range ages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, ages, nil
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
			// 对象不存在（空仓库日志）或所在归档包损坏：跳过遍历该分支。
			// 安全侧原则——读不出就不标记可达，而损坏包/坏松散对象不参与回收，不会误删。
			continue
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

// GC 回收不可达且超出保留期的对象，不论对象存放在松散文件还是归档包中：
// 松散对象直接删除；归档对象通过“写新包再原子替换旧包”的方式剔除，
// 被剔除的对象在新包落盘前始终可读。
// 中断可重入：每次运行都重新计算可达性，崩溃后重试不会误删，结果幂等稳定。
func (r *Repo) GC(opts GCOptions) (*GCResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reach, err := r.Reachable()
	if err != nil {
		return nil, err
	}
	candidates, ages, err := r.listCandidates()
	if err != nil {
		return nil, err
	}
	res := &GCResult{}
	now := r.Now()
	remove := map[string]bool{}
	for _, id := range candidates {
		if reach[id] {
			res.Reachable++
			continue
		}
		if opts.Grace > 0 {
			if mt, ok := ages[id]; ok && now.Sub(mt) < opts.Grace {
				res.Kept = append(res.Kept, id)
				continue
			}
		}
		res.Removed = append(res.Removed, id)
		remove[id] = true
	}
	sort.Strings(res.Removed)
	sort.Strings(res.Kept)
	if opts.DryRun || len(remove) == 0 {
		return res, nil
	}
	// 先重写归档包（原子替换），再删除松散对象：
	// 任何时刻对象只要仍存在就保持可读，崩溃后重试结果一致。
	if err := r.rewritePacks(remove); err != nil {
		return res, err
	}
	for id := range remove {
		if _, err := os.Stat(r.loosePath(id)); err == nil {
			if err := os.Remove(r.loosePath(id)); err != nil {
				return res, err // 下次重试补删，不影响已完成的包重写
			}
		}
	}
	// 清理空的分片目录。
	if subs, err := os.ReadDir(r.objectsDir()); err == nil {
		for _, s := range subs {
			if s.IsDir() {
				os.Remove(filepath.Join(r.objectsDir(), s.Name()))
			}
		}
	}
	return res, nil
}
