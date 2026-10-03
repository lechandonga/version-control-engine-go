package vcs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// 仓库目录布局（全部位于工作区下的 .vcs/）：
//
//	objects/xx/yyyy...       松散对象（内容寻址，只读）
//	packs/pack-<id>.pack     归档文件（对象容器顺序拼接）
//	packs/pack-<id>.idx      归档索引（可选，缺失可重建）
//	refs/heads/<name>        分支引用（内容为提交 ID + 换行）
//	HEAD                     当前检出："ref: refs/heads/<name>" 或裸提交 ID
//	index                    暂存区（树形快照的一行式序列化）
//	merge-state/             在途合并现场
//	rebase-state/            在途重放现场
//	logs/reflog              操作留痕（JSON Lines，逐行独立）
//	gc/                      回收断点 / 清单
//	locks/                   进程内与跨进程互斥锁

// Repo 为一个本地版本控制仓库句柄。
type Repo struct {
	root string

	mu      sync.Mutex // 串行化本进程内会移动引用 / 改状态的操作
	packsMu sync.RWMutex
	packs   []*packFile
}

// Init 在 workdir 下初始化空仓库（幂等：已初始化时直接打开）。
func Init(workdir string) (*Repo, error) {
	root := filepath.Join(workdir, ".vcs")
	for _, d := range []string{
		filepath.Join(root, "objects"),
		filepath.Join(root, "packs"),
		filepath.Join(root, "refs", "heads"),
		filepath.Join(root, "logs"),
		filepath.Join(root, "merge-state"),
		filepath.Join(root, "rebase-state"),
		filepath.Join(root, "gc"),
		filepath.Join(root, "locks"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	headPath := filepath.Join(root, "HEAD")
	if _, err := os.Stat(headPath); errors.Is(err, fs.ErrNotExist) {
		if err := writeFileAtomic(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return Open(workdir)
}

// Open 打开已有仓库并加载归档索引。
func Open(workdir string) (*Repo, error) {
	root := filepath.Join(workdir, ".vcs")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, errors.New("not a vcs repository: " + workdir)
	}
	r := &Repo{root: root}
	if err := r.refreshPacks(); err != nil {
		return nil, err
	}
	return r, nil
}

// Root 返回仓库元数据目录（.vcs）。
func (r *Repo) Root() string { return r.root }

// writeFileAtomic 先写同目录临时文件再 rename，保证读者要么看到旧文件
// 要么看到新文件，进程被强杀也不会留下半截正式文件。
func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// removeIfExists 删除文件，不存在不报错。
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
