package vcs

import (
	"errors"
	"os"
	"path/filepath"
)

// 仓库内部目录与文件名。
const (
	dirObjects = "objects"
	dirRefs    = "refs"
	dirHeads   = "heads"
	dirState   = "state"

	fileHEAD  = "HEAD"
	fileIndex = "index"
	lockName  = "lock"
)

// Repository 表示一个本地仓库：工作区为 workDir，元数据全部在 workDir/.vcs 下。
type Repository struct {
	root    string // .vcs 目录
	workDir string
	objects *objectStore
}

// Init 在 workDir 创建空仓库并建立 main 的未出生引用；重复初始化返回错误。
func Init(workDir string) (*Repository, error) {
	root := filepath.Join(workDir, ".vcs")
	if fileExists(root) {
		return nil, errors.New("vcs: repository already exists")
	}
	for _, d := range []string{
		filepath.Join(root, dirObjects),
		filepath.Join(root, dirRefs, dirHeads),
		filepath.Join(root, dirState),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := writeFileAtomic(filepath.Join(root, fileHEAD), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(root, fileIndex), emptyIndex(), 0o644); err != nil {
		return nil, err
	}
	if err := syncDir(root); err != nil {
		return nil, err
	}
	return Open(workDir)
}

// Open 打开已存在的仓库，并对 HEAD 与引用做基础完整性检查。
func Open(workDir string) (*Repository, error) {
	root := filepath.Join(workDir, ".vcs")
	if !fileExists(root) {
		return nil, errors.New("vcs: not a repository (missing .vcs)")
	}
	r := &Repository{
		root:    root,
		workDir: workDir,
		objects: newObjectStore(root),
	}
	// HEAD 必须存在且语法合法；指向的分支允许尚未出生。
	if _, _, err := r.readHEADRaw(); err != nil {
		return nil, err
	}
	return r, nil
}

// Root 返回 .vcs 目录。
func (r *Repository) Root() string { return r.root }

// WorkDir 返回工作区目录。
func (r *Repository) WorkDir() string { return r.workDir }

// lock 获取仓库级写锁。
func (r *Repository) lock() (*LockFile, error) {
	return acquireLock(r.root, lockName)
}

// withLock 执行持有仓库锁的临界区。
func (r *Repository) withLock(fn func() error) error {
	l, err := r.lock()
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

// Fsck 对全仓库做完整性检查：
//   - 每个对象通过哈希、长度与结构校验；
//   - tree 引用的子对象存在且类型正确（递归）；
//   - commit 引用的 tree/parent 存在且类型正确；
//   - 所有引用（含 HEAD）指向存在的提交。
//
// 遇到第一个错误即返回，错误原因可通过 errors.Is 区分。
func (r *Repository) Fsck() error {
	it, err := r.objects.iter()
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	for {
		id, ok := it.nextID()
		if !ok {
			break
		}
		if err := r.fsckObject(id, visited); err != nil {
			return err
		}
	}

	refs, err := r.listRefs()
	if err != nil {
		return err
	}
	for _, name := range refs {
		if err := r.verifyRef(name); err != nil {
			return err
		}
	}
	if _, err := r.HeadCommit(); err != nil && !errors.Is(err, ErrRefNotFound) {
		return err
	}
	return nil
}

func (it *objectIter) nextID() (string, bool) {
	if it.pos >= len(it.ids) {
		return "", false
	}
	id := it.ids[it.pos]
	it.pos++
	return id, true
}

func (r *Repository) fsckObject(id string, visited map[string]bool) error {
	if visited[id] {
		return nil
	}
	visited[id] = true
	raw, err := r.objects.read(id)
	if err != nil {
		return err
	}
	switch raw.objType {
	case TypeTree:
		t, err := parseTreeBody(raw.body)
		if err != nil {
			return &IntegrityError{ID: id, Kind: errors.Join(ErrObjectCorrupt, err)}
		}
		for _, e := range t.Entries {
			child, err := r.objects.read(e.ID)
			if err != nil {
				return err
			}
			want := TypeBlob
			if e.Mode.IsDir() {
				want = TypeTree
			}
			if child.objType != want {
				return &IntegrityError{ID: e.ID, Kind: ErrObjectTypeMismatch}
			}
			if want == TypeTree {
				if err := r.fsckObject(e.ID, visited); err != nil {
					return err
				}
			}
		}
	case TypeCommit:
		c, err := parseCommitBody(raw.body)
		if err != nil {
			return &IntegrityError{ID: id, Kind: errors.Join(ErrObjectCorrupt, err)}
		}
		treeObj, err := r.objects.read(c.Tree)
		if err != nil {
			return err
		}
		if treeObj.objType != TypeTree {
			return &IntegrityError{ID: c.Tree, Kind: ErrObjectTypeMismatch}
		}
		if err := r.fsckObject(c.Tree, visited); err != nil {
			return err
		}
		for _, p := range c.Parents {
			pobj, err := r.objects.read(p)
			if err != nil {
				return err
			}
			if pobj.objType != TypeCommit {
				return &IntegrityError{ID: p, Kind: ErrObjectTypeMismatch}
			}
			if err := r.fsckObject(p, visited); err != nil {
				return err
			}
		}
	}
	return nil
}
