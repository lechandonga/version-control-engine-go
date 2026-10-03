package vcs

import (
	"os"
	"path/filepath"
	"syscall"
)

// fileLock 基于 flock(2) 的跨进程互斥锁。同一进程内对不同 fd 再次加锁
// 会阻塞直到其它句柄释放，因此调用方须成对 Lock/Unlock，不可嵌套。
type fileLock struct {
	path string
	f    *os.File
}

func newFileLock(root, name string) *fileLock {
	return &fileLock{path: filepath.Join(root, "locks", name)}
}

func (l *fileLock) Lock() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return err
	}
	l.f = f
	return nil
}

func (l *fileLock) Unlock() error {
	if l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// TryLock 非阻塞加锁；锁被占用时返回错误而不是等待。
func (l *fileLock) TryLock() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return err
	}
	l.f = f
	return nil
}
