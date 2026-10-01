package vcs

import (
	"fmt"
	"sort"
)

// 合并冲突类型：可区分 “双方改同一处” 与 “一方删除、另一方修改”。
const (
	ConflictModifyModify = "modify-modify"
	ConflictDeleteModify = "delete-modify"
)

// Conflict 描述一个路径上的合并冲突。
type Conflict struct {
	Path string
	Kind string
	// BaseBlob 为空串表示共同祖先中该路径不存在（新增/新增）。
	BaseBlob  string
	OurBlob   string // 空串表示我方删除
	TheirBlob string // 空串表示对方删除
}

func (c Conflict) Error() string {
	switch c.Kind {
	case ConflictDeleteModify:
		return "vcs: merge conflict (delete/modify) at " + c.Path
	default:
		return "vcs: merge conflict (modify/modify) at " + c.Path
	}
}

// MergeConflictError 汇总一次合并的全部冲突。
type MergeConflictError struct {
	Conflicts []Conflict
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("vcs: merge failed with %d conflict(s)", len(e.Conflicts))
}

type blobEntry struct {
	id     string
	exists bool
}

// threeWayMerge 对扁平文件树执行三方合并。
//
// 判定规则（对每条路径独立，结果只取决于三个 blob ID）：
//   - 两边相同：任取一方
//   - 一边等于 base（未改动），另一边变化：取变化方（含删除）
//   - 两边都相对 base 变化：
//   - 变化结果相同：取该结果
//   - 一边删除、另一边改成不同内容：delete/modify 冲突
//   - 两边改成不同内容：modify/modify 冲突（含双方新增不同内容）
//
// base 是已经确定的唯一虚拟合并基（见 MergeBases 与 virtualBase）。
func threeWayMerge(base, ours, theirs BlobMap) (BlobMap, []Conflict) {
	result := BlobMap{}
	var conflicts []Conflict

	get := func(m BlobMap, p string) blobEntry {
		id, ok := m[p]
		return blobEntry{id: id, exists: ok}
	}

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
	var list []string
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)

	for _, p := range list {
		b := get(base, p)
		o := get(ours, p)
		t := get(theirs, p)

		switch {
		case o.exists && t.exists && o.id == t.id:
			result[p] = o.id
		case !o.exists && !t.exists:
			// 双方都删除（或都不存在）。
		case equalEntry(o, b):
			if t.exists {
				result[p] = t.id
			}
		case equalEntry(t, b):
			if o.exists {
				result[p] = o.id
			}
		case o.exists != t.exists:
			// 双方都相对 base 变化，且结果一边存在一边删除。
			conflicts = append(conflicts, Conflict{
				Path:      p,
				Kind:      ConflictDeleteModify,
				BaseBlob:  b.id,
				OurBlob:   o.id,
				TheirBlob: t.id,
			})
		default:
			conflicts = append(conflicts, Conflict{
				Path:      p,
				Kind:      ConflictModifyModify,
				BaseBlob:  b.id,
				OurBlob:   o.id,
				TheirBlob: t.id,
			})
		}
	}
	return result, conflicts
}

func equalEntry(a, b blobEntry) bool {
	if a.exists != b.exists {
		return false
	}
	return !a.exists || a.id == b.id
}

// virtualBase 处理多个候选共同祖先：
// 对每个路径，取候选祖先集合的 “多数票”；无多数票（分歧）时
// 回退为该路径在候选基中不存在（absent）。
//
// 多数票规则与候选集合排序无关（计数对称），而候选集合本身
// 由提交 ID 排序确定，因此虚拟基对同一仓库状态必然可复现。
// 空候选集（无共同祖先，不相关历史）等价于空基。
func (r *Repository) virtualBase(bases []string) (BlobMap, error) {
	maps := make([]BlobMap, 0, len(bases))
	for _, id := range bases {
		c, err := r.ReadCommit(id)
		if err != nil {
			return nil, err
		}
		m, err := r.flattenTree(c.Tree)
		if err != nil {
			return nil, err
		}
		maps = append(maps, m)
	}
	return majorityBase(maps), nil
}

// majorityBase 抽出为纯函数便于单测。
func majorityBase(bases []BlobMap) BlobMap {
	type vote struct {
		id     string
		exists bool
	}
	counts := map[string]map[vote]int{}
	paths := map[string]bool{}
	for _, m := range bases {
		for p, id := range m {
			paths[p] = true
			if counts[p] == nil {
				counts[p] = map[vote]int{}
			}
			counts[p][vote{id: id, exists: true}]++
		}
		// 对 “缺失” 也计票：需要全集路径，先只收集存在的路径。
	}
	// 缺失票：候选数 - 出现次数。
	n := len(bases)
	result := BlobMap{}
	for p := range paths {
		var best vote
		bestN := 0
		draw := false
		for v, c := range counts[p] {
			if c > bestN {
				best, bestN, draw = v, c, false
			} else if c == bestN {
				draw = true
			}
		}
		missing := 0
		for _, m := range bases {
			if _, ok := m[p]; !ok {
				missing++
			}
		}
		if missing > bestN {
			// 多数候选缺失该路径：虚拟基中也不存在。
			continue
		}
		if n > 0 && bestN*2 > n && !draw {
			result[p] = best.id
		}
		// 无多数票：视为 absent，不写入虚拟基。
	}
	return result
}
