# 监控存储容量对比

2026-11-13，在 Windows/amd64 上测试。独立 Go 模块只用于研究，不改变 vnray 的生产依赖与存储。
本模块使用 Go 1.26.7（Prometheus v0.316.0 的最低要求）；原 tstorage 测量使用 Go 1.26.5。

本文保留当时的比较结果。生产存储现使用 SQLite BLOB + CBOR + Zstd，见 [ADR 0060](../../docs/adr/0060-sqlite-cbor-monitoring.md)；此前在线 Prometheus 测量见 [ADR 0059](../../docs/adr/0059-embedded-prometheus-tsdb.md)。下方 BlockWriter 数据仍是离线文件测试，不代表在线 DB 的 Head/WAL 占用。

## 全量实测

10 用户、每 2 秒采样、7 天，固定种子 20261113。302400 轮采样、3024000 条用户快照、147 条数值序列，
合计 44452800 个数值点。沿用当时的主机/用户字段、高低位整数拆分及完成标记，不减少字段、不降采样、不降低数值精度。
每轮都有变化，因此去重不会减少记录。数据时间为 2026-11-01 00:00:00 UTC 至 2026-11-07 23:59:58 UTC。

| 实现 | 版本 | 文件总大小 | 相对 tstorage 减少 | 存储方式 |
| --- | --- | ---: | ---: | --- |
| tstorage | v0.3.6 | 176.34 MB | 基线 | 30 分钟分区，Gorilla 数据 + JSON 索引 |
| Prometheus TSDB | v0.316.0 / Prometheus 3.16.0 | 80.81 MB | 54.17% | 原生 XOR chunk + 二进制索引，336 个持久块 |
| parquet-go + Zstd | v0.32.0 | 66.59 MB | 62.24% | 336 个列式文件，Byte Stream Split + Zstd |
| Badger + Zstd | v4.9.6 | 106.23 MB | 39.76% | 时间戳键 + 整轮二进制快照值，LSM 表压缩 |

MB 为十进制；精确字节数分别为 176338288、80814075、66588226、106230826。
计入全部存储目录文件，包括字段映射 `schema.json`、索引、元数据、tombstone、manifest 等；不计测试报告、输入样本和日志。
统计文件长度，不是文件系统实际分配空间。Badger 自带的 1 MiB DISCARD 文件也计入。

这是相同信息量的存储方案比较，不是只替换引擎而保持记录布局完全一致：
Prometheus 保持每条指标一条序列，Parquet 一轮一行并把全部序列分列，Badger 一轮一个键值。
同一时间戳在后两种布局中只保存一次；这些布局差异也是压缩率差异的一部分。

## 公平性与验证

当时通过容量数据生成器和旧指标映射导出了统一输入。切换 SQLite 后，旧指标映射和导出测试已从根模块删除；本历史比较工具只消费已保存的输入文件。
每轮格式为小端 int64 纳秒时间戳，后接 147 个 float64 的原始位表示。
输入文件 SHA-256：`c7efddc2ec0f2e1b553c60744d4bc65656df88239201b12e604514612f699f41`。

每种实现都关闭并重新打开，然后读取全部数值验证数量及 SHA-256。
Parquet、Badger 重建完整输入字节串；Prometheus 对每条序列的时间戳和 float64 位表示分别做哈希核对。
包含缺失指标用的 NaN 位模式，因此不能靠丢弃空值或改写浮点数通过验证。
Prometheus 时间戳单位为毫秒，测试首先检查纳秒值可被精确转换；当前样本全部为整秒。
实际生产中的纳秒采样抖动不能原样保存在 Prometheus 毫秒时间戳中，接入前必须确定时间精度策略。

原 tstorage 分区在边界采样后才轮转，因此每块为 901 轮，而不是 900 轮；Prometheus、Parquet 用相同边界。
Badger 没有按时间划分物理块，按 901 轮批量提交，使用其自身 LSM 合并策略。

## 各实现设置及接入限制

### Prometheus TSDB

