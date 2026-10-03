package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 不可达回收规则：
//
//  1. 可达根（roots）包括：
//     - 所有分支引用（refs/heads/**）
//     - 当前检出位置 HEAD（分离 HEAD 时其指向也是独立根）
//     - 在途合并现场 merge-state（head/other/base 与冲突三方 blob）
//     - 在途重放现场 rebase-state（onto/original/queue/applied/
//       stopped 提交及暂停冲突的三方 blob）
//     - 操作留痕 logs/reflog 中出现的全部对象 id（旧位置 + 新位置），
//       保证“照记录恢复”永远能找回对象
//  2. 从根沿 commit -> tree -> blob 闭包遍历得到可达集合。
//  3. 其余对象为候选垃圾；仅删除“对象文件 mtime 早于 now-保留期”的
//     松散对象，以及从归档中重写剔除的不可达对象。保留期默认 14 天。
//  4. 断点 / 幂等：先写 gc/plan.json（含 mtime 指纹），删除阶段逐条
//     核对对象仍不可达且未变年轻；归档重写复用 Pack 的临时文件 +
//     rename 发布。崩溃后重跑会重新扫描、重新判定，不会因旧计划误删。

// GCOptions 控制回收行为。
type GCOptions struct {
	// Retain 为新对象保留期；零值取 DefaultRetention。
	Retain time.Duration
	// Now 仅用于测试注入；零值取当前时间。
	Now time.Time
}

// DefaultRetention 为默认的新生对象保留期。
const DefaultRetention = 14 * 24 * time.Hour

// GCReport 为一次回收（预览或执行）的结果。
type GCReport struct {
	Reachable   int
	Unreachable int
	// Reclaimable 为满足保留期、真正可清理的对象。
	Reclaimable []string
	// TooYoung 为不可达但仍在保留期内的对象。
	TooYoung       []string
	LooseDeleted   int
	PacksRewritten int
	// Roots 列出判定依据（根类型 -> id），供日志核对。
	Roots map[string][]string
}

type gcPlan struct {
	CreatedAt   time.Time `json:"created_at"`
	RetainUntil time.Time `json:"retain_until"`
	Reclaimable []string  `json:"reclaimable"`
	// Fingerprints 记录计划时刻每个待删松散对象的 mtime；执行前核对。
	Fingerprints map[string]time.Time `json:"fingerprints"`
}

// PreviewGC 只计算可达性与清理清单，不做任何删除。
func (r *Repo) PreviewGC(opts GCOptions) (*GCReport, error) {
	opts = normalizeGCOpts(opts)
	lock := newFileLock(r.root, "gc.lock")
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	return r.computeGC(opts)
}

