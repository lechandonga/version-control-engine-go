package vcs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// ReflogEntry 是一条操作留痕：什么时间、什么操作、哪个引用、从哪挪到哪。
type ReflogEntry struct {
	Time time.Time `json:"time"`
	Op   string    `json:"op"`
	Ref  string    `json:"ref"`
	Old  string    `json:"old,omitempty"`
	New  string    `json:"new,omitempty"`
	Msg  string    `json:"msg,omitempty"`
}

func (r *Repo) logPath(ref string) string {
	return filepath.Join(r.VCS, "logs", filepath.FromSlash(ref))
}

// logMoveLocked 追加一条移动记录到引用日志；HEAD 依附该引用时同步记入 HEAD 日志。
// 日志写入失败不影响主操作（留痕是附属数据），但会尽量落盘。
// 调用方需持有 r.mu。
func (r *Repo) logMoveLocked(op, ref, oldID, newID, msg string) {
	e := ReflogEntry{Time: r.Now().UTC(), Op: op, Ref: ref, Old: oldID, New: newID, Msg: msg}
	r.appendLog(ref, e)
	if ref != "HEAD" {
		if headRef, err := r.HeadRef(); err == nil && headRef == ref {
			he := e
			he.Ref = "HEAD"
			r.appendLog("HEAD", he)
		}
	}
}

func (r *Repo) appendLog(ref string, e ReflogEntry) {
	p := r.logPath(ref)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(e)
	f.Write(append(data, '\n'))
	f.Sync()
}

// ReflogCorrupt 标识一条损坏的日志记录，便于单独定位而不连累其它记录。
type ReflogCorrupt struct {
	File string
	Line int
	Msg  string
}

func (e *ReflogCorrupt) Error() string {
	return "reflog corrupt: " + e.File + ":" + itoa(e.Line) + ": " + e.Msg
}

func itoa(n int) string { return strconv.Itoa(n) }

// ReadReflog 读取某个引用的操作记录。损坏的行被单独识别并跳过，
// 不会让整份日志或整个仓库不可用。
func (r *Repo) ReadReflog(ref string) ([]ReflogEntry, []ReflogCorrupt, error) {
	f, err := os.Open(r.logPath(ref))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var entries []ReflogEntry
	var corrupt []ReflogCorrupt
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		var e ReflogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			corrupt = append(corrupt, ReflogCorrupt{File: ref, Line: line, Msg: "bad record encoding"})
			continue
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return entries, corrupt, err
	}
	return entries, corrupt, nil
}

// ListReflogs 列出所有有日志的引用名。
func (r *Repo) ListReflogs() ([]string, error) {
	var refs []string
	base := filepath.Join(r.VCS, "logs")
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		refs = append(refs, filepath.ToSlash(rel))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	sort.Strings(refs)
	return refs, err
}

// ReadAllReflog 汇总全部日志，按时间排序，用于按时间查看。
func (r *Repo) ReadAllReflog() ([]ReflogEntry, []ReflogCorrupt, error) {
	refs, err := r.ListReflogs()
	if err != nil {
		return nil, nil, err
	}
	var all []ReflogEntry
	var corrupt []ReflogCorrupt
	for _, ref := range refs {
		entries, bad, err := r.ReadReflog(ref)
		if err != nil {
			return all, corrupt, err
		}
		all = append(all, entries...)
		corrupt = append(corrupt, bad...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	return all, corrupt, nil
}

// Recover 把引用原子地恢复到指定位置（通常取自操作记录里的 old/new），
// 恢复动作本身也留痕。指针只可能落在旧值或新值，不会停在中间。
func (r *Repo) Recover(ref, id string) error {
	if !isHexID(id) {
		return &RefCorrupt{Name: ref, Msg: "recover target is not a commit id"}
	}
	if !r.HasObject(id) {
		return &ObjectMissing{ID: id}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeRefLocked(ref, id, "recover", "recover to "+shortID(id))
}

// reflogRootIDs 收集操作记录中出现的全部对象 ID（作为回收的可达根）。
func (r *Repo) reflogRootIDs() (map[string]bool, error) {
	out := map[string]bool{}
	entries, _, err := r.ReadAllReflog()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if isHexID(e.Old) {
			out[e.Old] = true
		}
		if isHexID(e.New) {
			out[e.New] = true
		}
	}
	return out, nil
}
