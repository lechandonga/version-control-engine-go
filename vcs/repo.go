package vcs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Repo 表示一个本地版本控制仓库：Root 为工作区，VCS 为元数据目录。
type Repo struct {
	Root string
	VCS  string

	// Now 可注入时钟，便于测试保留期等时间相关逻辑。
	Now func() time.Time

	mu          sync.Mutex // 串行化进程内的引用更新与维护操作
	packMu      sync.Mutex // 保护归档包缓存
	packs       []*packFile
	packsLoaded bool
}

// Init 在 dir 下创建新仓库。
func Init(dir string) (*Repo, error) {
	vcsDir := filepath.Join(dir, ".vcs")
	for _, sub := range []string{"objects", "packs", "refs/heads", "logs"} {
		if err := os.MkdirAll(filepath.Join(vcsDir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if err := writeFileAtomic(filepath.Join(vcsDir, "HEAD"), []byte("ref: refs/heads/master\n")); err != nil {
		return nil, err
	}
	return &Repo{Root: dir, VCS: vcsDir, Now: time.Now}, nil
}

// Open 打开已有仓库。只校验元数据目录存在，不读取日志等可损坏的附属数据。
func Open(dir string) (*Repo, error) {
	vcsDir := filepath.Join(dir, ".vcs")
	st, err := os.Stat(vcsDir)
	if err != nil || !st.IsDir() {
		return nil, &RefCorrupt{Name: ".vcs", Msg: "not a repository"}
	}
	return &Repo{Root: dir, VCS: vcsDir, Now: time.Now}, nil
}

func (r *Repo) headPath() string { return filepath.Join(r.VCS, "HEAD") }
func (r *Repo) refPath(name string) string {
	return filepath.Join(r.VCS, filepath.FromSlash(name))
}

// ReadRef 读取引用值（commit ID）。
func (r *Repo) ReadRef(name string) (string, error) {
	data, err := os.ReadFile(r.refPath(name))
	if err != nil {
		return "", &RefNotFound{Name: name}
	}
	id := strings.TrimSpace(string(data))
	if !isHexID(id) {
		return "", &RefCorrupt{Name: name, Msg: "not a commit id"}
	}
	return id, nil
}

// writeRefLocked 原子更新引用并记录操作日志。调用方需持有 r.mu。
func (r *Repo) writeRefLocked(name, id, op, msg string) error {
	old, _ := r.ReadRef(name)
	if err := writeFileAtomic(r.refPath(name), []byte(id+"\n")); err != nil {
		return err
	}
	r.logMoveLocked(op, name, old, id, msg)
	return nil
}

// DeleteRef 删除引用并记录日志（old 位置可据此找回）。
func (r *Repo) DeleteRef(name, op, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, err := r.ReadRef(name)
	if err != nil {
		return err
	}
	if err := os.Remove(r.refPath(name)); err != nil {
		return err
	}
	r.logMoveLocked(op, name, old, "", msg)
	return nil
}

// ListRefs 列出 refs/heads 下的全部分支及指向。
func (r *Repo) ListRefs() (map[string]string, error) {
	out := map[string]string{}
	base := filepath.Join(r.VCS, "refs", "heads")
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(r.VCS), p)
		id, err := r.ReadRef(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = id
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}

// HeadRef 返回 HEAD 指向的分支引用名；分离头指针时返回空。
func (r *Repo) HeadRef() (string, error) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", &RefCorrupt{Name: "HEAD", Msg: "unreadable"}
	}
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "ref: ") {
		return strings.TrimPrefix(s, "ref: "), nil
	}
	if isHexID(s) {
		return "", nil
	}
	return "", &RefCorrupt{Name: "HEAD", Msg: "bad content"}
}

// CurrentCommit 返回当前位置的 commit ID；仓库尚无提交时返回空串。
func (r *Repo) CurrentCommit() (string, error) {
	data, err := os.ReadFile(r.headPath())
	if err != nil {
		return "", &RefCorrupt{Name: "HEAD", Msg: "unreadable"}
	}
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "ref: ") {
		id, err := r.ReadRef(strings.TrimPrefix(s, "ref: "))
		if err != nil {
			if _, ok := err.(*RefNotFound); ok {
				return "", nil // 新仓库，分支尚未创建
			}
			return "", err
		}
		return id, nil
	}
	if isHexID(s) {
		return s, nil
	}
	return "", &RefCorrupt{Name: "HEAD", Msg: "bad content"}
}

// setHeadLocked 更新 HEAD（符号引用或分离），并记录日志。调用方需持有 r.mu。
func (r *Repo) setHeadLocked(target, op, msg string) error {
	old, _ := r.CurrentCommit()
	var content string
	if strings.HasPrefix(target, "refs/") {
		content = "ref: " + target + "\n"
	} else {
		content = target + "\n"
	}
	if err := writeFileAtomic(r.headPath(), []byte(content)); err != nil {
		return err
	}
	newID, _ := r.CurrentCommit()
	r.logMoveLocked(op, "HEAD", old, newID, msg)
	return nil
}

// updateHeadLocked 提交后前进 HEAD（跟随符号引用）。调用方需持有 r.mu。
func (r *Repo) updateHeadLocked(id, op, msg string) error {
	ref, err := r.HeadRef()
	if err != nil {
		return err
	}
	if ref != "" {
		return r.writeRefLocked(ref, id, op, msg)
	}
	old, _ := r.CurrentCommit()
	if err := writeFileAtomic(r.headPath(), []byte(id+"\n")); err != nil {
		return err
	}
	r.logMoveLocked(op, "HEAD", old, id, msg)
	return nil
}

// sortedKeys 返回排序后的键，保证输出稳定。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
