package vcs

import (
	"os"
	"path/filepath"
	"strconv"
)

// LockFile 是基于 O_CREATE|O_EXCL 的进程级互斥锁。
//
// 锁文件中记录持有者 PID 仅用于诊断；正确性完全依赖
// rename 原子性与锁文件的独占创建。仓库所有写操作
// （引用更新、索引更新、rebase 状态机）都必须先持有该锁。
type LockFile struct {
	path string
	f    *os.File
	held bool
}

// acquireLock 以非阻塞方式尝试加锁；失败返回 ErrLockHeld。
func acquireLock(dir, name string) (*LockFile, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLockHeld
		}
		return nil, err
	}
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	_ = f.Sync()
	return &LockFile{path: path, f: f, held: true}, nil
}

// Release 删除锁文件并关闭句柄。
func (l *LockFile) Release() error {
	if l == nil || !l.held {
		return nil
	}
	l.held = false
	err1 := l.f.Close()
	err2 := os.Remove(l.path)
	if err1 != nil {
		return err1
	}
	return err2
}
