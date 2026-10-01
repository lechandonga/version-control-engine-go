# 落盘格式与持久化保证

仓库所有数据都在工作区的 `.vcs/` 目录下，只使用本地文件系统，
不依赖任何外部服务或守护进程。

```
.vcs/
├── HEAD                 # 符号引用或分离头指针
├── index                # 暂存区（带校验和）
├── lock                 # 进程级互斥锁（运行期）
├── objects/             # 内容寻址对象库
│   └── ab/<cdef...>     # 前 2 位十六进制分桶
├── refs/
│   └── heads/<name>     # 本地分支引用
└── state/
    └── rebase/          # 进行中的重放状态（含起始快照）
```

## 对象格式

所有对象（blob / tree / commit）使用统一的封装：

```
header  = type SP body-length LF
body    = 类型特有内容，长度恰好为 body-length 字节
trailer = SHA-256(header || body)   # 32 字节原始摘要，附在文件末尾
```

**对象标识（ID）** 就是 `trailer` 中 SHA-256 的十六进制编码（64 个字符）。

- **blob** body：文件原始字节。
- **tree** body：若干条目直接相连（没有分隔符）。每条为
  `mode SP path NUL <32 字节哈希> NUL`。定长记录边界保证哈希中
  任意字节（包括 `0x0A`）都不会破坏解析。条目按 `path` 字节序严格升序。
  - `mode = 100644`：普通文件，哈希指向 blob
  - `mode = 040000`：目录，哈希指向另一棵 tree
- **commit** body：
  ```
  tree <hex>
  parent <hex>          # 零行（根提交）、一行或多行（合并提交）
  author <name> <<email>> <unix-nanos> <±HHMM>
  committer <name> <<email>> <unix-nanos> <±HHMM>
  <空行>
  <message，任意字节>
  ```

## 读取时的完整性校验与失败分类

每次读取对象都重新计算 SHA-256 并做结构检查，坏内容绝不会被当成
有效对象返回。失败原因互斥、可区分：

| 触发条件 | 哨兵错误 |
| --- | --- |
| 对象文件不存在 | `ErrObjectNotFound` |
| 尾部不足 32 字节 / 头部不完整 / 声明 body 长度与实际不符 | `ErrObjectTruncated` |
| 重算 SHA-256 与 trailer 不一致（内容被改动） | `ErrObjectTampered` |
| 未知类型、tree/commit body 结构无法解析 | `ErrObjectCorrupt` |
| 调用方期望的类型与实际类型不符 | `ErrObjectTypeMismatch` |

`Fsck()` 会递归校验所有对象（tree 的子项、commit 的 tree/parent）
以及全部引用，返回第一个可分类的错误。

## 去重

对象路径由内容哈希决定，相同内容写入只会落到同一路径，天然去重。
对象文件以只读权限（0444）保存，防止常规路径误改。

## 引用格式与原子更新

- 分支引用文件 `refs/heads/<name>` 的内容固定为 `<64 位 hex 提交 ID>\n`。
- `HEAD`：
  - 符号引用：`ref: refs/heads/<name>\n`
  - 分离头：`<64 位 hex>\n`

所有写操作（对象、引用、HEAD、index、rebase 状态文件）都采用
**同目录临时文件 + `fsync` + `rename` + 目录 `fsync`**。
在本地 POSIX 文件系统上 `rename` 是原子的，读者只能看到更新前或
更新后的完整内容，进程被强杀或写入中断后不会出现“写了一半”的文件；
残留的 `.tmp-*` 临时文件会被读取路径忽略。

引用读取同样分类失败：`ErrRefNotFound`（缺失）、`ErrRefCorrupt`
（空内容/非法字符/长度不对）、`ErrRefDangling`（指向的提交不存在）、
`ErrRefType`（指向非提交对象）。

## 仓库锁

`.vcs/lock` 基于 `O_CREATE|O_EXCL` 实现进程级互斥。所有会改变状态的
操作（暂存、提交、分支、切换、合并、重放）都在持锁临界区内完成，
抢锁失败立即返回 `ErrLockHeld`，避免并发写互相覆盖。

## 索引（暂存区）

```
magic  = "VCSIDX\x01"
record = pathLen(u32 小端) | path | <32 字节 blob 哈希>
checksum = SHA-256(magic || 全部 record)
```

记录按 path 升序，尾部 32 字节校验和保证索引被截断或篡改后不会被
当作有效状态使用。
