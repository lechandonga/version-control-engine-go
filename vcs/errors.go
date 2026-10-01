package vcs

import "errors"

var (
	// ErrObjectNotFound 对象在对象库中不存在（文件缺失）。
	ErrObjectNotFound = errors.New("vcs: object not found")
	// ErrObjectTruncated 对象文件被截断，结构不完整。
	ErrObjectTruncated = errors.New("vcs: object truncated")
	// ErrObjectTampered 对象内容与路径标识不一致（哈希校验失败），内容被篡改。
	ErrObjectTampered = errors.New("vcs: object content tampered")
	// ErrObjectCorrupt 对象无法按任何已知类型解析。
	ErrObjectCorrupt = errors.New("vcs: object corrupt")
	// ErrObjectTypeMismatch 调用方期望的对象类型与磁盘上的实际类型不符。
	ErrObjectTypeMismatch = errors.New("vcs: object type mismatch")
	// ErrRefNotFound 引用不存在。
	ErrRefNotFound = errors.New("vcs: ref not found")
	// ErrRefCorrupt 引用内容损坏（空内容、非法十六进制等）。
	ErrRefCorrupt = errors.New("vcs: ref corrupt")
	// ErrRefDangling 引用指向的提交不存在。
	ErrRefDangling = errors.New("vcs: ref points to missing object")
	// ErrRefType 引用指向的对象不是提交。
	ErrRefType = errors.New("vcs: ref does not point to a commit")
	// ErrLockHeld 仓库锁被其他进程持有。
	ErrLockHeld = errors.New("vcs: repository is locked")
)

// IntegrityError 携带对象标识与具体原因，便于调用方区分失败类别。
type IntegrityError struct {
	ID   string
	Kind error
	Path string
}

func (e *IntegrityError) Error() string {
	name := ""
	switch e.Kind {
	case ErrObjectNotFound:
		name = "missing"
	case ErrObjectTruncated:
		name = "truncated"
	case ErrObjectTampered:
		name = "tampered"
	case ErrObjectCorrupt:
		name = "corrupt"
	case ErrObjectTypeMismatch:
		name = "type-mismatch"
	}
	return "vcs: object " + e.ID + " " + name
}

func (e *IntegrityError) Unwrap() error { return e.Kind }

// RefError 携带引用名与损坏原因。
type RefError struct {
	Name string
	Kind error
}

func (e *RefError) Error() string {
	name := "corrupt"
	switch e.Kind {
	case ErrRefNotFound:
		name = "not-found"
	case ErrRefCorrupt:
		name = "corrupt"
	case ErrRefDangling:
		name = "dangling"
	case ErrRefType:
		name = "wrong-type"
	}
	return "vcs: ref " + e.Name + " " + name
}

func (e *RefError) Unwrap() error { return e.Kind }
