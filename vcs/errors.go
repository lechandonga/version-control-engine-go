package vcs

import "errors"

// 不同失败原因使用不同的错误类型，便于自动化流程精确判定。

// ConflictEntry 描述单个冲突文件的三方内容来源。
type ConflictEntry struct {
	// Path 为冲突文件相对工作区根的路径。
	Path string
	// Base/Ours/Theirs 分别为共同祖先、当前分支、合入分支上的 blob 对象 ID；
	// 对应一方不存在时为空字符串（新增/删除冲突）。
	Base   string
	Ours   string
	Theirs string
}

// ObjectMissing 表示按内容地址读取时对象文件不存在。
type ObjectMissing struct{ ID string }

func (e *ObjectMissing) Error() string { return "object missing: " + e.ID }

// ObjectTruncated 表示对象容器长度与声明不符（写入中断/截断）。
type ObjectTruncated struct{ ID string }

func (e *ObjectTruncated) Error() string { return "object truncated: " + e.ID }

// ObjectTampered 表示负载哈希与对象地址不一致（内容被篡改）。
type ObjectTampered struct{ ID string }

func (e *ObjectTampered) Error() string { return "object hash mismatch: " + e.ID }

// ObjectCorrupt 表示容器或对象负载结构损坏（头部非法、类型未知、编码错误）。
type ObjectCorrupt struct {
	ID  string
	Msg string
}

func (e *ObjectCorrupt) Error() string { return "object corrupt: " + e.ID + ": " + e.Msg }

// RefNotFound 表示引用不存在。
type RefNotFound struct{ Name string }

func (e *RefNotFound) Error() string { return "ref not found: " + e.Name }

// RefCorrupt 表示引用文件内容损坏（不是合法哈希/符号引用）。
type RefCorrupt struct {
	Name string
	Msg  string
}

func (e *RefCorrupt) Error() string { return "ref corrupt: " + e.Name + ": " + e.Msg }

// WouldOverwrite 表示操作会覆盖本地未提交修改或未跟踪文件。
type WouldOverwrite struct{ Paths []string }

func (e *WouldOverwrite) Error() string {
	return "operation would overwrite local changes: " + joinPaths(e.Paths)
}

// MergeConflict 表示三方合并产生了无法自动消解的冲突。
type MergeConflict struct {
	Paths []ConflictEntry
}

func (e *MergeConflict) Error() string {
	return "merge conflicts: " + joinPaths(conflictPaths(e.Paths))
}

// RebaseConflict 表示重放某个提交时发生冲突，现场已保留。
type RebaseConflict struct {
	Commit string
	Paths  []ConflictEntry
}

func (e *RebaseConflict) Error() string {
	return "rebase conflict at " + shortID(e.Commit) + ": " + joinPaths(conflictPaths(e.Paths))
}

// UnrelatedHistories 表示两条历史没有共同祖先。
type UnrelatedHistories struct {
	A string
	B string
}

func (e *UnrelatedHistories) Error() string {
	return "unrelated histories: " + shortID(e.A) + " and " + shortID(e.B)
}

// ErrRebaseInProgress / ErrMergeInProgress / ErrNoOperation 用于状态判定。
var (
	ErrRebaseInProgress = errors.New("rebase already in progress")
	ErrMergeInProgress  = errors.New("merge already in progress")
	ErrNoRebase         = errors.New("no rebase in progress")
	ErrNoMerge          = errors.New("no merge in progress")
)

func joinPaths(paths []string) string {
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func conflictPaths(cs []ConflictEntry) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
