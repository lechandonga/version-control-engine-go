package vcs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContentDedup 相同内容重复写入只产生一个对象文件。
func TestContentDedup(t *testing.T) {
	r, _ := testRepo(t)
	data := []byte("hello world\n")
	id1, err := r.writeBlob(data)
	assertNoError(t, err, "writeBlob 1")
	id2, err := r.writeBlob(append([]byte{}, data...))
	assertNoError(t, err, "writeBlob 2")
	if id1 != id2 {
		t.Fatalf("identical content produced different ids: %s vs %s", id1, id2)
	}
	matches, _ := filepath.Glob(filepath.Join(r.Root(), "objects", "*", "*"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 object file, got %d", len(matches))
	}

	got, err := r.readBlob(id1)
	assertNoError(t, err, "readBlob")
	if !bytes.Equal(got, data) {
		t.Fatalf("blob roundtrip mismatch")
	}
}

// TestObjectIntegrityFailures 覆盖截断、篡改、缺失三类可区分失败。
func TestObjectIntegrityFailures(t *testing.T) {
	r, _ := testRepo(t)
	id, err := r.writeBlob([]byte("payload-content"))
	assertNoError(t, err, "writeBlob")
	path := r.objects.objectPath(id)
	good, err := os.ReadFile(path)
	assertNoError(t, err, "read object")

	t.Run("missing", func(t *testing.T) {
		missing := "0000000000000000000000000000000000000000000000000000000000000000"
		_, err := r.objects.read(missing)
		t.Logf("input=missing-id(%s) 判定依据=文件不存在 => ErrObjectNotFound; err=%v", missing[:8], err)
		assertErrorIs(t, err, ErrObjectNotFound, "missing object")
	})

	t.Run("truncated", func(t *testing.T) {
		// 使用合法的 64-hex ID（在该桶下另建文件）。
		badID := id[:2] + strings.Repeat("0", IDLen-2)
		badPath := r.objects.objectPath(badID)
		// 构造一个声明长度大于实际 body 的对象。
		fake := append([]byte("blob 999\n"), []byte("x")...)
		fake = append(fake, make([]byte, 32)...)
		if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(badPath, fake, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := r.objects.read(badID)
		t.Logf("input=声明body长度999实际1 判定依据=长度不符 => ErrObjectTruncated; err=%v", err)
		assertErrorIs(t, err, ErrObjectTruncated, "truncated object")
	})

	t.Run("tampered", func(t *testing.T) {
		bad := append([]byte{}, good...)
		// 翻转 body 中第一个字节（header 之后），保持 trailer 不变。
		bad[10] ^= 0xff
		_ = os.Chmod(path, 0o644)
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := r.objects.read(id)
		t.Logf("input=翻转body首字节 判定依据=重算SHA256与trailer不符 => ErrObjectTampered; err=%v", err)
		assertErrorIs(t, err, ErrObjectTampered, "tampered object")
	})

	t.Run("tampered_trailer", func(t *testing.T) {
		bad := append([]byte{}, good...)
		bad[len(bad)-1] ^= 0xff
		_ = os.Chmod(path, 0o644)
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := r.objects.read(id)
		assertErrorIs(t, err, ErrObjectTampered, "tampered trailer")
	})
}

// TestTreeBinaryHashSafety 哈希中包含 0x0A 时 tree 仍可正确往返。
func TestTreeBinaryHashSafety(t *testing.T) {
	// 构造一个 blob，其哈希恰好含换行字节是概率事件；
	// 这里直接验证编码/解析对任意 32 字节哈希健壮。
	entries := []TreeEntry{
		{Mode: ModeRegular, Path: "a", ID: "0a0a0a0a00000000000000000000000000000000000000000000000000000000"},
		{Mode: ModeDir, Path: "b", ID: "ff0aff0000000000000000000000000000000000000000000000000000000000"},
	}
	body := encodeTree(entries)
	got, err := parseTreeBody(body)
	assertNoError(t, err, "parseTreeBody")
	if len(got.Entries) != 2 || got.Entries[0].Path != "a" || got.Entries[1].Path != "b" {
		t.Fatalf("tree entries mismatch: %+v", got.Entries)
	}
}

// TestSnapshotRestoreCommitHistory 提交可还原完整快照并沿父子走通历史。
func TestSnapshotRestoreCommitHistory(t *testing.T) {
	r, _ := testRepo(t)
	c1 := commitFiles(t, r, 1, "first", map[string]string{"a.txt": "a1\n", "d/b.txt": "b1\n"})
	c2 := commitFiles(t, r, 2, "second", map[string]string{"a.txt": "a2\n"})

	head, err := r.HeadCommit()
	assertNoError(t, err, "HeadCommit")
	if head != c2 {
		t.Fatalf("head=%s want %s", head, c2)
	}

	// 沿父子关系走历史。
	c2obj, err := r.ReadCommit(c2)
	assertNoError(t, err, "ReadCommit c2")
	if len(c2obj.Parents) != 1 || c2obj.Parents[0] != c1 {
		t.Fatalf("parent link broken: %+v", c2obj.Parents)
	}

	// 还原 c1 完整快照。
	c1obj, _ := r.ReadCommit(c1)
	snap1, err := r.flattenTree(c1obj.Tree)
	assertNoError(t, err, "flatten c1")
	if len(snap1) != 2 {
		t.Fatalf("c1 snapshot should contain 2 files, got %d", len(snap1))
	}
	a1, err := r.readBlob(snap1["a.txt"])
	assertNoError(t, err, "read a.txt blob")
	if string(a1) != "a1\n" {
		t.Fatalf("a.txt should be a1, got %q", a1)
	}

	// Fsck 全量校验通过。
	assertNoError(t, r.Fsck(), "Fsck")
}

// TestRefCorruptionCategories 引用损坏按 not-found/corrupt/dangling/wrong-type 分类。
func TestRefCorruptionCategories(t *testing.T) {
	r, _ := testRepo(t)
	cid := commitFiles(t, r, 1, "c", map[string]string{"f": "v"})

	t.Run("not found", func(t *testing.T) {
		_, err := r.readRef("nope")
		assertErrorIs(t, err, ErrRefNotFound, "missing ref")
	})

	t.Run("corrupt", func(t *testing.T) {
		p := r.refPath("bad")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not-a-hash\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := r.readRef("bad")
		t.Logf("input=非法十六进制 判定依据=内容不符合64hex => ErrRefCorrupt; err=%v", err)
		assertErrorIs(t, err, ErrRefCorrupt, "corrupt ref")
	})

	t.Run("dangling", func(t *testing.T) {
		p := r.refPath("dangling")
		missing := "1111111111111111111111111111111111111111111111111111111111111111"
		if err := os.WriteFile(p, []byte(missing+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := r.readRef("dangling")
		t.Logf("input=合法hash但对象缺失 判定依据=对象读取NotFound => ErrRefDangling; err=%v", err)
		assertErrorIs(t, err, ErrRefDangling, "dangling ref")
	})

	t.Run("wrong type", func(t *testing.T) {
		// 用 blob ID 作为引用目标。
		blobID, err := r.writeBlob([]byte("x"))
		assertNoError(t, err, "writeBlob")
		p := r.refPath("wrongtype")
		if err := os.WriteFile(p, []byte(blobID+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = r.readRef("wrongtype")
		t.Logf("input=引用指向blob 判定依据=对象类型!=commit => ErrRefType; err=%v", err)
		assertErrorIs(t, err, ErrRefType, "wrong-type ref")
	})
	_ = cid
}

// TestAtomicRefUpdate 引用文件永远是完整内容（rename 原子性）。
func TestAtomicRefUpdate(t *testing.T) {
	r, _ := testRepo(t)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, commitFiles(t, r, i, "c", map[string]string{"f": string(rune('a' + i))}))
	}
	// 最终引用必须指向最后一个提交且内容完整。
	got, err := r.readRef("main")
	assertNoError(t, err, "readRef after updates")
	if got != ids[4] {
		t.Fatalf("ref=%s want %s", got, ids[4])
	}
	// 对象目录不应残留临时文件。
	err = filepath.Walk(filepath.Join(r.Root(), "objects"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && bytes.HasPrefix([]byte(info.Name()), []byte(".tmp-")) {
			t.Fatalf("leftover temp object file: %s", path)
		}
		return nil
	})
	assertNoError(t, err, "walk objects")
}

func TestTypeMismatch(t *testing.T) {
	r, _ := testRepo(t)
	blobID, _ := r.writeBlob([]byte("x"))
	_, err := r.ReadCommit(blobID)
	if !errors.Is(err, ErrObjectTypeMismatch) {
		t.Fatalf("expected type mismatch, got %v", err)
	}
}
