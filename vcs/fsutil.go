package vcs

import (
	"os"
	"path/filepath"
)

// writeFileAtomic 通过 “同目录临时文件 + rename” 原子写入。
// rename 在本地 POSIX 文件系统上是原子的，因此读方只能看到旧文件或新文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return syncDir(dir)
}

// syncDir 让目录项变更（create/rename/unlink）落盘。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeAll 是 os.RemoveAll 的薄封装，集中处理路径。
func removeAll(path string) error { return os.RemoveAll(path) }
