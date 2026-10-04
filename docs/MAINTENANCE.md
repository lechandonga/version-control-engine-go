# 长期运行维护：留痕找回、打包归档、不可达回收

本文档说明仓库维护能力的落盘格式、规则与恢复流程，以及如何本地复现验证。

## 1. 操作留痕与找回

### 记录格式

所有移动分支指针或检出位置的操作（`commit`、`switch`、`branch`、`branch-delete`、
`merge`、`rebase`、`recover`）都会追加一条 JSON 记录到 `.vcs/logs/<引用路径>`：

```json
{"time":"2026-10-03T04:33:14Z","op":"commit","ref":"refs/heads/master","old":"<64hex>","new":"<64hex>","msg":"c1"}
```

- 分支移动写入该分支自己的日志；HEAD 依附该分支时同步写入 `.vcs/logs/HEAD`。
- 删除分支时 `new` 为空，`old` 即删除前位置——这是找回的依据。
- 每条记录包含：时间、操作类型、引用、旧位置、新位置、说明。

### 查看

```bash
vcs reflog                      # 全部记录，按时间排序
vcs reflog -ref refs/heads/dev  # 只看某个分支
```

### 恢复流程

```bash
vcs reflog -ref refs/heads/feature   # 找到删除/回退前的位置（记录的 old/new）
vcs recover refs/heads/feature <commit-id>
```

**目标标识可以直接用记录里展示的短标识。** `reflog` 输出里 `old`/`new` 列是
8 位短标识，`recover` 会自动把它解析为完整提交（在松散对象和归档包里一起查找）：

- 短标识唯一匹配：直接恢复；
- 匹配到多个对象：报 `ambiguous commit id prefix`，加几位再试；
- 格式非法（非十六进制、不足 4 位）：报 `invalid commit id`；
- 合法但不存在：报 `object missing`。

以上失败都不会改动任何引用。需要完整标识时，也可以直接看
`.vcs/logs/<引用路径>` 里的原始 JSON 记录。

恢复通过 临时文件 + fsync + rename 原子落盘：进程被强杀后，指针要么停在旧值，
要么停在新值，不会停在中间。恢复动作本身也会留痕（`op=recover`）。

### 损坏隔离

日志逐行解析。某一行损坏时，该行被单独识别为 `ReflogCorrupt`（含文件名与行号），
其余记录照常可用；日志损坏不影响打开仓库、提交、归档或回收。

## 2. 打包归档

### 落盘格式

`vcs pack` 把 `.vcs/objects/` 下的松散对象收拢为 `.vcs/packs/pack-<hash>.pack`：

```
magic   "VCSPACK1\n"        9 字节
count   uint64 BE           条目数
entry   { 64 字节十六进制 ID, 2 字节类型长度, 类型, 8 字节负载长度, 负载 } × count
footer  32 字节             以上全部内容的 SHA-256
```

- 包名由排序后的对象 ID 列表哈希决定：**同一批内容反复归档得到同一个包名，天然幂等**，
  不会越归档越大。
- 写入流程：临时文件（`pack-*.tmp`）→ fsync → rename；松散文件只在包落盘后才删除。
  任何时刻崩溃，对象要么在松散文件、要么在包里，历史不会读不了；残留的 `.tmp`
  会在下次归档时清理。
- 归档期间允许并发写入：新对象照常写松散文件，归档只处理扫描时看到的集合。

### 损坏分类

归档文件与普通对象走同一套分类错误：

| 情况 | 错误类型 |
|---|---|
| 包文件被截断（长度不足/条目不完整） | `ObjectTruncated` |
| 内容被篡改（footer 校验和或负载哈希不符） | `ObjectTampered` |
| 头部非法/结构损坏 | `ObjectCorrupt` |
| 对象不存在 | `ObjectMissing` |

损坏的松散对象不会被收进包，而是留在原地并在归档结果中报告，错误分类不丢失。

## 3. 不可达回收

### 可达性规则

以下位置引用的对象（含其祖先链上的 commit/tree/blob）一律视为可达：

1. 所有分支引用（`refs/heads/*`）与 HEAD 当前位置；
2. 在途重放现场（`.vcs/REBASE_STATE`：onto/original/current/remaining）；
3. 在途合并现场（`.vcs/MERGE_STATE`：theirs/base）；
4. 全部操作记录中出现过的位置（保证找回前不会被回收）。

### 保留期

修改时间距今不足保留期（默认 24h，`-grace` 可调）的对象一律保留，
避免“刚提交完、引用尚未落稳”的对象被误判为垃圾。
对已经收进归档包的对象，保留期按**所在包的修改时间**保守判定：
包在保留期内则整包保留。因此“先归档再回收”和“先回收再归档”两种顺序
都不会误清保留期内的对象。

### 使用

```bash
vcs gc -dry-run            # 只预览将清理的对象
vcs gc -grace 24h          # 确认后执行
```

### 归档后回收的语义

回收**不挑对象存放位置**：松散对象和已收进归档包的对象一视同仁。
归档包里的无引用对象通过“重写剔除”回收——把要保留的条目写入新包
（临时文件 + fsync + rename），落盘后再删除旧包；整包都是垃圾时直接删包。
因此：

