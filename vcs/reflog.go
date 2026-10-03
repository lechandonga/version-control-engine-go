package vcs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 操作留痕（reflog）落盘在 logs/reflog，采用 JSON Lines：
// 每条记录恰好一行 JSON，行间互不依赖。单行损坏只影响该行，可被单独
// 识别（ReadReflog 返回的 ReflogEntry.Bad=true），不会连累仓库打开。
//
// 记录结构（ReflogEntry）：
//
//	{"time":"RFC3339Nano","ref":"refs/heads/x 或 HEAD","op":"commit|...",
//	 "old":"<id 或空>","new":"<id 或空>","reason":"可读原因","pid":123}
//
// 可靠性：记录先于指针更新落盘（fsync 后追加），随后原子 rename 指针。
// 若两步之间进程被杀，下一次打开仓库会把这种“悬空记录”对账标记为
// pending 并按指针现状收尾；恢复时只认 (ref, old, new) 三元组。

// ReflogOp 标识触发指针移动的操作种类。
type ReflogOp string

const (
	OpCommit        ReflogOp = "commit"
	OpCheckout      ReflogOp = "checkout"
	OpBranchCreate  ReflogOp = "branch-create"
	OpBranchDelete  ReflogOp = "branch-delete"
	OpMerge         ReflogOp = "merge"
	OpRebaseAdvance ReflogOp = "rebase-advance"
	OpRebasePause   ReflogOp = "rebase-pause"
	OpRebaseAbort   ReflogOp = "rebase-abort"
	OpRestore       ReflogOp = "restore"
)

// ReflogEntry 为一条操作留痕。
type ReflogEntry struct {
	Line   int       `json:"-"`
	Time   time.Time `json:"time"`
	Ref    string    `json:"ref"`
	Op     ReflogOp  `json:"op"`
	Old    string    `json:"old"`
	New    string    `json:"new"`
	Reason string    `json:"reason"`
	Pid    int       `json:"pid"`
	// Bad=true 表示该行无法解析，BadReason 给出原因；坏行不阻断其它记录。
	Bad       bool   `json:"-"`
	BadReason string `json:"-"`
	// Pending=true 表示该记录写入后未见对应指针落盘（崩溃对账得出）。
	Pending bool `json:"pending,omitempty"`
	// terminal=true 表示该记录是对引用终态的独立写入（建/删分支、恢复），
	// 读取时要核对当前指针是否等于记录的 New；指针推进类操作不校验。
	terminal bool `json:"-"`
}

func (e ReflogEntry) String() string {
	old := e.Old
	if old == "" {
		old = "<none>"
	}
	return fmt.Sprintf("%s %s %s: %s %s -> %s%s",
		e.Time.Format(time.RFC3339Nano), e.Ref, e.Op, e.Reason, shortID(old), shortID(e.New),
		badPendingSuffix(e))
}

func badPendingSuffix(e ReflogEntry) string {
	switch {
	case e.Bad:
		return " [CORRUPT LINE: " + e.BadReason + "]"
	case e.Pending:
		return " [pending: 指针未落盘或已被后续操作覆盖]"
	default:
		return ""
	}
}

func (r *Repo) reflogPath() string { return filepath.Join(r.root, "logs", "reflog") }

