# 长期运行维护：操作留痕、打包归档、不可达回收

本文描述本地版本控制引擎（以下简称 vcs）在仓库长期运行后提供的三项维护
能力：操作留痕与找回、打包归档（pack）、不可达对象回收（gc）。内容包括
落盘格式、判定规则、恢复流程、兼容范围，以及本地复现/验证方法。

所有元数据位于工作区下的 `.vcs/` 目录：

| 路径 | 作用 |
| --- | --- |
| `objects/xx/<剩余 62 hex>` | 松散对象（内容寻址，SHA-256，只读） |
| `packs/pack-<id>.pack` / `.idx` | 归档文件与索引（索引可缺失，自动重建） |
| `refs/heads/<name>` | 分支引用（一行提交 ID） |
| `HEAD` | 当前检出：`ref: refs/heads/<name>` 或裸提交 ID |
| `index` | 暂存区 |
| `merge-state/state.json` | 在途合并现场 |
| `rebase-state/state.json` | 在途重放现场 |
| `logs/reflog` | 操作留痕（JSON Lines） |
| `gc/plan.json` | 回收断点清单（执行成功后删除） |
| `locks/*.lock` | 跨进程互斥锁（flock） |

## 1. 对象容器格式（松散与归档通用）

```
VCSOBJ1\n
<type> <hexid> <declaredLen>\n
<payload, declaredLen 字节>
```

- `type` ∈ `blob | tree | commit`；`hexid` 恒为 payload 的 SHA-256（64 hex）。
- 读取时按失败原因分类：
  - 文件不存在 / 所有归档均无该记录 → `ObjectMissing`
  - 容器长度与声明不符（写入中断、尾部被截）→ `ObjectTruncated`
  - payload 的 SHA-256 与头部 id 不符，或读取地址与内容地址不符 → `ObjectTampered`
  - 魔数错误、头部字段非法、类型未知、payload 结构非法 → `ObjectCorrupt`
- 所有正式文件（引用、状态、归档、索引）一律“同目录临时文件 + fsync +
  rename”发布：读者要么看到旧版本要么看到新版本，进程被强杀不会留下
  半截正式文件。

## 2. 操作留痕（reflog）

### 2.1 记录格式

`logs/reflog` 是 JSON Lines：每条记录恰好一行 JSON，行间互不依赖：

```json
{"time":"2026-10-03T04:10:10.48Z","ref":"refs/heads/main","op":"commit",
 "old":"<64hex 或空>","new":"<64hex 或空>","reason":"可读原因","pid":1234}
```

- `ref` 为 `HEAD` 或完整引用名（如 `refs/heads/feature`）。
- `op` 取值：`commit`、`checkout`、`branch-create`、`branch-delete`、
  `merge`、`rebase-advance`、`rebase-pause`、`rebase-abort`、`restore`。
  所有会移动分支指针或当前检出位置的动作（提交、切换、建/删分支、合并、
  重放前进/暂停/回退）都留痕，记录时间、原因、from/to 两个位置。
- 留痕先于指针移动落盘（追加写 + fsync），随后原子发布指针。

### 2.2 查看

```
vcs reflog                                   # 全部（新 -> 旧）
vcs reflog --ref refs/heads/feature          # 只看某引用
vcs reflog --since 2026-10-01T00:00:00Z      # 按时间
vcs reflog --check                           # 单独做坏行健康检查
```

每行带行号（`L<n>`），供恢复时精确定位。

### 2.3 坏行隔离

- 单行 JSON 无法解析、缺 `ref`/`op`、目标不是合法对象 ID 时，该行标记为
  `Bad`（输出中带 `[CORRUPT LINE: ...]`），`vcs reflog --check` 按行号
  汇报原因。
- 坏行不阻断仓库打开、不影响其它记录读取，也不能作为恢复依据
  （`RestoreRef` 直接拒绝坏行）。

### 2.4 恢复流程

恢复遵循“指针要么落在旧位置，要么落在新位置”：

1. `vcs reflog --ref <ref>` 找到目标记录，记下行号 `L<n>` 与 `old`/`new`。
2. 删错分支：用 `branch-delete` 记录（其 `old` 为删除前位置）。
3. 错误回退重放/合并：用对应的 `rebase-advance`/`merge` 记录里的 `new`
   （即“当时挪到的位置”）；删分支记录的 `new` 为空表示删除。
4. 执行 `vcs restore --line <n> [--ref <name>]`。恢复先追加一条
   `restore` 留痕，再原子改写指针；目标对象已不存在时报
   `ObjectMissing`，重复执行幂等。

崩溃语义：若“留痕已写、指针未落盘”时被杀，下一次读取会把该记录标记为
`pending`（输出带 `[pending: ...]`）。`branch-create` / `branch-delete` /
`restore` 三类独立终态动作会核对指针现状；普通指针推进（commit/merge/
rebase）的历史记录不做终态校验，避免把正常的后续推进误报为悬空。

## 3. 打包归档（pack）

### 3.1 落盘格式

```
<record1><record2>...<recordN>
VCSSUM256 <sha256(以上全部字节)>\n
```

- 每条 `record` 与松散对象容器字节完全一致（见第 1 节），因此截断 /
  篡改 / 结构损坏的分类规则与松散对象一致，不会把坏数据当好数据。
