package vcs

import "sort"

// ancestors 返回从 start 可达的全部提交集合（含自身）。
func (r *Repository) ancestors(start string) (map[string]bool, error) {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		c, err := r.ReadCommit(id)
		if err != nil {
			return nil, err
		}
		stack = append(stack, c.Parents...)
	}
	return seen, nil
}

// MergeBases 返回两个提交的全部共同祖先（merge-base --all），
// 即双方祖先集合交集中不被交集内其他提交所包含的元素。
func (r *Repository) MergeBases(a, b string) ([]string, error) {
	aa, err := r.ancestors(a)
	if err != nil {
		return nil, err
	}
	bb, err := r.ancestors(b)
	if err != nil {
		return nil, err
	}
	var common []string
	for id := range aa {
		if bb[id] {
			common = append(common, id)
		}
	}
	sort.Strings(common)

	// 剔除被交集中其他元素包含的祖先，保留极大元。
	var bases []string
	for _, c := range common {
		dominated := false
		for _, other := range common {
			if c == other {
				continue
			}
			if bb[other] && aa[other] && r.isAncestor(c, other) {
				dominated = true
				break
			}
		}
		if !dominated {
			bases = append(bases, c)
		}
	}
	sort.Strings(bases)
	return bases, nil
}

// isAncestor 判断 ancestor 是否为 descendant 的祖先（含相等）。
// 注意：调用处保证两个 ID 都在同一条可达关系内。
func (r *Repository) isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		c, err := r.ReadCommit(id)
		if err != nil {
			return false
		}
		for _, p := range c.Parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// IsAncestor 是 isAncestor 的导出包装，供 CLI 与测试使用。
func (r *Repository) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	aa, err := r.ancestors(descendant)
	if err != nil {
		return false, err
	}
	return aa[ancestor], nil
}
