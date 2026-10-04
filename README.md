# version-control-engine-go

本地版本控制引擎：内容寻址对象库、分支/合并/重放，以及面向长期运行的维护能力
（操作留痕与找回、打包归档、不可达回收）。

## 快速开始

```bash
go build -o vcs ./cmd/vcs
./vcs init
echo hello > a.txt && ./vcs commit -m "first"
./vcs branch feature && ./vcs switch feature
./vcs merge master
./vcs rebase master
```

## 维护命令

| 命令 | 说明 |
|---|---|
| `vcs reflog [-ref <name>]` | 查看指针移动记录（按分支或按时间） |
| `vcs recover <ref> <id>` | 按记录恢复引用，完整标识或 ≥7 位短前缀均可（原子、留痕） |
| `vcs pack` | 把松散对象归档为包文件（幂等、崩溃安全） |
| `vcs gc [-dry-run] [-grace 24h]` | 回收不可达对象（松散+已归档，先预览再动手） |

规则要点：归档与回收的先后顺序不影响结果，已归档垃圾也能被回收；
当前检出位置所在分支不能直接删除（先 `switch`）；悬空 HEAD 的老仓库
提交时会报错并给出 `recover`/`switch` 恢复指引。

详细格式、规则与恢复流程见 [docs/MAINTENANCE.md](docs/MAINTENANCE.md)。

## 测试

```bash
go test ./vcs/ -v
CGO_ENABLED=1 go test ./... -race -count=3
```
