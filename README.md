# version-control-engine-go

一个纯本地、无外部依赖的 Go 版本控制引擎库（`vcs` 包）加命令行工具
（`cmd/vcs`）。对象使用 SHA-256 内容寻址，支持：

- 对象库 / 引用 / 暂存区 / 工作区检出；
- 提交、分支建删、三方合并（快进与合并提交、冲突现场保留与消解）；
- 变基式重放（前进 / 冲突暂停 / 继续 / 跳过 / 整体回退）；
- **操作留痕与找回**（`reflog` / `restore`）；
- **打包归档**（`pack`，幂等、可与写入并发、崩溃安全）；
- **不可达回收**（`gc --dry-run`，保留期、可重入、结果一致）。

快速开始：

```bash
go build -o vcs ./cmd/vcs
./vcs init
./vcs add <file> && ./vcs commit -m "msg"
./vcs branch create dev && ./vcs checkout dev
./vcs reflog && ./vcs pack && ./vcs gc --dry-run
```

维护能力的落盘格式、可达性与保留期规则、留痕格式与恢复流程、兼容
范围和本地验证步骤见 [`docs/maintenance.md`](docs/maintenance.md)。

测试：

```bash
go test ./...
CGO_ENABLED=1 go test -race -count=5 ./...
```
