package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// Checkout 切换当前检出位置到分支或裸提交，并同步工作区/暂存区。
// 会覆盖本地未提交修改时返回 *WouldOverwrite，引用移动留痕。
func (r *Repo) Checkout(target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var commitID, newHeadContent, refName string
	symbolic := false
	if id, err := r.ResolveBranch(target); err == nil {
		commitID = id
		refName = branchRefName(target)
		newHeadContent = "ref: " + refName + "\n"
		symbolic = true
	} else {
		var nf *RefNotFound
		if !errors.As(err, &nf) {
			return err
		}
		if _, cerr := r.readCommit(target); cerr != nil {
			return cerr
		}
		commitID = target
		newHeadContent = target + "\n"
	}

	commit, err := r.readCommit(commitID)
	if err != nil {
		return err
	}
	targetEntries, err := r.treeFlat(commit.Tree)
	if err != nil {
		return err
	}

	old, _ := r.headTarget()
	var headEntries []IndexEntry
	if old != "" {
		if headCommit, herr := r.readCommit(old); herr == nil {
			headEntries, _ = r.treeFlat(headCommit.Tree)
		}
	}
	staged, err := r.readIndex()
	if err != nil {
		return err
	}
	dirty := localChanges(headEntries, staged, r.workdir())
	if paths := wouldOverwritePaths(dirty, indexMap(targetEntries), r.workdir()); len(paths) > 0 {
		return &WouldOverwrite{Paths: paths}
	}

	if err := r.replaceWorktree(indexMap(targetEntries), r.workdir()); err != nil {
		return err
	}
	if err := r.writeIndex(targetEntries); err != nil {
		return err
	}
	if err := r.appendReflog("HEAD", OpCheckout, old, commitID, "checkout "+target); err != nil {
		return err
	}
	if err := writeFileAtomic(r.headPath(), []byte(newHeadContent), 0o644); err != nil {
		return err
	}
	if symbolic && refName != "HEAD" {
		if err := r.appendReflog(refName, OpCheckout, old, commitID, "checkout "+target); err != nil {
			// 分支指针本身未动，仅记录 HEAD 视角即可；分支留痕保持“移动才记录”。
			_ = err
		}
	}
	return nil
}

func indexMap(entries []IndexEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Path] = e.ID
	}
	return m
}

// localChanges 返回相对 HEAD 有变化（暂存或工作区）的路径集合。
func localChanges(headEntries, staged []IndexEntry, workdir string) map[string]bool {
	head := indexMap(headEntries)
	stagedM := indexMap(staged)
	changed := map[string]bool{}
	for p, id := range stagedM {
		if head[p] != id {
			changed[p] = true
		}
	}
	for p, id := range head {
		if _, ok := stagedM[p]; !ok {
			changed[p] = true
		}
		// 已暂存但工作区又改了
		data, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(p)))
		if err == nil && hashBytes(data) != id {
			if _, inStaged := stagedM[p]; inStaged {
				changed[p] = true
			}
		}
	}
	// 工作区相对暂存区又被修改的文件
	for p, id := range stagedM {
		data, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(p)))
		if err == nil && hashBytes(data) != id {
			changed[p] = true
		}
	}
	return changed
}

// wouldOverwritePaths 判断检出目标是否会覆盖本地改动。
func wouldOverwritePaths(dirty map[string]bool, target map[string]string, workdir string) []string {
	var paths []string
	for p := range dirty {
		targetID, inTarget := target[p]
		data, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(p)))
		if err != nil {
			if inTarget {
				paths = append(paths, p)
			}
			continue
		}
		if hashBytes(data) != targetID {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// replaceWorktree 把工作区替换为目标快照：删除目标中不存在的旧普通文件，
// 写入目标文件；.vcs 目录永不触碰。
func (r *Repo) replaceWorktree(target map[string]string, workdir string) error {
	keep := map[string]bool{}
	for p, id := range target {
		keep[p] = true
		abs := filepath.Join(workdir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		data, err := r.readBlob(id)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(abs, data, 0o644); err != nil {
			return err
		}
	}
	// 清理不再受版本控制的旧文件（仅删除已知会变空的目录下的文件）。
	return filepath.WalkDir(workdir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".vcs" {
			if rel == ".vcs" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !keep[rel] {
			return os.Remove(path)
		}
		return nil
	})
}
