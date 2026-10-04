package vcs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestRecoverWithShortID 场景：从查看记录到恢复的完整链路——
// 记录里展示的短标识可以直接拿来恢复，不用再翻原始日志文件。
func TestRecoverWithShortID(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	commitAll(t, r, "c1")
	if err := r.CreateBranch("feature"); err != nil {
		t.Fatal(err)
	}
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "f.txt", "work")
	want := commitAll(t, r, "feature-work")
	if err := r.Checkout("master"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteBranch("feature"); err != nil {
		t.Fatal(err)
	}

	// 第一步：查看记录，拿到删除前位置（用户看到的是短标识）。
	entries, _, err := r.ReadReflog("refs/heads/feature")
	if err != nil {
		t.Fatal(err)
	}
	var lastPos string
	for _, e := range entries {
		if e.Op == "branch-delete" {
			lastPos = e.Old
		}
	}
	if lastPos != want {
		t.Fatalf("记录的位置 %s != 实际 %s", lastPos, want)
	}
	short := shortID(lastPos) // 与命令行输出一致的 8 位短标识
	t.Logf("场景: 记录展示短标识 %s, 直接用于恢复", short)

	// 第二步：短标识直接恢复。
	if err := r.Recover("refs/heads/feature", short); err != nil {
		t.Fatalf("短标识恢复应可用: %v", err)
	}
	got, err := r.ReadRef("refs/heads/feature")
	if err != nil || got != want {
		t.Fatalf("恢复后指向 %s, 期望 %s (err=%v)", got, want, err)
	}
	// 恢复动作留痕，且记录的是完整标识。
	entries, _, _ = r.ReadReflog("refs/heads/feature")
	last := entries[len(entries)-1]
	if last.Op != "recover" || last.New != want {
		t.Fatalf("恢复应留痕且记录完整标识: %+v", last)
	}
	if err := r.Checkout("feature"); err != nil {
		t.Fatal(err)
	}
	if readWork(t, r, "f.txt") != "work" {
		t.Fatal("恢复后历史可读")
	}
	t.Log("判定: 查看记录 -> 短标识恢复链路可用, 恢复留痕")
}

// TestRecoverInvalidTargets 场景：非法、不存在、不唯一的目标都报错，
// 且引用保持原样不动。
func TestRecoverInvalidTargets(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")

	assertUnchanged := func(step string) {
		t.Helper()
		if got, err := r.ReadRef("refs/heads/master"); err != nil || got != c1 {
			t.Fatalf("%s: 引用不应变化: %s err=%v", step, got, err)
		}
	}

	// 非法格式。
	for _, bad := range []string{"not-a-commit", "abc", "zzzz", ""} {
		err := r.Recover("refs/heads/master", bad)
		var inv *InvalidID
		if !errors.As(err, &inv) {
			t.Fatalf("非法目标 %q 应返回 InvalidID, got %v", bad, err)
		}
		assertUnchanged("非法目标")
	}
	// 不存在：完整标识但对象库里没有。
	missing := strings.Repeat("deadbeef", 8)
	err := r.Recover("refs/heads/master", missing)
	var om *ObjectMissing
	if !errors.As(err, &om) {
		t.Fatalf("不存在目标应返回 ObjectMissing, got %v", err)
	}
	assertUnchanged("不存在目标")
	// 不存在：合法短前缀但无匹配。
	if _, err := r.ResolveID("ffff"); err == nil {
		// ffff 恰好存在的概率可忽略；若存在则跳过该子项。
		t.Log("ffff 前缀恰好存在, 跳过")
	} else {
		var om *ObjectMissing
		if !errors.As(err, &om) {
			t.Fatalf("无匹配短前缀应返回 ObjectMissing, got %v", err)
		}
	}

	// 不唯一：构造两个共享 4 位前缀的对象。
	seen := map[string]string{}
	var prefix string
	for i := 0; prefix == "" && i < 200000; i++ {
		id := writeLoose(t, r, fmt.Sprintf("ambig-%d", i))
		p := id[:4]
		if _, ok := seen[p]; ok {
			prefix = p
		}
		seen[p] = id
	}
	if prefix == "" {
		t.Skip("未能构造前缀冲突")
	}
	err = r.Recover("refs/heads/master", prefix)
	var amb *AmbiguousID
	if !errors.As(err, &amb) {
		t.Fatalf("不唯一前缀应返回 AmbiguousID, got %v", err)
	}
	t.Logf("场景: 前缀 %s 匹配 %d 个对象, 拒绝恢复", prefix, len(amb.Matches))
	assertUnchanged("不唯一目标")
	t.Log("判定: 非法/不存在/不唯一目标均报错且引用不变")
}

// TestRecoverShortIDAfterPack 场景：目标对象已归档时，短标识照样能解析恢复。
func TestRecoverShortIDAfterPack(t *testing.T) {
	r := newRepo(t)
	writeWork(t, r, "a.txt", "1")
	c1 := commitAll(t, r, "c1")
	writeWork(t, r, "a.txt", "2")
	c2 := commitAll(t, r, "c2")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover("refs/heads/master", c1[:8]); err != nil {
		t.Fatalf("已归档对象的短标识恢复: %v", err)
	}
	if got, _ := r.ReadRef("refs/heads/master"); got != c1 {
		t.Fatalf("恢复后应为 %s, got %s", c1[:8], got[:8])
	}
	if _, err := r.ResolveID(c2[:8]); err != nil {
		t.Fatal(err)
	}
	t.Log("判定: 已归档对象的短标识可解析可恢复")
}
