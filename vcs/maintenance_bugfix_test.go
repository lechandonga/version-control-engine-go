package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupHistory 建立两条分支历史，供维护类用例复用。
func setupHistory(t *testing.T) (*Repo, string, string) {
	t.Helper()
	r := newRepo(t)
	writeWork(t, r, "a.txt", "a1")
	c1 := commitAll(t, r, "c1")
	if err := r.CreateBranch("other"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "a.txt", "a2")
	c2 := commitAll(t, r, "c2")
	return r, c1, c2
}

// TestGCPackedUnreachable 场景：先归档后回收，已收进包的无引用对象
// 在预览中如实列出、执行后真正清除，仓库瘦下来；可达对象不受影响。
func TestGCPackedUnreachable(t *testing.T) {
	r, c1, c2 := setupHistory(t)
	if err := r.DeleteBranch("other"); err != nil {
		t.Fatal(err)
	}
	dangling := writeLoose(t, r, "half-finished-intermediate")
	t.Logf("场景: dangling=%s, c1=%s(日志保留), c2=%s(分支/HEAD)", dangling[:8], c1[:8], c2[:8])

	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	if !r.packHas(dangling) || !r.packHas(c1) {
		t.Fatal("前置条件：dangling 与 c1 已收进归档包")
	}
	packSizeBefore := dirSize(t, r.packsDir())

	preview, err := r.GC(GCOptions{Grace: 0, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(preview.Removed, dangling) {
		t.Fatalf("预览应列出已归档的无引用对象, got %v", preview.Removed)
	}
	if contains(preview.Removed, c1) || contains(preview.Removed, c2) {
		t.Fatal("预览不得包含日志/分支引用的可达对象")
	}
	if !r.HasObject(dangling) {
		t.Fatal("DryRun 不得删除任何对象")
	}
	t.Logf("判定: 预览列出 %d 个对象(含已归档 dangling), 实际未删除", len(preview.Removed))

	res, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Removed, dangling) {
		t.Fatalf("执行应清除 dangling, got %v", res.Removed)
	}
	if r.HasObject(dangling) {
		t.Fatal("执行后已归档的垃圾对象必须真正消失")
	}
	if !r.HasObject(c1) || !r.HasObject(c2) {
		t.Fatal("可达对象（日志保留的 c1、HEAD 指向的 c2）必须仍可读")
	}
	if _, err := r.ReadCommit(c2); err != nil {
		t.Fatalf("回收后历史必须可读: %v", err)
	}
	if err := r.Recover("refs/heads/other", c1); err != nil {
		t.Fatalf("回收后应仍可按记录找回分支: %v", err)
	}
	packSizeAfter := dirSize(t, r.packsDir())
	t.Logf("判定: 垃圾已从包内清除, 包 %d -> %d 字节, 可达历史与找回正常",
		packSizeBefore, packSizeAfter)
	if packSizeAfter >= packSizeBefore {
		t.Fatal("剔除已归档垃圾后包应变小")
	}

	res2, err := r.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Removed) != 0 {
		t.Fatalf("重复回收不应再删对象, got %v", res2.Removed)
	}
}

