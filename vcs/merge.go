package vcs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ConflictEntry 描述一条三路合并冲突。
type ConflictEntry struct {
	Path   string `json:"path"`
	Base   string `json:"base,omitempty"`
	Ours   string `json:"ours,omitempty"`
	Theirs string `json:"theirs,omitempty"`
}

// MergeState 是在途合并现场，落盘保存以便恢复与作为回收的可达根。
type MergeState struct {
	Theirs string   `json:"theirs"`
	Base   string   `json:"base"`
	Paths  []string `json:"paths"`
}

func (r *Repo) mergeStatePath() string { return filepath.Join(r.VCS, "MERGE_STATE") }

func (r *Repo) readMergeState() (*MergeState, error) {
	data, err := os.ReadFile(r.mergeStatePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st MergeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, &ObjectCorrupt{ID: "MERGE_STATE", Msg: "bad encoding"}
	}
	return &st, nil
}

// MergeBase 找到两个提交最近的共同祖先；无共同祖先返回 UnrelatedHistories。
func (r *Repo) MergeBase(a, b string) (string, error) {
	ancA, err := r.ancestors(a)
	if err != nil {
		return "", err
	}
	// 从 b 做 BFS，第一个命中 ancA 的即最近共同祖先。
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if ancA[id] {
			return id, nil
		}
		c, err := r.ReadCommit(id)
		if err != nil {
			return "", err
		}
		queue = append(queue, c.Parents...)
	}
	return "", &UnrelatedHistories{A: a, B: b}
}

func (r *Repo) ancestors(id string) (map[string]bool, error) {
	out := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if out[cur] {
			continue
		}
		out[cur] = true
		c, err := r.ReadCommit(cur)
		if err != nil {
			return nil, err
		}
		queue = append(queue, c.Parents...)
	}
	return out, nil
}

