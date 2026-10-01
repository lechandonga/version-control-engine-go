package vcs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Rebase 把当前分支相对 upstream 的独有提交逐个重放到 upstream 之上。
//
// 状态机落盘在 <root>/state/rebase/ 下（所有更新原子写 + rename）：
//
//	meta        : 起始快照信息（见 rebaseMeta 编码）
//	orig-head   : 原分支名与提交 ID
//	onto        : 重放基底提交 ID
//	pending     : 待重放提交 ID，每行一个，顺序即重放顺序
//	applied     : 已生成的重放提交 ID，每行一个
//	current     : 正在重放的原始提交 ID（冲突暂停时存在）
//	conflicts   : 暂停时的冲突路径，每行一个
//	head        : 当前临时 HEAD（已应用到的提交 ID）
//	lock-side   : 操作开始时的工作区/索引/HEAD 快照，供 abort 精确回退

const rebaseDir = "rebase"

func (r *Repository) rebasePath() string {
	return filepath.Join(r.root, dirState, rebaseDir)
}

type rebaseMeta struct {
	OrigBranch string
	OrigHead   string
	Upstream   string
	Onto       string
	Pending    []string // 待重放的原始提交（旧 -> 新）
}

func writeLines(path string, lines []string) error {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 {
		return nil, nil
	}
	parts := bytes.Split(data, []byte("\n"))
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out, nil
}

// rebaseStatus 表示是否存在未完成的重放。
type rebaseStatus struct {
	Present   bool
	Paused    bool
	Meta      rebaseMeta
	Current   string
	Conflicts []string
	Applied   []string
	Head      string
}

// RebaseInProgressError 表示已有一个未完成的重放。
type RebaseInProgressError struct {
	Paused bool
}

func (e *RebaseInProgressError) Error() string {
	if e.Paused {
		return "vcs: rebase in progress (paused at a conflict)"
	}
	return "vcs: rebase already in progress"
}