// TestGCPackedGraceOrderIndependent 场景：保留期判定不依赖归档/回收先后顺序。
func TestGCPackedGraceOrderIndependent(t *testing.T) {
	r, _, c2 := setupHistory(t)
	dangling := writeLoose(t, r, "young-dangling")
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return fixed }
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(r.packsDir())
	if err != nil || len(ents) != 1 {
		t.Fatalf("expect 1 pack: %v", err)
	}
	if err := os.Chtimes(filepath.Join(r.packsDir(), ents[0].Name()), fixed, fixed); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return fixed.Add(time.Hour) }
	res, err := r.GC(GCOptions{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if contains(res.Removed, dangling) {
		t.Fatal("保留期内的已归档对象不得因先归档后回收被误清")
	}
	if !r.HasObject(dangling) {
		t.Fatal("保留期内对象必须仍在")
	}
	r.Now = func() time.Time { return fixed.Add(7 * 24 * time.Hour) }
	res, err = r.GC(GCOptions{Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Removed, dangling) || contains(res.Removed, c2) {
		t.Fatalf("超保留期应只清 dangling: removed=%v", res.Removed)
	}
	t.Log("判定: 已归档对象年龄按归档时间算, 顺序无关, 可达对象不受保留期影响")
}

// TestGCCorruptPackUntouched 场景：归档包里混有坏数据，回收不动该包，仓库照常可读。
func TestGCCorruptPackUntouched(t *testing.T) {
	r, _, c2 := setupHistory(t)
	dangling := writeLoose(t, r, "in-bad-pack")
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(r.packsDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("expect 1 pack: %v", err)
	}
	packPath := filepath.Join(r.packsDir(), entries[0].Name())
	data, _ := os.ReadFile(packPath)
	copy(data, "GARBAGE!!!")
	if err := os.WriteFile(packPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Reachable(); err != nil {
		t.Fatalf("坏包不应让可达性计算失败: %v", err)
	}
	res, err := r2.GC(GCOptions{Grace: 0})
	if err != nil {
		t.Fatalf("坏包不应阻断回收: %v", err)
	}
	if contains(res.Removed, dangling) || contains(res.Removed, c2) {
		t.Fatalf("损坏包中的对象（含垃圾）一律不动, got %v", res.Removed)
	}
	if _, err := os.Stat(packPath); err != nil {
		t.Fatal("损坏的归档包必须原样保留，等待人工修复")
	}
	if countFiles(t, r2.objectsDir()) != 0 {
		t.Fatal("本用例对象都在包内，松散目录应为空")
	}
	t.Log("判定: 损坏归档包整体保留, 回收安全完成, 仓库可读")
}

// TestDeleteCurrentBranchRejected 场景：删除当前所在分支被明确拒绝，
// 分支、当前位置、工作区、记录均无变化；删别的分支照常可删可找回。
func TestDeleteCurrentBranchRejected(t *testing.T) {
	r, c1, c2 := setupHistory(t)
	entriesBefore, _, _ := r.ReadAllReflog()
	workBefore := readWork(t, r, "a.txt")
	refsBefore, _ := r.ListRefs()
	headRefBefore, _ := r.HeadRef()

	err := r.DeleteBranch("master")
	bco, ok := err.(*BranchCheckedOut)
	if !ok {
		t.Fatalf("删当前分支应报 BranchCheckedOut, got %T %v", err, err)
	}
	if !strings.Contains(bco.Error(), "master") {
		t.Fatalf("报错应指出分支名且看得懂: %v", err)
	}
	refsAfter, _ := r.ListRefs()
	if len(refsAfter) != len(refsBefore) {
		t.Fatal("拒绝后分支集合不得变化")
	}
	if got, _ := r.ReadRef("refs/heads/master"); got != c2 {
		t.Fatal("拒绝后 master 指针不得变化")
	}
	if hr, _ := r.HeadRef(); hr != headRefBefore {
		t.Fatal("拒绝后当前位置不得变化")
	}
	if got := readWork(t, r, "a.txt"); got != workBefore {
		t.Fatal("拒绝后工作区不得变化")
	}
	entriesAfter, _, _ := r.ReadAllReflog()
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatal("拒绝操作不得写入记录")
	}
	t.Logf("判定: 拒绝信息=%q; 分支/位置/工作区/记录均未变化", err)

	if err := r.DeleteBranch("other"); err != nil {
		t.Fatalf("删除其他分支不应被拒绝: %v", err)
	}
	if _, err := r.ReadRef("refs/heads/other"); err == nil {
		t.Fatal("other 应已删除")
	}
	if err := r.Recover("refs/heads/other", c1); err != nil {
		t.Fatalf("应能按记录找回 other: %v", err)
	}
	if got, _ := r.ReadRef("refs/heads/other"); got != c1 {
		t.Fatal("找回后 other 应回到旧位置")
	}
}

// TestOrphanHeadOldRepoGuarded 场景：已处于“HEAD 悬空”状态的老仓库，
// 不再静默产生断链新根，而是明确报错并给出恢复指引；按指引可恢复。
func TestOrphanHeadOldRepoGuarded(t *testing.T) {
	r, _, c2 := setupHistory(t)
	if err := os.Remove(r.refPath("refs/heads/master")); err != nil {
		t.Fatal(err)
	}
	writeWork(t, r, "a.txt", "a3")
	_, err := r.Commit("would-be-root")
	oh, ok := err.(*OrphanHead)
	if !ok {
		t.Fatalf("悬空 HEAD 上提交应报 OrphanHead, got %T %v", err, err)
	}
	if !strings.Contains(oh.Error(), "recover") || !strings.Contains(oh.Error(), "switch") {
		t.Fatalf("报错应给出恢复指引: %v", err)
	}
	t.Logf("判定: 悬空提交被拒绝, 指引=%q", err)
	if err := r.Recover("refs/heads/master", c2); err != nil {
		t.Fatalf("按指引恢复应成功: %v", err)
	}
	if got, _ := r.ReadRef("refs/heads/master"); got != c2 {
		t.Fatal("恢复后 master 应回到 c2")
	}
	if head, _ := r.CurrentCommit(); head != c2 {
		t.Fatal("恢复后当前位置应回到 c2")
	}
	writeWork(t, r, "a.txt", "a3")
	c3, err := r.Commit("c3")
	if err != nil {
		t.Fatalf("恢复后提交应正常: %v", err)
	}
	c3Obj, err := r.ReadCommit(c3)
	if err != nil {
		t.Fatal(err)
	}
	if len(c3Obj.Parents) != 1 || c3Obj.Parents[0] != c2 {
		t.Fatalf("新提交必须以 c2 为父提交, got %v", c3Obj.Parents)
	}
	t.Log("判定: 恢复后提交以 c2 为父, 历史链不再断开")
}

// TestReflogToRecoverShortID 场景：从查看记录到恢复的完整链路，
// 记录中的短标识可直接用于恢复；非法/不存在/不唯一/非提交分类处理；恢复原子且留痕。
func TestReflogToRecoverShortID(t *testing.T) {
	r, c1, _ := setupHistory(t)
	entries, _, err := r.ReadReflog("refs/heads/other")
	if err != nil {
		t.Fatal(err)
	}
	var deletedPos string
	for _, e := range entries {
		if e.New != "" {
			deletedPos = e.New
		}
	}
	if deletedPos == "" {
		t.Fatal("应能从记录拿到 other 的位置")
	}
	if err := r.DeleteBranch("other"); err != nil {
		t.Fatal(err)
	}

	short := deletedPos[:8]
	if err := r.Recover("refs/heads/other", short); err != nil {
		t.Fatalf("记录里的短标识应可直接恢复: %v", err)
	}
	if got, _ := r.ReadRef("refs/heads/other"); got != c1 {
		t.Fatal("短标识恢复后应落在记录的完整位置")
	}
	logEntries, _, _ := r.ReadReflog("refs/heads/other")
	last := logEntries[len(logEntries)-1]
	if last.Op != "recover" || last.New != c1 {
		t.Fatal("恢复动作必须留痕且记录完整目标")
	}
	t.Logf("判定: 短标识 %s 解析为 %s 并原子恢复, 动作留痕", short, c1)

	before, _ := r.ReadRef("refs/heads/other")
	for _, bad := range []string{"xyz", "abc123", "g" + strings.Repeat("0", 63), "deadbeefzz"} {
		err := r.Recover("refs/heads/other", bad)
		if _, ok := err.(*RefCorrupt); !ok {
			t.Fatalf("非法标识 %q 应报 RefCorrupt, got %T %v", bad, err, err)
		}
		if got, _ := r.ReadRef("refs/heads/other"); got != before {
			t.Fatalf("非法恢复目标 %q 不得改动引用", bad)
		}
	}
	err = r.Recover("refs/heads/other", strings.Repeat("f", 8))
	if _, ok := err.(*ObjectMissing); !ok {
		t.Fatalf("不存在目标应报 ObjectMissing, got %T %v", err, err)
	}
	if got, _ := r.ReadRef("refs/heads/other"); got != before {
		t.Fatal("不存在的恢复目标不得改动引用")
	}
	blob := writeLoose(t, r, "not-a-commit")
	err = r.Recover("refs/heads/other", blob)
	if _, ok := err.(*RefCorrupt); !ok {
		t.Fatalf("非提交目标应报 RefCorrupt, got %T %v", err, err)
	}
	if got, _ := r.ReadRef("refs/heads/other"); got != before {
		t.Fatal("非提交目标不得改动引用")
	}

	// 不唯一：构造两个共享 7 位前缀的对象，用公共前缀恢复应明确报错。
	prefix := collisionPrefix(t, r)
	if prefix != "" {
		err = r.Recover("refs/heads/other", prefix)
		amb, ok := err.(*AmbiguousCommitID)
		if !ok {
			t.Fatalf("不唯一前缀应报 AmbiguousCommitID, got %T %v", err, err)
		}
		if len(amb.Candidates) < 2 {
			t.Fatal("歧义错误应给出候选列表")
		}
		if got, _ := r.ReadRef("refs/heads/other"); got != before {
			t.Fatal("歧义恢复不得改动引用")
		}
		// 用完整 ID 仍可恢复。
		if err := r.Recover("refs/heads/other", amb.Candidates[0]); err == nil {
			t.Fatal("给出完整 ID 后歧义消除；但该对象是 blob 而非 commit，仍应拒绝")
		}
		if got, _ := r.ReadRef("refs/heads/other"); got != before {
			t.Fatal("非提交目标不得改动引用")
		}
		t.Logf("判定: 歧义前缀 %s 匹配 %d 个对象并明确报错, 引用未动", prefix, len(amb.Candidates))
	}
	t.Log("判定: 查看记录→短标识恢复链路可用; 非法/不存在/非提交/歧义均不改动引用")
}

// collisionPrefix 写入内容直到两个对象共享 7 位十六进制前缀，返回公共前缀。
// 3 万次按生日悖论碰撞概率 >95%；极小概率未碰撞时返回空串，调用方跳过该子断言。
func collisionPrefix(t *testing.T, r *Repo) string {
	t.Helper()
	buckets := map[string]string{}
	for i := 0; i < 30000; i++ {
		id := writeLoose(t, r, fmt.Sprintf("collide-%d", i))
		p := id[:7]
		if other, ok := buckets[p]; ok && other != id {
			return p
		}
		buckets[p] = id
	}
	return ""
}
