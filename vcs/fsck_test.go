package vcs

import (
	"os"
	"testing"
)

// TestFsckDetectsTamperedAndDangling 全仓库校验必须发现：
//   - 被篡改的对象（哈希不符）
//   - 树/提交引用的缺失子对象
func TestFsckDetectsTamperedAndDangling(t *testing.T) {
	r, _ := testRepo(t)
	id := commitFiles(t, r, 1, "c", map[string]string{"a": "1", "sub/b": "2"})
	assertNoError(t, r.Fsck(), "clean fsck")

	// 篡改 a 的 blob：替换对象文件内容但保持路径（先解除只读）。
	c, _ := r.ReadCommit(id)
	tree, err := r.flattenTree(c.Tree)
	assertNoError(t, err, "flatten")
	blobPath := r.objects.objectPath(tree["a"])
	raw, err := os.ReadFile(blobPath)
	assertNoError(t, err, "read raw")
	bad := append([]byte{}, raw...)
	bad[10] ^= 0xff
	assertNoError(t, os.Chmod(blobPath, 0o644), "chmod")
	assertNoError(t, os.WriteFile(blobPath, bad, 0o644), "tamper")
	err = r.Fsck()
	t.Logf("input=篡改blob内容 判定依据=递归校验哈希 => err=%v", err)
	assertErrorIs(t, err, ErrObjectTampered, "fsck must detect tamper")

	// 恢复后再删除一个被引用对象，应报 not-found。
	assertNoError(t, os.WriteFile(blobPath, raw, 0o644), "restore")
	subBlob := r.objects.objectPath(tree["sub/b"])
	assertNoError(t, os.Chmod(subBlob, 0o644), "chmod2")
	assertNoError(t, os.Remove(subBlob), "remove referenced blob")
	err = r.Fsck()
	t.Logf("input=删除树引用的blob 判定依据=递归读取子对象 => err=%v", err)
	assertErrorIs(t, err, ErrObjectNotFound, "fsck must detect missing child")
}

// TestFsckDetectsBrokenParent 删除提交的父对象后 Fsck 应失败。
func TestFsckDetectsBrokenParent(t *testing.T) {
	r, _ := testRepo(t)
	c1 := commitFiles(t, r, 1, "c1", map[string]string{"f": "1"})
	c2 := commitFiles(t, r, 2, "c2", map[string]string{"f": "2"})
	_ = c2

	p := r.objects.objectPath(c1)
	assertNoError(t, os.Chmod(p, 0o644), "chmod")
	assertNoError(t, os.Remove(p), "remove parent commit")
	err := r.Fsck()
	assertErrorIs(t, err, ErrObjectNotFound, "missing parent commit")
}