- 文件名 `pack-<id>.pack`，其中 `id = sha256(对象 id 排序后拼接)`：
  对象集合相同则文件名相同，天然幂等。
- 同名 `.idx` 为加速索引（`VCSIDX1\n<id> <offset> <length>\n...`），
  仅作性能副本：缺失或损坏时顺序扫描 `.pack` 重建，不影响正确性。

### 3.2 行为与发布顺序

- `vcs pack` 收拢当前全部松散对象；先写临时 pack、再写临时 idx，
  先 rename pack 再 rename idx，最后刷新内存索引并删除已进归档的松散副本。
- 归档期间允许并发新写入：新写入对象保持松散，留待下次归档；
  重复执行不会越归档越大（集合相同直接命中已有归档，仅做幂等收尾）。
- 上层表现不变：分支、历史、工作区、在途合并/重放现场照常读取，
  提交标识（SHA-256）与快进/三方合并、重放冲突的判定结果不变。
- 崩溃恢复：未 rename 的 `.tmp-*` 文件从未发布，重开仓库时安全清理；
  已发布 pack 但未删松散时，重试归档命中同文件名并完成删除。
- 归档魔数损坏的包被隔离登记：仓库照常打开，访问其中对象得到
  `ObjectCorrupt`，而不是 `ObjectMissing` 或假数据。

## 4. 不可达回收（gc）

### 4.1 可达根

从以下根出发，沿 commit → parent/tree → tree entry → blob 做闭包遍历，
进入闭包的对象全部判为可达：

- 所有分支引用 `refs/heads/**`；
- 当前检出位置 `HEAD`（分离 HEAD 时其裸提交也是独立根）；
- 在途合并现场 `merge-state`：head / other / base 提交与每个冲突的
  base/ours/theirs 三方 blob；
- 在途重放现场 `rebase-state`：onto / original_head / queue / applied /
  暂停提交，以及暂停冲突的三方 blob；
- 操作留痕 `logs/reflog` 中出现的全部对象 id（`old` 与 `new`），
  保证“照记录恢复”永远找得回对象。

### 4.2 保留期

- 不在可达闭包中的对象为候选垃圾；只有“对象文件 mtime 早于
  `now - retain`”的松散对象才会真正删除，默认保留期 14 天
  （`vcs gc --retain 336h` 可覆盖）。
- 预览中的 `too-young` 列出不可达但仍在保留期内的对象。
- 归档中的对象没有独立 mtime，视为“已冷却”，随归档重写剔除
  （剔除对象同样必须先通过不可达判定）。

### 4.3 预览、执行与可重入

```
vcs gc --dry-run             # 只预览：reachable/unreachable/reclaimable/too-young
vcs gc                       # 确认后执行
vcs gc --retain 720h         # 自定义保留期
```

执行分三段，全部可中断重入：

1. 计算清单并落 `gc/plan.json`（记录每个待删松散对象当时的 mtime 指纹）。
2. 删除松散对象前**重新做一次可达性判定**，并核对 mtime 指纹：
   中途重新可达、文件被重写（mtime 变化）、已被删掉的对象一律跳过。
3. 需要剔除垃圾的归档走“写新归档 → 校验新归档覆盖全部保留对象 →
   删除旧归档”的顺序；新归档不完整时保守保留旧归档，绝不制造缺失。

中途被强杀后重新 `vcs gc` 会重新扫描、重新判定，不依赖旧计划做删除；
回收完成后所有可达历史的读取、合并与重放结果与回收前完全一致。

## 5. 兼容与迁移

- **已有仓库无需迁移**：旧仓库即“全松散对象、无归档、无留痕历史”的
  仓库，打开后照常工作；此后的新操作开始累计留痕。
- **不归档的老数据**：永远以松散形态保留，读取路径先查松散再查归档，
  老对象不迁移也能正常读；只有显式执行 `vcs pack` 才会收拢。
- 归档与回收均为纯增量机制，不改变任何对象 id（内容寻址），因此
  归档/回收前后提交、合并、重放的判定标识保持不变。
- 互斥锁基于 `flock(2)`（Linux/Unix 语义）；进程内另有串行锁，保证
  同进程并发调用也安全。

## 6. 本地复现与验证

```bash
go build ./...
go test ./...                                   # 全量单测
CGO_ENABLED=1 go test -race -count=5 ./...      # 竞态 + 多轮

# 手工复现
go build -o /tmp/vcs ./cmd/vcs
mkdir demo && cd demo
/tmp/vcs init
echo 1 > a && /tmp/vcs add a && /tmp/vcs commit -m c1
/tmp/vcs branch create feature
/tmp/vcs reflog
/tmp/vcs branch delete feature
/tmp/vcs reflog --ref refs/heads/feature
/tmp/vcs restore --line <branch-delete 的行号>
/tmp/vcs pack
/tmp/vcs gc --dry-run
/tmp/vcs gc
```

测试日志中的“判定依据”字样对应各条需求的验收点：删除分支找回、回退
位置找回、留痕坏行分类、归档前后读取一致、反复归档幂等、归档截断/
篡改/魔数损坏分类、可达性四类根、保留期边界、pack/gc 各中断阶段重启
重试、并发写入与维护、万级对象文件数下降至少一个数量级。
