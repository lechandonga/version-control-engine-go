# 本地版本控制引擎（Go）

一个完全落盘的、内容寻址的本地版本控制引擎，作为内部自动化流程的底座。
不依赖任何外部服务、数据库或守护进程：所有对象、引用、索引和操作状态
都保存在工作区的 `.vcs/` 目录里；历史结果可复现、可解释。

- Go 包：`github.com/lechandonga/version-control-engine-go/vcs`
- 命令行：`cmd/vcs`

## 能力

- **内容寻址对象存储**：blob/tree/commit 按 SHA-256 内容标识分桶存放，
  相同内容只保留一份；读取时强制完整性校验，截断、篡改、缺失按不同
  原因明确失败。
- **提交历史**：提交可还原完整目录快照，沿父子关系可走通历史；
  `fsck` 递归校验全仓库。
- **原子引用**：引用 / HEAD / 索引 / 操作状态全部“临时文件 + rename”
  原子更新，强杀或写入中断后不会出现半更新状态，损坏按类型报清楚。
- **分支与合并**：创建/切换/删除分支；快进如实快进；真正分叉做三方
  合并；多个候选共同祖先用确定性的虚拟合并基；同区域修改冲突、
  删除/修改冲突分别上报；本地改动与未跟踪文件不会被悄悄覆盖。
- **变更重放（rebase）**：逐个重放提交，等效变更按内容跳过（不受提交
  信息/时间戳/顺序影响）；冲突可暂停、继续或整体回退；重启后识别
  未完成重放并恢复或安全回退。

## 快速开始

```sh
go build -o bin/vcs ./cmd/vcs

mkdir demo && cd demo
vcs init
echo hello > a.txt
vcs add a.txt
vcs commit -m "first"
vcs branch feature
vcs checkout feature
vcs status
vcs merge main
vcs rebase main                 # 或 --continue / --abort / --recover
vcs log
vcs fsck
```

## 作为 Go 库使用

```go
r, err := vcs.Init("/path/to/workdir")     // 或 vcs.Open(...)
r.Add([]string{"."})
id, err := r.Commit(vcs.CommitOptions{
    Message: "msg",
    Author:  vcs.Signature{Name: "ci", Email: "ci@local", When: time.Now()},
})
r.Branch("dev"); r.Checkout("dev")
res, err := r.Merge("main", opts)          // 冲突：errors.As -> *vcs.MergeConflictError
res, err = r.Rebase(vcs.RebaseOptions{Upstream: "main"})
```

错误分类用 `errors.Is` / `errors.As` 判断：

- 对象：`ErrObjectNotFound` / `ErrObjectTruncated` / `ErrObjectTampered`
  / `ErrObjectCorrupt` / `ErrObjectTypeMismatch`
- 引用：`ErrRefNotFound` / `ErrRefCorrupt` / `ErrRefDangling` / `ErrRefType`
- 操作：`*MergeConflictError`（含 `ConflictDeleteModify` /
  `ConflictModifyModify`）、`*UnsafeOverwriteError`、`ErrLockHeld`、
  `*RebaseInProgressError`

## 本地复现与验证

```sh
# 全部单元测试
go test ./...

# 竞态检测（并发/锁用例在 -race 下稳定通过）
CGO_ENABLED=1 go test ./... -race

# 重复运行验证确定性与稳定性
CGO_ENABLED=1 go test ./... -race -count=10

# 格式化与静态检查
gofmt -l . && go vet ./...

# 全仓库完整性自检（在任一仓库目录）
vcs fsck
```

测试覆盖的关键场景：多个候选共同祖先（criss-cross）的确定性合并、
同区域 modify/modify 冲突、删除/修改冲突、重放冲突后的继续与回退、
重复变更跳过、坏对象（缺失/截断/篡改）识别、引用损坏分类、
覆盖保护、中断后恢复，以及并发提交的锁串行化（`-race`）。
关键用例会在测试日志中打印输入与判定依据。

为保证提交 ID 与期望结果可复现，库支持注入固定时间（`CommitOptions.When`
/`RebaseOptions.When`），测试统一使用固定时钟。

## 文档

- [落盘格式与持久化保证](docs/STORAGE.md)
- [分支、合并与变更重放判定规则](docs/MERGE_AND_REBASE.md)

## 兼容范围

- 语言：Go 1.25+（在 Go 1.26 上验证）。
- 平台：依赖 POSIX 语义（`rename` 原子性、`O_EXCL` 锁、`fsync`）的
  Linux/Unix 文件系统；对象与引用格式为本引擎自有格式（与 Git 不兼容）。
- 文件类型：仅支持普通文件与目录；符号链接、可执行位等扩展属性不在
  当前范围内。
- 存储格式版本：对象封装 v1（SHA-256）、索引 `VCSIDX\x01`；后续不兼容
  变更会提升魔数/版本号。