// GC 先预览、确认无误后执行回收；返回最终报告。
func (r *Repo) GC(opts GCOptions) (*GCReport, error) {
	opts = normalizeGCOpts(opts)
	lock := newFileLock(r.root, "gc.lock")
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	report, err := r.computeGC(opts)
	if err != nil {
		return nil, err
	}
	if len(report.Reclaimable) == 0 {
		return report, nil
	}

	// 阶段 1：落计划（含 mtime 指纹）。
	plan := gcPlan{
		CreatedAt:    opts.Now,
		RetainUntil:  opts.Now.Add(-opts.Retain),
		Reclaimable:  append([]string(nil), report.Reclaimable...),
		Fingerprints: map[string]time.Time{},
	}
	for _, id := range report.Reclaimable {
		if fi, err := os.Stat(objectLoosePath(r.root, id)); err == nil {
			plan.Fingerprints[id] = fi.ModTime()
		}
	}
	if err := writeJSONAtomic(r.gcPlanPath(), plan); err != nil {
		return nil, err
	}

	// 阶段 2：删除松散垃圾。执行前重新判定，确保中途写入不会被误删。
	fresh, err := r.computeGC(opts)
	if err != nil {
		return nil, err
	}
	freshReclaim := map[string]bool{}
	for _, id := range fresh.Reclaimable {
		freshReclaim[id] = true
	}
	for _, id := range plan.Reclaimable {
		if !freshReclaim[id] {
			continue // 已重新可达 / 变年轻 / 已被删，跳过
		}
		if mt, ok := plan.Fingerprints[id]; ok {
			if fi, err := os.Stat(objectLoosePath(r.root, id)); err == nil && !fi.ModTime().Equal(mt) {
				continue // 文件在计划后变化（典型为并发重写），保守保留
			}
		}
		if err := os.Remove(objectLoosePath(r.root, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		report.LooseDeleted++
	}
	if err := r.removeEmptyObjectDirs(); err != nil {
		return nil, err
	}

	// 阶段 3：重写归档，剔除不可达且已过保留期的对象。
	reclaimSet := map[string]bool{}
	for _, id := range report.Reclaimable {
		reclaimSet[id] = true
	}
	rewritten, err := r.rewritePacks(reclaimSet)
	if err != nil {
		return nil, err
	}
	report.PacksRewritten = rewritten

	_ = removeIfExists(r.gcPlanPath())
	return report, nil
}

func normalizeGCOpts(o GCOptions) GCOptions {
	if o.Retain <= 0 {
		o.Retain = DefaultRetention
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	return o
}

func (r *Repo) gcPlanPath() string { return filepath.Join(r.root, "gc", "plan.json") }

func (r *Repo) computeGC(opts GCOptions) (*GCReport, error) {
	if err := r.refreshPacks(); err != nil {
		return nil, err
	}
	roots, err := r.collectRoots()
	if err != nil {
		return nil, err
	}
	reachable, err := r.walkReachable(roots)
	if err != nil {
		return nil, err
	}
	all, err := r.listAllObjects()
	if err != nil {
		return nil, err
	}
	cutoff := opts.Now.Add(-opts.Retain)
	rep := &GCReport{Reachable: len(reachable), Roots: roots}
	for _, id := range all {
		if reachable[id] {
			continue
		}
		rep.Unreachable++
		fi, err := os.Stat(objectLoosePath(r.root, id))
		if err == nil {
			if fi.ModTime().Before(cutoff) {
				rep.Reclaimable = append(rep.Reclaimable, id)
			} else {
				rep.TooYoung = append(rep.TooYoung, id)
			}
			continue
		}
		// 对象只存在于归档中：保留期按计划时刻无法看 mtime，
		// 归档对象视为“已冷却”，允许随归档重写剔除。
		rep.Reclaimable = append(rep.Reclaimable, id)
	}
	sort.Strings(rep.Reclaimable)
	sort.Strings(rep.TooYoung)
	return rep, nil
}

func (r *Repo) listAllObjects() ([]string, error) {
	loose, err := r.listLooseObjects()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var all []string
	add := func(id string) {
		if isHexID(id) && !seen[id] {
			seen[id] = true
			all = append(all, id)
		}
	}
	for _, id := range loose {
		add(id)
	}
	r.packsMu.RLock()
	packs := append([]*packFile(nil), r.packs...)
	r.packsMu.RUnlock()
	for _, p := range packs {
		for id := range p.idx {
			add(id)
		}
	}
	sort.Strings(all)
	return all, nil
}

func (r *Repo) collectRoots() (map[string][]string, error) {
	roots := map[string]map[string]bool{
		"branch": {},
		"head":   {},
		"merge":  {},
		"rebase": {},
		"reflog": {},
	}
	add := func(kind, id string) {
		if isHexID(id) {
			roots[kind][id] = true
		}
	}
	names, err := r.Branches()
	if err == nil {
		for _, n := range names {
			if id, err := r.ResolveBranch(n); err == nil {
				add("branch", id)
			}
		}
	}
	if id, err := r.headTarget(); err == nil {
		add("head", id)
	}
	if st, err := r.mergeInProgress(); err == nil {
		add("merge", st.Head)
		add("merge", st.Other)
		add("merge", st.Base)
		for _, c := range st.Conflicts {
			add("merge", c.Base)
			add("merge", c.Ours)
			add("merge", c.Theirs)
		}
	}
	if st, err := r.rebaseInProgress(); err == nil {
		add("rebase", st.Onto)
		add("rebase", st.OriginalHead)
		for _, id := range st.Queue {
			add("rebase", id)
		}
		for _, id := range st.Applied {
			add("rebase", id)
		}
		for _, c := range st.StoppedConflicts {
			add("rebase", c.Base)
			add("rebase", c.Ours)
			add("rebase", c.Theirs)
		}
	}
	entries, err := r.ReadReflog("", time.Time{}, time.Time{})
	if err == nil {
		for _, e := range entries {
			add("reflog", e.Old)
			add("reflog", e.New)
		}
	}
	out := make(map[string][]string, len(roots))
	for kind, set := range roots {
		for id := range set {
			out[kind] = append(out[kind], id)
		}
		sort.Strings(out[kind])
	}
	return out, nil
}

func (r *Repo) walkReachable(roots map[string][]string) (map[string]bool, error) {
	reachable := map[string]bool{}
	var stack []string
	for _, ids := range roots {
		stack = append(stack, ids...)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reachable[id] || !isHexID(id) {
			continue
		}
		t, _, err := r.readObject(id)
		if err != nil {
			// 根引用了缺失对象：向上抛出，避免“因为读不到所以当垃圾”。
			return nil, err
		}
		reachable[id] = true
		switch t {
		case ObjectCommit:
			c, cerr := r.readCommit(id)
			if cerr != nil {
				return nil, cerr
			}
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case ObjectTree:
			entries, terr := r.readTree(id)
			if terr != nil {
				return nil, terr
			}
			for _, e := range entries {
				stack = append(stack, e.ID)
			}
		}
	}
	return reachable, nil
}