// appendReflog 追加一条留痕（带文件锁，fsync 保证崩溃后可见）。
func (r *Repo) appendReflog(ref string, op ReflogOp, oldID, newID, reason string) error {
	if ref == "" {
		return errors.New("reflog: empty ref name")
	}
	e := ReflogEntry{
		Time:   time.Now().UTC(),
		Ref:    ref,
		Op:     op,
		Old:    oldID,
		New:    newID,
		Reason: reason,
		Pid:    os.Getpid(),
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	lock := newFileLock(r.root, "reflog.lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	f, err := os.OpenFile(r.reflogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}

// ReadReflog 读取全部留痕。ref 非空时只看该引用（HEAD 用 "HEAD"）；
// since/until 为零值时不限。坏行以 Bad=true 原样返回，不返回错误。
func (r *Repo) ReadReflog(ref string, since, until time.Time) ([]ReflogEntry, error) {
	data, err := os.ReadFile(r.reflogPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	current := r.snapshotRefsForPending()
	var out []ReflogEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e ReflogEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			out = append(out, ReflogEntry{Line: lineNo, Bad: true, BadReason: "json: " + err.Error()})
			continue
		}
		e.Line = lineNo
		if !isHexID(e.New) && e.New != "" {
			e.Bad = true
			e.BadReason = "new target is not an object id"
		} else if !isHexID(e.Old) && e.Old != "" {
			e.Bad = true
			e.BadReason = "old target is not an object id"
		} else if e.Ref == "" || e.Op == "" {
			e.Bad = true
			e.BadReason = "missing ref or op"
		}
		if !e.Bad && isTerminalOp(e.Op) {
			cur, ok := current[e.Ref]
			if e.Op == OpBranchDelete && !ok {
				e.Pending = false // 引用不存在正是删除的终态
			} else {
				e.Pending = !ok || cur != e.New
			}
		}
		if ref != "" && e.Ref != ref {
			continue
		}
		if !since.IsZero() && e.Time.Before(since) {
			continue
		}
		if !until.IsZero() && e.Time.After(until) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, sc.Err()
}

func isTerminalOp(op ReflogOp) bool {
	return op == OpBranchCreate || op == OpBranchDelete || op == OpRestore
}

// ReflogCorruptLine 描述一条无法解析的留痕行。
type ReflogCorruptLine struct {
	Line   int
	Reason string
}

// ReflogHealth 为留痕文件健康检查结果。
type ReflogHealth struct {
	Corrupt []ReflogCorruptLine
	Entries int
}

// CheckReflog 单独检查留痕文件并分类汇报坏行；仓库其余功能不依赖本结果。
func (r *Repo) CheckReflog() (ReflogHealth, error) {
	var h ReflogHealth
	data, err := os.ReadFile(r.reflogPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return h, nil
		}
		return h, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		h.Entries++
		var e ReflogEntry
		if jerr := json.Unmarshal(raw, &e); jerr != nil {
			h.Corrupt = append(h.Corrupt, ReflogCorruptLine{Line: lineNo, Reason: "json: " + jerr.Error()})
			continue
		}
		if e.Ref == "" || e.Op == "" {
			h.Corrupt = append(h.Corrupt, ReflogCorruptLine{Line: lineNo, Reason: "missing ref or op"})
			continue
		}
		if (!isHexID(e.Old) && e.Old != "") || (!isHexID(e.New) && e.New != "") {
			h.Corrupt = append(h.Corrupt, ReflogCorruptLine{Line: lineNo, Reason: "target is not an object id"})
		}
	}
	return h, sc.Err()
}

// snapshotRefsForPending 收集 HEAD 与所有分支当前指向，供悬空记录对账。
func (r *Repo) snapshotRefsForPending() map[string]string {
	out := map[string]string{}
	head, err := r.headTarget()
	if err == nil {
		out["HEAD"] = head
	}
	if names, err := r.Branches(); err == nil {
		for _, n := range names {
			if id, err := r.ResolveBranch(n); err == nil {
				out[branchRefName(n)] = id
			}
		}
	}
	return out
}

func branchRefName(name string) string {
	if strings.HasPrefix(name, "refs/") {
		return name
	}
	return "refs/heads/" + name
}

// RestoreRef 按留痕把引用恢复到记录中的旧位置：
// 指针从当前值原子移动到 entry.Old；entry.Old 为空表示删除该引用。
// 先写留痕（restore），再原子改指针，崩溃后重放仍只落在两个位置之一。
func (r *Repo) RestoreRef(entry ReflogEntry, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.Bad || entry.Ref == "" {
		return errors.New("restore: bad or empty reflog entry")
	}
	if entry.Old != "" && !r.hasObject(entry.Old) {
		return &ObjectMissing{ID: entry.Old}
	}
	ref := entry.Ref
	if reason == "" {
		reason = "restore from reflog line " + fmt.Sprint(entry.Line)
	}
	current := ""
	if ref == "HEAD" {
		t, err := r.headTarget()
		if err == nil {
			current = t
		}
	} else {
		name := strings.TrimPrefix(ref, "refs/heads/")
		t, err := r.ResolveBranch(name)
		if err == nil {
			current = t
		} else {
			var nf *RefNotFound
			if !errors.As(err, &nf) {
				return err
			}
		}
	}
	if current == entry.Old {
		return nil // 已在目标位置，幂等
	}
	if err := r.appendReflog(ref, OpRestore, current, entry.Old, reason); err != nil {
		return err
	}
	if ref == "HEAD" {
		if entry.Old == "" {
			return errors.New("restore: cannot detach HEAD to nothing")
		}
		return r.detachHEAD(entry.Old)
	}
	name := strings.TrimPrefix(ref, "refs/heads/")
	if entry.Old == "" {
		return r.deleteBranchPointer(name)
	}
	return r.writeBranchPointer(name, entry.Old)
}