- 测量使用公开 `tsdb.BlockWriter` 生成原生持久块，通过 `OpenDBReadOnly` 查询并验证。
- 本次固定 30 分钟块，没有进一步跨块合并；TSDB 本身具备 leveled compaction、WAL、时间范围查询和保留策略。
- BlockWriter 是离线块写入接口，本次不包含在线 DB 的活跃 Head、WAL 和检查点，不是在线运行磁盘峰值。
- 已落盘数据约 75.11 MB，二进制索引约 5.59 MB，其余元数据约 0.12 MB。
- 适合希望使用完整时序引擎的路线，但依赖规模大，内部库 API 随版本演进；最新版本要求更新 Go 工具链。
- 验证器最初使用无穷大查询上限，导致只含持久块的目录尝试读取不存在的 WAL；调整为数据真实时间范围后通过。未修改引擎源码。

### Parquet + Zstd

- 使用 `parquet-go`，纳秒时间戳采用 Delta Binary Packed，float64 采用 Byte Stream Split，Zstd 默认级别约为 3。
- 每文件 901 轮，一个 row group，共 336 个文件；完整列模式和页索引/统计信息包含在文件中。
- Parquet 是标准列式文件格式，不是完整在线 TSDB：需要自行实现活跃缓冲、WAL/崩溃恢复、文件发布、目录索引与保留期管理。
- 测量是封口后的历史文件，未包含这些尚未实现的在线功能开销。只重写 JSON 索引不能获得这里的压缩率。

### Badger + Zstd

- 8 字节大端纳秒时间戳作键，1176 字节的完整指标值作值；每个值保留全部 147 个 float64 位表示。
- 显式使用 Zstd level 3、`ValueThreshold=1 MiB`，让本次值保存在可压缩的 LSM 表中；使用默认值分离策略可能得到不同结果。
- `ValueLogFileSize=32 MiB`，批量写入完成后调用 `Flatten(1)` 并正常关闭，计入合并后的所有文件。
- 有事务、写入日志、范围迭代和自动合并；需要自行实现监控字段投影、时间保留和降采样。
- 值是整轮快照，读取单个指标也需要读取对应快照。此布局更接近本项目现有整轮采样与去重语义。
- Windows 不支持只读打开，验证使用普通打开后查询并关闭。统计的是首次正常关闭后的目录大小。

这些全量容量测试不代表生产级耐久性、长时间在线运行、断电恢复或最终管理 API 查询性能验证。
测试中的批量写入耗时不能当作每 2 秒同步提交的性能指标；完整哈希验证耗时也不能代替历史查询接口延迟。
该次比较没有增加逐点同步落盘保证，当时生产采样与历史使用原版 tstorage；当前实现见 ADR 0060。

## 其他候选

- [VictoriaMetrics](https://github.com/VictoriaMetrics/VictoriaMetrics)：完整时序系统，有压缩及合并，但通常按独立服务部署；其内部存储包不是与 tstorage 等价的轻量稳定嵌入接口。本次未运行，不给出容量估算。
- [Prometheus](https://github.com/prometheus/prometheus)、[parquet-go](https://github.com/parquet-go/parquet-go)、[Badger](https://github.com/dgraph-io/badger) 和 VictoriaMetrics 均为 Apache-2.0；测试时上述项目均未归档，近期仍有提交/发布。

## 使用已保存的输入重跑

需要事先保存的 `bin/storage-comparison/fixture/samples.bin` 和 `manifest.json`。这些实验产物不纳入版本控制，新检出仓库无法直接运行旧方案比较；当前 SQLite 容量测试的复现命令见 [ADR 0060](../../docs/adr/0060-sqlite-cbor-monitoring.md)。

已有上述输入时，从仓库根目录运行：

```powershell
$env:VNRAY_STORAGE_BENCH_DIR="$PWD/bin/storage-comparison"
Set-Location tools/storagebench
$env:VNRAY_STORAGE_ENGINE='parquet-zstd'
go test -run '^TestStorageComparison$' -count=1 -v -timeout=20m
```

引擎可选 `parquet-zstd`、`badger-zstd`、`prometheus`，各输出目录必须不存在。
重跑时设置 `VNRAY_STORAGE_RUN_SUFFIX='new-run'`，保留前一次产物。
不设置环境变量时测试自动跳过。`go vet ./...` 验证本测试模块。

本次有效报告位于根目录的 `bin/storage-comparison/`：

- `parquet-zstd-report.json`
- `badger-zstd-v2-report.json`
- `prometheus-v2-report.json`

生产模块未引入上述替代库；`go.mod`/`go.sum` 仅在本独立模块中记录测试依赖。
