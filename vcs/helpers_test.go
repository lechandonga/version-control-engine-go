package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testClock 是固定时钟，保证提交 ID 与测试结果可复现。
func testClock(n int) time.Time {
	return time.Unix(1700000000+int64(n)*60, 0).UTC()
}

func testAuthor() Signature {
	return Signature{Name: "t", Email: "t@x", When: testClock(0)}
}

// testRepo 在临时目录创建仓库。
func testRepo(t *testing.T) (*Repository, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r, dir
}

func writeWork(t *testing.T, dir, path, content string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWork(t *testing.T, dir, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func removeWork(t *testing.T, dir, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
		t.Fatal(err)
	}
}

// commitFiles 是测试便捷函数：写文件 -> add -> commit。
func commitFiles(t *testing.T, r *Repository, n int, msg string, files map[string]string) string {
	t.Helper()
	dir := r.WorkDir()
	for p, c := range files {
		writeWork(t, dir, p, c)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	if err := r.Add(paths); err != nil {
		t.Fatalf("Add: %v", err)
	}
	id, err := r.Commit(CommitOptions{
		Message: msg,
		Author:  Signature{Name: "t", Email: "t@x", When: testClock(n)},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return id
}

func assertErrorIs(t *testing.T, err error, target error, ctx string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: expected error %v, got %v", ctx, target, err)
	}
}

func assertNoError(t *testing.T, err error, ctx string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", ctx, err)
	}
}