- 先 `pack` 再 `gc`：预览（`-dry-run`）如实列出包内将清理的对象，确认后真正
  清掉，仓库体积随之下降；
- 崩溃安全：重写中途被强杀，任意时刻对象至少在旧包或新包之一里，历史不会
  读不了；重试会基于重新扫描的状态继续，结果幂等；
- 损坏的归档包（截断/篡改/头部非法）保持原样不动，回收不会碰它，也不影响
  其余包的回收与正常读取；
- 与正常写入并发：新对象照常写松散文件，回收只处理扫描时看到的集合。

回收每次运行都重新计算可达性，逐个文件删除，**中断可重入**：上次跑到一半被强杀，
重试只会补删剩余对象，不会误删。回收完成后所有历史的读取、合并、重放结果
与回收前完全一致。

## 4. 当前分支保护

删除**当前所在分支**会被明确拒绝：

```
$ vcs unbranch master
vcs: cannot delete current branch: master (switch to another branch first)
```

拒绝时分支、HEAD、工作区、操作记录都不发生任何变化。删除其他分支照常可用，
且能按记录找回。请先 `vcs switch` 到别的分支再删。

**老仓库的悬空 HEAD**：历史版本可能已经把当前分支删掉（HEAD 指向一个不存在
的分支）。在这种仓库上提交会明确报错而不是静默产生无父新根：

```
vcs: HEAD points to missing branch refs/heads/master; recover it with:
vcs recover refs/heads/master <commit-id> (see: vcs reflog -ref refs/heads/master)
```

按指引从记录里找到删除前位置恢复（短标识即可），之后提交照常接在原历史之后。

## 5. 兼容范围

- **已有仓库无需迁移**：维护能力是纯增量的。老仓库打开后即可直接使用
  `reflog`/`pack`/`gc`；历史遗留的松散对象照常读取，第一次 `pack` 时会被收拢。
- **不归档的老数据**：永远可以读。读取顺序是“先松散、后归档”，归档只是换一种
  存放形式，对象 ID、提交标识、合并与重放的判定结果都不变。
- **不维护也可以**：不跑 `pack`/`gc` 不影响任何正常功能，只是文件数持续增长。

## 6. 本地复现与验证

```bash
# 全部单元测试（含场景与判定日志）
go test ./vcs/ -v

# 竞态检测下多轮稳定性（并发/中断相关用例）
CGO_ENABLED=1 go test ./... -race -count=3

# 命令行手动复现
go build -o /tmp/vcs ./cmd/vcs
cd /path/to/repo
/tmp/vcs init && echo hi > a.txt && /tmp/vcs commit -m c1
/tmp/vcs branch dev && /tmp/vcs unbranch dev
/tmp/vcs reflog -ref refs/heads/dev        # 找到删除前位置（短标识即可）
/tmp/vcs recover refs/heads/dev <短标识>    # 找回
/tmp/vcs pack                              # 归档
/tmp/vcs gc -dry-run                       # 预览回收
/tmp/vcs gc                                # 确认回收（含已归档的无引用对象）

# 当前分支保护
/tmp/vcs unbranch master                   # 拒绝：cannot delete current branch
```

测试覆盖场景（日志中可见判定依据）：

- 按记录找回被删分支 / 回退位置（`TestRecoverDeletedBranch`、`TestRecoverAfterRebaseAbort`）
- 查看记录到恢复的完整链路：短标识直接可用（`TestRecoverWithShortID`、`TestRecoverShortIDAfterPack`）；
  非法 / 不存在 / 不唯一目标报错且引用不变（`TestRecoverInvalidTargets`）
- 记录损坏的分类识别与隔离（`TestReflogCorruptIsolation`）
- 删除当前所在分支被拒绝且现场不变（`TestDeleteCurrentBranchRejected`）；
  删除其他分支可找回（`TestDeleteOtherBranchAndRecover`）；
  悬空 HEAD 的老仓库提交明确报错并给出恢复指引（`TestDanglingHeadCommit`）
- 归档前后读取一致（`TestPackReadConsistency`）、反复归档幂等（`TestPackIdempotent`）
- 归档文件截断/篡改/头部损坏的分类报错（`TestPackCorruptionClassification`）
- 可达性判断：在途重放/合并现场、保留期边界、操作记录根（`TestGCReachability`、`TestGCInFlightState`、`TestGCGracePeriod`）
- 先归档后回收：包内无引用对象预览列出并真正清掉、可达对象不动、纯垃圾包整体删除
  （`TestGCPackThenGC`、`TestGCPackOnlyGarbage`）；保留期对已归档对象同样生效（`TestGCPackGracePeriod`）；
  损坏归档包隔离不动（`TestGCPackCorruptUntouched`）
- 归档/回收中途强杀后重启可用、重试成功（`TestPackCrashRecovery`、`TestGCInterruptReentrant`）
- 回收重写归档包中途强杀后重试稳定（`TestGCPackRewriteCrashReentrant`）
- 维护与正常写入并发稳定性（`TestPackConcurrentWrites`、`TestGCPackConcurrentWrites`）
- 上万对象规模的文件数压缩与读耗时（`TestPackScaleReduction`）
