# Log Recover Tool

从 SeaweedFS filer log 文件恢复 filer entries 的工具。支持log完整目录恢复。

## 功能特性

### 核心功能
- 读取和解析 filer log 文件
- 重放文件事件（create, update, delete）
- 构建最终的 filer entries 状态
- 生成与 `fs.meta.save` 兼容的 meta 文件
- 支持跳过已删除的文件
- 详细的处理日志输出

### 完整的恢复工具链
1. **日志下载** (`download_logs`): 从 SeaweedFS filer 下载完整的 log 目录
2. **完整目录恢复** (`log_recover_all`): 从整个 log 目录恢复，支持跨文件事件合并

### 高级特性
- **跨文件事件合并**: 自动合并同一文件在不同 log 文件中的事件
- **时间序列处理**: 按时间戳正确排序和处理所有事件
- **内存优化**: 支持大型数据集的批处理和内存限制
- **完整的 chunk 重建**: 正确重建文件的所有数据块信息

## 使用方法

### 基本用法

```bash
# 1. 下载所有 log 文件
./download_logs -filer=http://192.168.2.39:8888 -output=downloaded_logs -v

# 2. 从下载的 logs 恢复
./log_recover_all -logdir=downloaded_logs -output=recovered.meta -v

# 3. 恢复到 filer
weed shell
> fs.meta.load recovered.meta
```