// mergeTrees 三路合并三个 路径->blobID 映射；返回合并结果或冲突列表。
func mergeTrees(base, ours, theirs map[string]string) (map[string]string, []ConflictEntry) {
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	out := map[string]string{}
	var conflicts []ConflictEntry
	for p := range paths {
		b, inB := base[p]
		o, inO := ours[p]
		t, inT := theirs[p]
		switch {
		case inO && inT && o == t:
			out[p] = o // 双方一致（含都未改）
		case inB && inO && !inT && b == o:
			// 对方删除，我方未改 -> 删除
		case inB && !inO && inT && b == t:
			// 我方删除，对方未改 -> 删除
		case !inB && inO && !inT:
			out[p] = o // 仅我方新增
		case !inB && !inO && inT:
			out[p] = t // 仅对方新增
		case inB && inO && b == o && inT:
			out[p] = t // 仅对方修改
		case inB && inT && b == t && inO:
			out[p] = o // 仅我方修改
		case inB && !inO && !inT:
			// 双方都删除
		default:
			conflicts = append(conflicts, ConflictEntry{Path: p, Base: b, Ours: o, Theirs: t})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return out, conflicts
}

// buildTree 把 路径->blobID 映射写回嵌套 tree 对象，返回根 tree ID。
func (r *Repo) buildTree(files map[string]string) (string, error) {
	dirs := map[string][]TreeEntry{"": {}}
	for _, rel := range sortedKeys(files) {
		dir, name := splitPath(rel)
		dirs[dir] = append(dirs[dir], TreeEntry{Name: name, ID: files[rel]})
	}
	depths := make([]string, 0, len(dirs))
	for d := range dirs {
		depths = append(depths, d)
	}
	sort.Slice(depths, func(i, j int) bool { return len(depths[i]) > len(depths[j]) })
	treeIDs := map[string]string{}
	for _, d := range depths {
		id, err := r.writeTree(&Tree{Entries: dirs[d]})
		if err != nil {
			return "", err
		}
		treeIDs[d] = id
		if d == "" {
			continue
		}
		parent, name := splitPath(trimSlash(d))
		dirs[parent] = append(dirs[parent], TreeEntry{Name: name + "/", ID: id})
	}
	return treeIDs[""], nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Merge 把目标分支/提交合并进当前分支。冲突时保留现场并返回 MergeConflict。
func (r *Repo) Merge(target string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, err := r.readMergeState(); err != nil {
		return "", err
	} else if st != nil {
		return "", ErrMergeInProgress
	}
	theirs, err := r.resolveTarget(target)
	if err != nil {
		return "", err
	}
	head, err := r.CurrentCommit()
	if err != nil {
		return "", err
	}
	if head == "" {
		return "", &RefNotFound{Name: "HEAD"}
	}
	base, err := r.MergeBase(head, theirs)
	if err != nil {
		return "", err
	}
	if base == theirs {
		return head, nil // 已包含，无需合并
	}
	if base == head {
		// 快进
		if err := r.checkoutTreeLocked(theirs); err != nil {
			return "", err
		}
		if err := r.updateHeadLocked(theirs, "merge", "fast-forward "+target); err != nil {
			return "", err
		}
		return theirs, nil
	}
	baseTree, err := r.commitTree(base)
	if err != nil {
		return "", err
	}
	ourTree, err := r.commitTree(head)
	if err != nil {
		return "", err
	}
	theirTree, err := r.commitTree(theirs)
	if err != nil {
		return "", err
	}
	merged, conflicts := mergeTrees(baseTree, ourTree, theirTree)
	if len(conflicts) > 0 {
		st := &MergeState{Theirs: theirs, Base: base}
		for _, c := range conflicts {
			st.Paths = append(st.Paths, c.Path)
		}
		data, _ := json.Marshal(st)
		if err := writeFileAtomic(r.mergeStatePath(), data); err != nil {
			return "", err
		}
		return "", &MergeConflict{Paths: conflicts}
	}
	// 自动合并成功：落工作区并创建合并提交。
	treeID, err := r.buildTree(merged)
	if err != nil {
		return "", err
	}
	if err := r.materialize(merged); err != nil {
		return "", err
	}
	c := &Commit{
		Tree:    treeID,
		Parents: []string{head, theirs},
		Message: "merge " + target,
		Author:  "vcs",
		Time:    r.Now().UTC().Format(timeRFC),
	}
	data, _ := json.Marshal(c)
	id, err := r.WriteObject(TypeCommit, data)
	if err != nil {
		return "", err
	}
	if err := r.updateHeadLocked(id, "merge", "merge "+target); err != nil {
		return "", err
	}
	return id, nil
}

// MergeAbort 放弃在途合并，回到合并前位置。
func (r *Repo) MergeAbort() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.readMergeState()
	if err != nil {
		return err
	}
	if st == nil {
		return ErrNoMerge
	}
	head, err := r.CurrentCommit()
	if err != nil {
		return err
	}
	if err := r.checkoutTreeLocked(head); err != nil {
		return err
	}
	return os.Remove(r.mergeStatePath())
}

// resolveTarget 把分支名或 commit ID 解析为 commit ID。
func (r *Repo) resolveTarget(target string) (string, error) {
	if id, err := r.ReadRef("refs/heads/" + target); err == nil {
		return id, nil
	}
	if isHexID(target) && r.HasObject(target) {
		return target, nil
	}
	return "", &RefNotFound{Name: target}
}

func (r *Repo) commitTree(id string) (map[string]string, error) {
	c, err := r.ReadCommit(id)
	if err != nil {
		return nil, err
	}
	return r.flattenTree(c.Tree)
}

// materialize 把 路径->blobID 映射写入工作区（不做覆盖检查，调用方负责）。
func (r *Repo) materialize(files map[string]string) error {
	work, err := r.listWorkFiles()
	if err != nil {
		return err
	}
	for p := range work {
		if _, ok := files[p]; !ok {
			os.Remove(r.workPath(p))
		}
	}
	for p, id := range files {
		typ, payload, err := r.ReadObject(id)
		if err != nil {
			return err
		}
		if typ != TypeBlob {
			return &ObjectCorrupt{ID: id, Msg: "expected blob"}
		}
		if err := os.MkdirAll(filepath.Dir(r.workPath(p)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(r.workPath(p), payload, 0o644); err != nil {
			return err
		}
	}
	return nil
}

const timeRFC = "2006-01-02T15:04:05.999999999Z07:00"
