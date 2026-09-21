# 功能规格：制品下载计量与分析（FR-142 / FR-143 / FR-144）

> 状态：待实现 · 2026-09-21 · 关联 PRD §4 FR-142~144

## 1. 背景与目标

**现有链路与它的设计约束**：仪表盘「下载量」KPI 来自 `OperationsDashboardService`（协议层 `ProtocolMetric` → 分钟聚合 `ProtocolMinute` 表，30 天保留）。该链路**刻意不携带路径、主体、地址或任何凭据**（见 `operations_observability.go`）——为低开销与隐私而设计。

**张力**：FR-142 需要 **per-制品** 计数与**来源（IP/UA）**聚合——明细数据不在现有链路的承载范围内，且不能以牺牲其约束为代价。

**目标**：新增一条**按分钟预聚合**的明细链路，支撑：① per-制品累计下载次数（FR-142）② 仪表盘累计趋势（FR-143）③ 按 IP（Top 10）与客户端类型聚合（FR-144）。

**「与 KPI 同源」的口径**（已与用户确认）：**同一条采集点**（协议层 GET 成功处），而非同一张表；KPI 继续使用 ProtocolMinute，不改动、不回归。

## 2. 需求（要什么）

### 2.1 计数口径（用户确认）

- 每次**完整传输**的成功下载计一次：`GET` 且 `status == 200`
  - 与 KPI 的差异：KPI 计 `200 || 206`（请求级）；本功能**不计 206**（Range 分段属断点续传的中间态，避免虚高）
- 含**匿名**下载（`anonymous` 标记，主体可空）
- **原始事件全记**；「同 IP + 同制品 1 小时窗口去重」是**查询口径**，原始数据不丢

### 2.2 展示

- 仓库详情「浏览」文件列表：每制品显示累计下载次数（**公开可见**——次数本身不含来源信息）
- 仪表盘新增「下载分析」区块（跟随既有时间范围筛选）：
  - 累计下载趋势
  - 来源 IP Top 10（**明文仅管理员**，与审计域既有口径一致）
  - 客户端类型分布（UA 归类：maven / gradle / npm / curl / 浏览器 / 其他；**不展示原始 UA 串**）

### 2.3 明确不做（边界）

- 不做实时（分钟级延迟可接受，沿用既有 flush 节奏）
- 不做单次下载的逐条明细浏览——事件数据是**聚合中间态**，"谁在何时下了什么"属审计域职责
- 不改动现有 KPI 口径与 ProtocolMinute 表
- 不做 IP 归属地/反查（YAGNI 且涉隐私）

## 3. 设计（怎么做）

### 3.1 数据模型（新增 migration `0038_asset_download_metrics.sql`）

表 `asset_download_minutes`——**按分钟预聚合**，控制膨胀：

| 列             | 类型    | 说明                                                |
| -------------- | ------- | --------------------------------------------------- |
| bucket_start   | TEXT    | UTC 分钟桶（RFC3339Nano，与 ProtocolMinute 同格式） |
| repo           | TEXT    | 仓库名                                              |
| asset_path     | TEXT    | 制品路径                                            |
| client_ip      | TEXT    | 来源 IP（明文；仅管理员可查询）                     |
| ua_family      | TEXT    | UA 归类结果（采集时归类，**原始串永不落库**）       |
| download_count | INTEGER | 该分钟该组合的完整下载次数                          |

主键 `(bucket_start, repo, asset_path, client_ip, ua_family)`；写入用 `INSERT … ON CONFLICT DO UPDATE SET download_count = download_count + excluded.download_count`（幂等累加）。

索引：`(repo, asset_path, bucket_start)`（per-制品查询）· `(bucket_start)`（趋势与保留期清理）。

保留期：**30 天**（与 `operationsRetention` 一致，复用清理任务）。

### 3.2 采集（写路径，低开销）