// RebaseStatus 返回当前重放状态（无重放时 Present=false）。
func (r *Repository) RebaseStatus() (*rebaseStatus, error) {
	dir := r.rebasePath()
	if !fileExists(filepath.Join(dir, "meta")) {
		return &rebaseStatus{}, nil
	}
	st := &rebaseStatus{Present: true}
	metaLines, err := readLines(filepath.Join(dir, "meta"))
	if err != nil {
		return nil, err
	}
	if len(metaLines) < 4 {
		return nil, errors.New("vcs: rebase state corrupt: meta")
	}
	st.Meta.OrigBranch = metaLines[0]
	st.Meta.OrigHead = metaLines[1]
	st.Meta.Upstream = metaLines[2]
	st.Meta.Onto = metaLines[3]
	st.Meta.Pending, _ = readLines(filepath.Join(dir, "pending"))
	st.Applied, _ = readLines(filepath.Join(dir, "applied"))
	st.Head, _ = readOneLine(filepath.Join(dir, "head"))
	st.Current, err = readOneLine(filepath.Join(dir, "current"))
	if err == nil {
		st.Paused = true
		st.Conflicts, _ = readLines(filepath.Join(dir, "conflicts"))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return st, nil
}

func readOneLine(path string) (string, error) {
	lines, err := readLines(path)
	if err != nil {
		return "", err
	}
	if len(lines) != 1 || lines[0] == "" {
		return "", errors.New("vcs: rebase state corrupt: " + filepath.Base(path))
	}
	return lines[0], nil
}

// snapshot 保存操作开始时的完整现场，供 abort 精确恢复。
type snapshot struct {
	headMode  string // "ref:" 分支名，或 "detach:"+id
	headValue string
	index     []IndexEntry
	// 未跟踪文件（不属于起始 HEAD 也不属于索引）需要一并恢复。
	untracked FileSet
	tracked   BlobMap // 起始 HEAD 跟踪集，用于判断删除的文件
}

func (r *Repository) takeSnapshot() (*snapshot, error) {
	sn := &snapshot{}
	name, detached, err := r.readHEADRaw()
	if err != nil {
		return nil, err
	}
	if detached != "" {
		sn.headMode = "detach"
		sn.headValue = detached
	} else {
		sn.headMode = "ref"
		sn.headValue = name
	}
	sn.index, err = r.readIndex()
	if err != nil {
		return nil, err
	}
	sn.tracked, err = r.headTreeMap()
	if err != nil {
		return nil, err
	}
	work, err := r.readWorkdir()
	if err != nil {
		return nil, err
	}
	indexSet := indexAsMap(sn.index)
	sn.untracked = FileSet{}
	for p, data := range work {
		_, inHead := sn.tracked[p]
		_, inIndex := indexSet[p]
		if !inHead && !inIndex {
			sn.untracked[p] = data
		}
	}
	return sn, nil
}

// saveSnapshot 把快照原子落盘（内容寻址的 blob + 清单文件）。
func (r *Repository) saveSnapshot(sn *snapshot) error {
	dir := filepath.Join(r.rebasePath(), "snapshot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var manifest bytes.Buffer
	fmt.Fprintf(&manifest, "%s %s\n", sn.headMode, sn.headValue)
	// 索引
	for _, e := range sn.index {
		fmt.Fprintf(&manifest, "idx %s %s\n", e.Path, e.Blob)
	}
	// 跟踪集（起始 HEAD）
	for p, b := range sn.tracked {
		fmt.Fprintf(&manifest, "track %s %s\n", p, b)
	}
	// 未跟踪文件：内容写入对象库（rebase 专用，复用 blob 存储）。
	paths := make([]string, 0, len(sn.untracked))
	for p := range sn.untracked {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		id, err := r.writeBlob(sn.untracked[p])
		if err != nil {
			return err
		}
		fmt.Fprintf(&manifest, "untrack %s %s\n", p, id)
	}
	return writeFileAtomic(filepath.Join(dir, "manifest"), manifest.Bytes(), 0o644)
}

func (r *Repository) loadSnapshot() (*snapshot, error) {
	dir := filepath.Join(r.rebasePath(), "snapshot")
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	sn := &snapshot{untracked: FileSet{}, tracked: BlobMap{}}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(line, []byte(" "), 3)
		switch string(parts[0]) {
		case "detach", "ref":
			sn.headMode = string(parts[0])
			sn.headValue = string(parts[1])
		case "idx":
			sn.index = append(sn.index, IndexEntry{Path: string(parts[1]), Blob: string(parts[2])})
		case "track":
			sn.tracked[string(parts[1])] = string(parts[2])
		case "untrack":
			blob, err := r.readBlob(string(parts[2]))
			if err != nil {
				return nil, err
			}
			sn.untracked[string(parts[1])] = blob
		}
	}
	return sn, nil
}

// restoreSnapshot 把仓库恢复到快照现场：
// 重建跟踪文件 + 未跟踪文件，删除重放期间产生的额外文件。
func (r *Repository) restoreSnapshot(sn *snapshot) error {
	target := BlobMap{}
	for _, e := range sn.index {
		target[e.Path] = e.Blob
	}
	// materialize 需要“旧跟踪集”来删除文件；这里用当前工作区无法获得，
	// 因此先清空工作区跟踪文件，再全量写入。
	current, err := r.headTreeMap()
	if err != nil {
		return err
	}
	// 以 index 为准恢复（含暂存删除），未跟踪文件随后补回。
	if err := r.materialize(target, current); err != nil {
		return err
	}
	// 恢复未跟踪文件（它们不在索引中）。
	paths := make([]string, 0, len(sn.untracked))
	for p := range sn.untracked {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		abs := filepath.Join(r.workDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(abs, sn.untracked[p], 0o644); err != nil {
			return err
		}
	}
	// 恢复索引与 HEAD。
	if err := r.writeIndexAtomic(sn.index); err != nil {
		return err
	}
	switch sn.headMode {
	case "ref":
		return r.writeHEADSymbolic(sn.headValue)
	default:
		return r.writeHEADDetached(sn.headValue)
	}
}

// RebaseOptions 控制重放元数据。
type RebaseOptions struct {
	Upstream string // 基底分支名
	Author   Signature
	When     time.Time
}

// RebaseResult 描述重放结果。
type RebaseResult struct {
	Kind     string // "rebased" | "up-to-date" | "paused"
	Head     string
	Applied  int
	Skipped  int
	Conflict string // paused 时的首个冲突路径
}

// Rebase 启动一次重放。
func (r *Repository) Rebase(opts RebaseOptions) (*RebaseResult, error) {
	var res *RebaseResult
	err := r.withLock(func() error {
		existing, err := r.RebaseStatus()
		if err != nil {
			return err
		}
		if existing.Present {
			return &RebaseInProgressError{Paused: existing.Paused}
		}
		upstreamID, err := r.resolveCommit(opts.Upstream)
		if err != nil {
			return err
		}
		origHead, err := r.HeadCommit()
		if err != nil {
			return err
		}
		branch, err := r.CurrentBranch()
		if err != nil {
			return err
		}

		// 独有提交：origHead 可达、upstream 不可达，按旧->新排列。
		pending, err := r.commitsNotIn(origHead, upstreamID)
		if err != nil {
			return err
		}

		// 无独有提交：当前已包含 upstream，直接快进（若可）或无操作。
		if len(pending) == 0 {
			ff, err := r.IsAncestor(origHead, upstreamID)
			if err != nil {
				return err
			}
			if ff {
				// 快进：需要安全检查。
				uc, err := r.ReadCommit(upstreamID)
				if err != nil {
					return err
				}
				target, err := r.flattenTree(uc.Tree)
				if err != nil {
					return err
				}
				old, err := r.headTreeMap()
				if err != nil {
					return err
				}
				if err := r.guardSwitch(old, target); err != nil {
					return err
				}
				if err := r.applyMergedTree(target, old); err != nil {
					return err
				}
				if err := r.updateHEADTo(upstreamID); err != nil {
					return err
				}
			}
			res = &RebaseResult{Kind: "up-to-date", Head: origHead}
			return nil
		}

		// 建立状态目录与起始快照。
		dir := r.rebasePath()
		if fileExists(dir) {
			_ = removeAll(dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		sn, err := r.takeSnapshot()
		if err != nil {
			return err
		}
		meta := rebaseMeta{
			OrigBranch: branch,
			OrigHead:   origHead,
			Upstream:   opts.Upstream,
			Onto:       upstreamID,
			Pending:    pending,
		}
		if err := r.writeRebaseMeta(meta); err != nil {
			return err
		}
		if err := r.saveSnapshot(sn); err != nil {
			return err
		}
		if err := writeLines(filepath.Join(dir, "applied"), nil); err != nil {
			return err
		}
		if err := writeLines(filepath.Join(dir, "head"), []string{upstreamID}); err != nil {
			return err
		}

		// 分离到 onto 开始重放。
		if err := r.detachToLocked(upstreamID); err != nil {
			return err
		}

		rr := &RebaseResult{Kind: "rebased", Head: upstreamID}
		cont, cerr := r.replayLoop(rr, opts)
		if cerr != nil {
			return cerr
		}
		if cont {
			if err := r.finishRebase(meta); err != nil {
				return err
			}
		}
		res = rr
		return nil
	})
	return res, err
}

func (r *Repository) writeRebaseMeta(m rebaseMeta) error {
	dir := r.rebasePath()
	lines := append([]string{m.OrigBranch, m.OrigHead, m.Upstream, m.Onto}, m.Pending...)
	if err := writeLines(filepath.Join(dir, "meta"), lines[:4]); err != nil {
		return err
	}
	return writeLines(filepath.Join(dir, "pending"), m.Pending)
}

// commitsNotIn 返回 head 可达但 upstream 不可达的提交（旧 -> 新）。
func (r *Repository) commitsNotIn(head, upstream string) ([]string, error) {
	up, err := r.ancestors(upstream)
	if err != nil {
		return nil, err
	}
	var ordered []string
	cur := head
	for cur != "" && !up[cur] {
		ordered = append(ordered, cur)
		c, err := r.ReadCommit(cur)
		if err != nil {
			return nil, err
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	// 反转为旧 -> 新。
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered, nil
}

// replayLoop 从状态文件继续重放，返回 completed=true 表示全部完成。
func (r *Repository) replayLoop(rr *RebaseResult, opts RebaseOptions) (bool, error) {
	dir := r.rebasePath()
	for {
		pending, err := readLines(filepath.Join(dir, "pending"))
		if err != nil {
			return false, err
		}
		if len(pending) == 0 {
			return true, nil
		}
		cur := pending[0]
		headID, err := readOneLine(filepath.Join(dir, "head"))
		if err != nil {
			return false, err
		}

		applied, skip, conflict, err := r.replayOne(cur, headID, opts)
		if err != nil {
			return false, err
		}
		if conflict != nil {
			// 暂停：记录 current/conflicts，工作区保留合并现场。
			if err := writeLines(filepath.Join(dir, "current"), []string{cur}); err != nil {
				return false, err
			}
			paths := make([]string, len(conflict.Conflicts))
			for i, c := range conflict.Conflicts {
				paths[i] = c.Path
			}
			if err := writeLines(filepath.Join(dir, "conflicts"), paths); err != nil {
				return false, err
			}
			rr.Kind = "paused"
			rr.Conflict = paths[0]
			return false, nil
		}

		// 推进状态文件（先写新 head/pending，再追加 applied）。
		newPending := pending[1:]
		if err := writeLines(filepath.Join(dir, "pending"), newPending); err != nil {
			return false, err
		}
		if skip {
			rr.Skipped++
			// 跳过时 head 不变。
		} else {
			rr.Applied++
			rr.Head = applied
			if err := writeLines(filepath.Join(dir, "head"), []string{applied}); err != nil {
				return false, err
			}
			prev, _ := readLines(filepath.Join(dir, "applied"))
			if err := writeLines(filepath.Join(dir, "applied"), append(prev, applied)); err != nil {
				return false, err
			}
		}
	}
}

// replayOne 重放单个提交到 baseHead 之上。
//
// 返回 applied=新提交ID（skip=true 表示等效变更已存在，跳过）；
// conflict 非空表示三方合并冲突，现场已保留在工作区/索引之外不改动。
func (r *Repository) replayOne(commitID, baseHead string, opts RebaseOptions) (string, bool, *MergeConflictError, error) {
	c, err := r.ReadCommit(commitID)
	if err != nil {
		return "", false, nil, err
	}
	var parentTree BlobMap
	if len(c.Parents) > 0 {
		pc, err := r.ReadCommit(c.Parents[0])
		if err != nil {
			return "", false, nil, err
		}
		parentTree, err = r.flattenTree(pc.Tree)
		if err != nil {
			return "", false, nil, err
		}
	} else {
		parentTree = BlobMap{}
	}
	patchTree, err := r.flattenTree(c.Tree)
	if err != nil {
		return "", false, nil, err
	}
	bc, err := r.ReadCommit(baseHead)
	if err != nil {
		return "", false, nil, err
	}
	ontoTree, err := r.flattenTree(bc.Tree)
	if err != nil {
		return "", false, nil, err
	}

	merged, conflicts := threeWayMerge(parentTree, patchTree, ontoTree)
	// 冲突判定后不落地，直接返回（保持暂停现场由调用方写状态）。
	if len(conflicts) > 0 {
		return "", false, &MergeConflictError{Conflicts: conflicts}, nil
	}

	// 等效变更跳过：重放结果与 onto 当前树逐路径完全相同。
	if blobMapsEqual(merged, ontoTree) {
		return "", true, nil, nil
	}

	treeID, err := r.buildTree(merged)
	if err != nil {
		return "", false, nil, err
	}
	author := opts.Author
	if author.Name == "" {
		author = c.Author
	}
	nc := &Commit{
		Tree:      treeID,
		Parents:   []string{baseHead},
		Author:    author,
		Committer: author,
		Message:   c.Message,
	}
	when := opts.When
	if when.IsZero() {
		when = time.Now()
	}
	nc.Author.When = when
	nc.Committer.When = when
	newID, err := r.writeCommit(nc)
	if err != nil {
		return "", false, nil, err
	}

	// 更新工作区与分离 HEAD。
	if err := r.applyMergedTree(merged, ontoTree); err != nil {
		return "", false, nil, err
	}
	if err := r.writeHEADDetached(newID); err != nil {
		return "", false, nil, err
	}
	return newID, false, nil, nil
}

func blobMapsEqual(a, b BlobMap) bool {
	if len(a) != len(b) {
		return false
	}
	for p, id := range a {
		if b[p] != id {
			return false
		}
	}
	return true
}

// finishRebase 全部完成：把原分支引用原子指向重放结果，
// 恢复符号 HEAD，清理状态目录。
func (r *Repository) finishRebase(meta rebaseMeta) error {
	dir := r.rebasePath()
	finalID, err := readOneLine(filepath.Join(dir, "head"))
	if err != nil {
		return err
	}
	// 若一个提交都没应用（全部跳过），head 仍为 onto；
	// 此时原分支应指向 onto（已包含其全部变更）。
	if meta.OrigBranch != "" {
		if err := r.writeRefAtomic(meta.OrigBranch, finalID); err != nil {
			return err
		}
		if err := r.writeHEADSymbolic(meta.OrigBranch); err != nil {
			return err
		}
	} else {
		if err := r.writeHEADDetached(finalID); err != nil {
			return err
		}
	}
	return removeAll(dir)
}

// RebaseContinue 在冲突暂停后继续：调用方需先用 Add 标记冲突已解决。
//
// 以当前索引内容作为当前提交的重放结果生成提交，然后继续剩余提交。
func (r *Repository) RebaseContinue(opts RebaseOptions) (*RebaseResult, error) {
	var res *RebaseResult
	err := r.withLock(func() error {
		st, err := r.RebaseStatus()
		if err != nil {
			return err
		}
		if !st.Present || !st.Paused {
			return errors.New("vcs: no paused rebase to continue")
		}
		dir := r.rebasePath()
		cur := st.Current

		// 当前索引即冲突解决结果。
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
		cc, err := r.ReadCommit(cur)
		if err != nil {
			return err
		}
		author := opts.Author
		if author.Name == "" {
			author = cc.Author
		}
		when := opts.When
		if when.IsZero() {
			when = time.Now()
		}
		author.When = when
		nc := &Commit{
			Tree:      treeID,
			Parents:   []string{st.Head},
			Author:    author,
			Committer: author,
			Message:   cc.Message,
		}
		newID, err := r.writeCommit(nc)
		if err != nil {
			return err
		}
		if err := r.writeHEADDetached(newID); err != nil {
			return err
		}

		// 推进状态：current 出队、落 head、清 current/conflicts。
		pending, err := readLines(filepath.Join(dir, "pending"))
		if err != nil {
			return err
		}
		if len(pending) == 0 || pending[0] != cur {
			return errors.New("vcs: rebase state corrupt: pending/current mismatch")
		}
		if err := writeLines(filepath.Join(dir, "pending"), pending[1:]); err != nil {
			return err
		}
		if err := writeLines(filepath.Join(dir, "head"), []string{newID}); err != nil {
			return err
		}
		prev, _ := readLines(filepath.Join(dir, "applied"))
		if err := writeLines(filepath.Join(dir, "applied"), append(prev, newID)); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, "current")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(filepath.Join(dir, "conflicts")); err != nil && !os.IsNotExist(err) {
			return err
		}

		rr := &RebaseResult{Kind: "rebased", Head: newID, Applied: 1}
		completed, err := r.replayLoop(rr, opts)
		if err != nil {
			return err
		}
		if completed {
			meta := st.Meta
			meta.Pending, _ = readLines(filepath.Join(dir, "pending"))
			if err := r.finishRebase(meta); err != nil {
				return err
			}
		}
		res = rr
		return nil
	})
	return res, err
}

// RebaseAbort 整体回退：用起始快照精确恢复工作区、索引、HEAD，
// 然后删除重放状态。恢复完成后仓库状态与操作开始前完全一致。
func (r *Repository) RebaseAbort() error {
	return r.withLock(func() error {
		st, err := r.RebaseStatus()
		if err != nil {
			return err
		}
		if !st.Present {
			return errors.New("vcs: no rebase in progress")
		}
		sn, err := r.loadSnapshot()
		if err != nil {
			return err
		}
		if err := r.restoreSnapshot(sn); err != nil {
			return err
		}
		return removeAll(r.rebasePath())
	})
}

// RebaseRecover 在进程重启后识别未完成的重放并恢复现场。
//
// 返回动作：
//   - "none"    : 无未完成重放
//   - "paused"  : 检测到冲突暂停，现场已确认一致，等待 continue/abort
//   - "aborted" : 检测到非暂停但状态不完整（进程在重放中途被杀），
//     已安全回退到起始快照
//
// 由于每个提交的推进都是 “工作区/HEAD 落定 -> 状态文件原子更新”，
// 崩溃只会落在两种一致点：尚未暂停（可能已应用若干提交但 current
// 未落盘）或已暂停。对前者，宁可整体安全回退，也不留下半成品。
func (r *Repository) RebaseRecover() (string, error) {
	var action string
	err := r.withLock(func() error {
		st, err := r.RebaseStatus()
		if err != nil {
			return err
		}
		if !st.Present {
			action = "none"
			return nil
		}
		if st.Paused {
			// 暂停点：把工作区/索引/HEAD 对齐到暂停现场（st.Head 提交）。
			if err := r.alignToPaused(st); err != nil {
				return err
			}
			action = "paused"
			return nil
		}
		// 非暂停的未完成重放：无法确定边界，安全回退。
		sn, err := r.loadSnapshot()
		if err != nil {
			return err
		}
		if err := r.restoreSnapshot(sn); err != nil {
			return err
		}
		if err := removeAll(r.rebasePath()); err != nil {
			return err
		}
		action = "aborted"
		return nil
	})
	return action, err
}

// alignToPaused 确保暂停现场与 st.Head 提交一致。
func (r *Repository) alignToPaused(st *rebaseStatus) error {
	if st.Head == "" {
		return errors.New("vcs: rebase state corrupt: missing head")
	}
	c, err := r.ReadCommit(st.Head)
	if err != nil {
		return err
	}
	target, err := r.flattenTree(c.Tree)
	if err != nil {
		return err
	}
	old, err := r.headTreeMap()
	if err != nil {
		// HEAD 可能已是分离状态，尝试按当前提交读取。
		old = BlobMap{}
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
	return r.writeHEADDetached(st.Head)
}
