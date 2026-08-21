# dat_recover — 从 volume .dat 恢复 filer 元数据

## 用途

filer 元数据存储（如 RocksDB）损坏或丢失后，利用 volume server 上的 `.dat` 文件恢复出 filer 的目录结构和文件条目。

原理：filer 的系统日志（`topics/.system/log`）以 needle 形式追加存储在**collection 为空** 的 volume 中。每个 needle 是一次日志批量 flush，内含多条
filer 事件。本工具扫描这些 `.dat`，重放全部事件，重建每个路径的最新状态，输出 `weed shell` `fs.meta.load` 兼容的 meta 文件。

文件数据本身仍在原有 volume 中，meta load 回 filer 后对象即可重新访问。

## 使用步骤

### 1. 找出存日志的 volume

collection 为空、且包含 filer 日志 needle 的 volume。可通过 master 的volume 查询确认，或直接把候选 `.dat` 逐个跑一遍（不是日志 volume 的会 0 事件）。

### 2. 准备 dat 列表文件（多 volume 恢复）

每行一个 `.dat` 的完整路径，volume id 从文件名提取（如 `/data/3.dat` → 3）：

```
/data/1.dat
/data/2.dat
/data/3.dat
```

支持空行和 `#` 注释行。

### 3. 执行恢复

```bash
# 多 volume（推荐）
dat_recover -datList=/data/list.txt -output=/data/recovered.meta

# 单 volume
dat_recover -dat=/data/3.dat -volumeId=3 -output=/data/recovered.meta
```

参数说明：

| 参数 | 默认 | 说明 |
|------|------|------|
| `-datList` | | dat 路径列表文件，每行一个，volume id 从文件名提取 |
| `-dat` | | 单个 dat 文件路径（与 datList 二选一） |
| `-volumeId` | | 配合 -dat 使用的 volume id |
| `-output` | 自动命名 | 输出 meta 文件路径 |
| `-v` | false | 打印事件明细（CREATE/UPDATE/DELETE/RENAME） |
| `-skip-deleted` | true | 最终不输出已删除的文件 |
| `-include-uploads` | false | 包含 `.uploads/`（S3 multipart 中间文件），默认排除 |

### 4. 恢复到 filer（weed shell）

```bash
weed shell   # 连接到集群，或在 master 容器内执行
> fs.meta.load -v=false /path/to/recovered.meta
> fs.ls -l /buckets/<bucket>/

echo "fs.meta.load -v=false /path/to/recovered.meta" | kubectl exec -i seaweed-master-0 -- weed shell
```

建议先在测试环境验证 meta load 与目录结构，再对目标 filer 操作。

## 状态合并规则

- 每个路径按事件时间戳（`LogEntry.TsNs`）合并出最新状态，volume 扫描顺序不影响结果
- rename/move：新路径生效，旧路径标记删除
- chunks 按文件内 offset 合并去重（相同 offset 取较新的 ModifiedTsNs），文件大小按合并后的 chunk 覆盖范围计算
- 最终输出时过滤：已删除路径、`.uploads/` 中间文件（可用 -include-uploads 打开）
- 输出条目排序：目录在前（按路径排序），保证 load 时父目录先于子文件创建

## 注意事项

- 工具对 `.dat` **只读**，可在线对运行中的 volume server 数据目录执行
- 输出 meta 文件写到 PVC 挂载目录（/data）便于拷出，或写到 /tmp 后用`kubectl cp` 拷出
- 若 volume server 的 PVC 为 ReadWriteOnce，恢复 pod 需与 volume server调度到同一节点
- 恢复覆盖范围 = 日志 volume 中保留的事件时间范围；早于日志最早期的事件无法恢复
- load 后请抽查若干对象实际读取，确认 chunk 引用有效

## 常见问题

**Q: 怎么确认一个 .dat 是日志 volume？**
跑 `dat_recover -dat=xx.dat -volumeId=N`，看 summary 中 `Events processed`是否大于 0。普通数据 volume 的 needle 不是 LogBuffer 格式，会大量 skipped 或 0 事件。

**Q: 恢复出的文件读不到内容？**
元数据指向的 chunk 所在 volume 必须仍存在于集群。若对应 volume 已被删除或 vacuum，该文件内容不可恢复，只能恢复元数据条目。