- 协议层 GET **完整成功（200）** 处调用 `RecordAssetDownload(repo, path, ip, ua, anonymous, at)`
- UA → `ua_family` 在采集时归类（子串匹配，表见 §附录）
- 内存缓冲 `pending[key]count`（key = 分钟桶 + 仓库 + 路径 + IP + UA 族）→ 由既有周期性 tick 一并 Flush（与 `OperationsDashboardService.Flush` 同节奏、同失败保留语义）
- **不在下载路径逐请求写 SQLite**（沿用现有设计原则）

### 3.3 查询 API

| 端点                                                               | 鉴权       | 说明                                                                                                                             |
| ------------------------------------------------------------------ | ---------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `GET /api/v1/repositories/{name}/assets`（既有列表）               | 仓库可读   | 响应项新增 `downloadCount`（对 asset_download_minutes 的 SUM，列表 SQL 内 LEFT JOIN 聚合，一次查询）                             |
| `GET /api/v1/observability/downloads/trend?range=1h\|24h\|7d\|30d` | **管理员** | `[{bucket, count}]`，原始累计；bucket 粒度自适应（1h→分钟、24h→小时、7d/30d→天）                                                 |
| `GET /api/v1/observability/downloads/by-client?range=…&dedupe=1h`  | **管理员** | `{topIps:[{ip, count}], families:[{family, count}]}`；`dedupe=1h` 为**默认**（同 IP+同制品 1h 内只计一次贡献，即"独立来源"视角） |

### 3.4 前端

- 文件列表：新增「下载」列（紧凑宽度；窄屏并入主标识副文本）
- 仪表盘「下载分析」区块：累计趋势（recharts `AreaChart`，复用已升级的 recharts 3）+ IP Top 10 与客户端分布（紧凑条形/列表）；时间范围与既有仪表盘筛选联动

### 3.5 隐私

- IP 明文仅管理员：API 层鉴权（非管理员 403）+ 前端仅管理员渲染
- UA 只存**归类结果**，原始串永不落库（比审计域更保守）

## 4. 任务拆分

1. migration `0038` + 持久层（`AddAssetDownloadMinutes` / 查询方法）
2. 采集接入（协议层 GET 200 处 + UA 归类 + 缓冲 flush）+ 单测
3. per-制品计数进入既有列表 API（LEFT JOIN 聚合）+ 前端「下载」列
4. `trend` / `by-client` API + 仪表盘「下载分析」区块
5. 保留期清理接入 + 索引与查询计划验证

## 5. 验收标准

- [ ] **真机过**：线上完整下载一次 → 计数 +1；Range 分段（206）不计
- [ ] **真机过**：匿名下载计入；1h 去重口径正确（同 IP 同制品多次 → `by-client` 只计一次贡献；原始累计不丢）
- [ ] **真机过**：文件列表显示累计次数（公开可见）；仪表盘趋势与 IP/客户端聚合正确
- [ ] 非管理员访问 `trend` / `by-client` → 403；界面不渲染 IP 明文
- [ ] 现有 KPI 口径零回归（ProtocolMinute 与 `RecordProtocol` 不动）
- [ ] 30 天保留清理生效（单测覆盖）
- [ ] 性能：采集在内存完成、flush 批量写；并发下载不产生逐请求 SQLite 写

## 6. 风险 / 待定

- **per-制品计数在既有列表里的实现**：列表 SQL 内 LEFT JOIN 聚合（倾向，一次查询）vs 独立批量端点；实现时看查询计划（`EXPLAIN QUERY PLAN`）定夺
- **bucket 行数放大**：高频同 IP 同制品（CI 轮询）会放大行数；分钟聚合已大幅缓解，暂不做更粗的合并档（YAGNI）
- **趋势粒度自适应阈值**的具体分界在实现时定（1h→分钟已定；小时/天分界按数据量验证）

## 附录：UA 归类表

| `ua_family` | 匹配（子串，大小写不敏感，按序首中即停） |
| ----------- | ---------------------------------------- |
| `maven`     | `Apache-Maven` / `Maven`                 |
| `gradle`    | `Gradle`                                 |
| `npm`       | `npm/` / `node-fetch` / `node`           |
| `curl`      | `curl`                                   |
| `browser`   | `Mozilla`（且未中以上）                  |
| `other`     | 其余与空值                               |
