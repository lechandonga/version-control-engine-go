package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := Init(dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return r, dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorkdirFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func commitAll(t *testing.T, r *Repo, dir, msg string) string {
	t.Helper()
	if err := r.AddAll(); err != nil {
		t.Fatalf("addall: %v", err)
	}
	id, err := r.Commit(CommitOptions{Message: msg, AllowEmpty: true})
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return id
}

func countFiles(t *testing.T, root, sub string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func looseCount(t *testing.T, r *Repo) int {
	t.Helper()
	n, err := countLoose(r.root)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func assertErrorType(t *testing.T, err error, target string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", target)
	}
	check := func(name string, ok bool) {
		if ok {
			t.Logf("场景判定: 期望 %s -> 实际匹配 %s（%v）", target, name, err)
		}
	}
	var (
		missing *ObjectMissing
		trunc   *ObjectTruncated
		tamper  *ObjectTampered
		corrupt *ObjectCorrupt
	)
	if errors.As(err, &missing) {
		check("ObjectMissing", true)
		if target != "missing" {
			t.Fatalf("want %s got missing: %v", target, err)
		}
		return
	}
	if errors.As(err, &trunc) {
		check("ObjectTruncated", true)
		if target != "truncated" {
			t.Fatalf("want %s got truncated: %v", target, err)
		}
		return
	}
	if errors.As(err, &tamper) {
		check("ObjectTampered", true)
		if target != "tampered" {
			t.Fatalf("want %s got tampered: %v", target, err)
		}
		return
	}
	if errors.As(err, &corrupt) {
		check("ObjectCorrupt", true)
		if target != "corrupt" {
			t.Fatalf("want %s got corrupt: %v", target, err)
		}
		return
	}
	if target != "other" {
		t.Fatalf("want %s got %v", target, err)
	}
}

func allHistoryIDs(t *testing.T, r *Repo, head string) []string {
	t.Helper()
	var out []string
	cur := head
	for cur != "" {
		out = append(out, cur)
		c, err := r.ReadCommit(cur)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	return out
}

func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." || strings.HasPrefix(rel, ".vcs") {
			if rel == ".vcs" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func fixedTime(n int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, n, 0, time.UTC)
}
