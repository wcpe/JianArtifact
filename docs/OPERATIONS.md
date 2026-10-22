# 运维手册：JianArtifact

> 本文只记录当前可执行的部署、升级、备份、恢复、回滚和排障流程。版本真源是根目录 `VERSION`；当前发布版本为 `0.12.0`（M3 收尾条目与搜索体验 / 性能改造已交付，下一个开发窗口尚未开启）。节点搬迁与备份以一致性备份包为单位（见 §3.2、§3.3、§5）；实时复制通道已随 FR-138 整体退役，本文不含任何复制拓扑、角色或凭据操作。

## 1. 运行与配置

### 1.1 当前支持的基础配置

配置通过环境变量、部署 Secret 或 systemd 环境文件注入；真实环境文件不入库。

| 变量                                   | 作用                                        | 默认/约束                                                                                      |
| -------------------------------------- | ------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `JIAN_HTTP_ADDR`                       | HTTP 监听地址                               | `:8080`                                                                                        |
| `JIAN_DATA_DIR`                        | SQLite 与 blob 数据根目录                   | `./data`；生产必须使用持久化绝对路径（见下方"数据目录解析"）                                   |
| `JIAN_JWT_SECRET`                      | JWT(HS256) 签名密钥                         | 生产必填、强随机、不得打印                                                                     |
| `JIAN_MIGRATION_CREDENTIAL_KEY`        | 在线迁移凭据 AES-256-GCM 密钥               | Base64 编码 32 字节；生产应显式固定                                                            |
| `JIAN_UPSTREAM_TIMEOUT`                | proxy 回源整体超时（秒）                    | `30`                                                                                           |
| `JIAN_ENABLED_FORMATS`                 | 启用的协议格式                              | 缺省 `raw,maven,npm`；显式空值关闭全部                                                         |
| `JIAN_PUBLIC_URL`                      | 对外基础 URL                                | 节点本地配置；影响下载链接与 usage 片段                                                        |
| `JIAN_SYNC_INTERVAL`                   | 设置页「同步间隔」的初始默认值（秒）        | `5`；仅在 setting 键不存在时写入，当前无调度器消费（复制退役遗留）                             |
| `JIAN_BLOB_GC_INTERVAL`                | 清理遗留 / 孤儿 blob 的间隔（秒）           | `86400`；`0` 禁用                                                                              |
| `JIAN_STORAGE_CLEANUP_INTERVAL`        | 存储治理清理作业的间隔（秒）                | `86400`；`0` 禁用（作业不注册）；非法 / 负数回落默认                                           |
| `JIAN_STORAGE_METADATA_RETENTION_DAYS` | 终态操作与隔离元数据的保留天数              | `7`；`0` 禁用元数据裁剪；非法 / 负数回落默认                                                   |
| `JIAN_STORAGE_TEMP_MAX_AGE_HOURS`      | 过期 OCI 上传临时文件的最长滞留（小时）     | `24`；`0` 禁用该项清理；非法 / 负数回落默认                                                    |
| `JIAN_BACKUP_AUTO_INTERVAL`            | 自动备份间隔（秒）——**仅作首次部署初值**    | `0`（**默认关闭**）；`0` = 关闭；非法 / 负数回落默认。界面可填 `30s`/`5m`/`2h`/`1d`/`1mo`/`1y` |
| `JIAN_BACKUP_KEEP_LAST`                | 保留最近 N 份自动备份（份）——**仅作初值**   | `7`；`0` = 不按份数限制；非法 / 负数回落默认                                                   |
| `JIAN_BACKUP_RETENTION_DAYS`           | 自动备份的保留天数（天）——**仅作初值**      | `30`；`0` = 不限天数；非法 / 负数回落默认                                                      |
| `JIAN_BACKUP_ORPHAN_CLEANUP_INTERVAL`  | 孤儿备份包回收作业的间隔（秒）              | `86400`；`0` 禁用（作业不注册）；非法 / 负数回落默认                                           |
| `JIAN_BACKUP_ORPHAN_GRACE_HOURS`       | 孤儿回收的宽限期（小时）                    | `1`；`0` / 负数 / 非法回落 1（**不允许零宽限**，见下）                                         |
| `JIAN_TLS_ADDR`                        | 内置 HTTPS 监听地址                         | 为空表示不启用                                                                                 |
| `JIAN_TLS_CERT` / `JIAN_TLS_KEY`       | TLS 证书和私钥路径                          | 配置 `JIAN_TLS_ADDR` 时必填                                                                    |
| `JIAN_AUTO_BLOCK_BASE_SECONDS`         | proxy 上游 auto-block 退避起始时长（秒）    | `40`；之后每档翻倍；`0` / 负数 / 非法回落默认（无禁用语义）                                    |
| `JIAN_SEARCH_TIMEOUT_SECONDS`          | 搜索链路（网页搜索 / npm 搜索）总预算（秒） | `10`；三条搜索 SQL 共享同一 deadline；缺省 / 非数字 / 负数 / `0` 均回落默认（**无禁用语义**）  |
| `JIAN_CAPACITY_CACHE_TTL_SECONDS`      | 仪表盘容量聚合结果的缓存有效期（秒）        | `30`；显式 `0` **禁用缓存**（每次实时聚合），非法 / 负数回落默认                               |
| `JIAN_SQLITE_MAX_CONNS`                | SQLite 连接池上限（条）                     | `8`；WAL 下多读并发 + 单写；非正数 / 非法回落默认（**无禁用语义**）                            |
| `JIAN_RESTART_UNIT`                    | 一键重启的目标 systemd 单元名               | `jianartifact`；须匹配 `[A-Za-z0-9._@-]`，否则该能力降级为不可用                               |
| `JIAN_RESTART_SCOPE`                   | 一键重启的作用域                            | `system`；可选 `systemd-user`（**推荐**）/ `docker`；非法回落默认                              |

派生路径固定为 `${JIAN_DATA_DIR}/jianartifact.db` 和 `${JIAN_DATA_DIR}/blobs`。启动会创建数据目录并执行 schema 迁移；不会在启动时扫描活动 blob；遗留 / 孤儿 blob 的定时清理由本实例按 `JIAN_BLOB_GC_INTERVAL` 独立运行；存储治理清理（隔离区空目录、终态操作与隔离元数据裁剪、过期 OCI 上传临时文件、代理缓存保留）由 `storage-cleanup` 作业按 `JIAN_STORAGE_CLEANUP_INTERVAL` 独立运行；备份孤儿回收（`${JIAN_DATA_DIR}/backups/` 下"有包体、无登记"的残留）由 `backup-orphan-cleanup` 作业按 `JIAN_BACKUP_ORPHAN_CLEANUP_INTERVAL` 独立运行。三者按目录命名空间各管一块、互不重叠。以上作业**都只在第一个间隔到期后执行**（启动不清理、不扫描历史）。

### 自动备份间隔的时间单位（FR-40 追加九）

界面上「自动备份间隔」由**数值 + 单位**两个控件组成，支持秒 / 分钟 / 小时 / 天 / 周 / 个月 / 年。填写时界面会实时显示换算出的秒数，便于核对。

- **存储口径不变**：权威值仍是秒（`setting` 表的 `backup_auto_interval`），本功能只改变"人的表达方式"，不需要迁移，也不改变既有配置的含义。
- **月与年是保守近似**：1 个月按 30 天、1 年按 365 天换算。自动备份是"至少隔这么久跑一次"的兜底动作，差一两天的语义影响远小于"日历月长度不同导致配置漂移"的困惑。
- **上限 30 天**：超过上限会**钳制**到上限而不是拒绝保存——填"1 年"的意图是"尽量久"，回落比报错更贴近意图。
- **`0` 恒等于关闭**，与单位无关（"0 天"也是关闭），不存在"既非开也非关"的状态。
- **接口层**：`PUT /api/v1/settings` 接受 `backupAutoIntervalExpr`（如 `"1d"`），优先级高于 `backupAutoInterval`（秒）；响应回显 `backupAutoIntervalExpr` / `backupAutoIntervalValue` / `backupAutoIntervalUnit`，供界面直接回填，避免前端重复实现拆分规则。
- **`m` 是分钟，月必须写 `mo`**：这是 time 包的通用约定。把 `1m` 当成一个月会有 43200 倍的偏差，因此后端对两者严格区分，并有专门的单测守着。

### 恢复失败时能看到真实原因（FR-40 追加十）

早先的失败提示几乎是同一句话：「服务内部错误，请稍后重试或查看日志。」真实原因（基线包缺失、已有导入在跑、包未就绪、归档损坏…）被丢弃，日志里也可能没有对应记录——现场无法自行判断，也无法反馈有效信息。

现在分为两类：

- **可自行处置的失败**会给出各自的错误码与做法：`target_not_empty`（勾选覆盖）、`base_missing`（先导入基线包）、`import_in_progress`（等前一个跑完）、`package_not_ready`（包未生成完成或已失败）、`restore_pending`（先重启）、`incompatible`（升级程序）、`package_oversize`（拆包）。
- **真正的未知故障**才标 `internal`，并且**把原始错误文本一并返回**（`内部错误：<原始错误>`）与写入导入记录的 `error` 字段。该接口仅对管理员开放，错误文本来自本程序的领域逻辑，不含凭据。

### 恢复前先看包内容，并对比与本机的差异（FR-40 追加十一 / 十四）

恢复确认弹窗内直接展开「包里有什么 · 与本机差异」，回答**恢复后会怎么变**：

| 分类               | 含义                        |
| ------------------ | --------------------------- |
| 包里有、本机没有   | 恢复后**新增**              |
| 本机有、包里没有   | 恢复后**消失** ← 最需要警惕 |
| 两边都有但内容不同 | 恢复后**被替换**            |
| 完全一致           | 恢复后不变                  |

**性能设计**（几十万制品时仍然可用）：

- 打开时只请求**摘要**——服务端只跑聚合查询（`ATTACH` 包内快照后一条 SQL 求差集），不搬明细；
- 点开某一类才拉**分页明细**，单页 50、硬上限 200，界面显示"第几页 / 共多少条"，绝不让用户以为看全了；
- 包内快照解出后**缓存在临时目录**（TTL 10 分钟、同时最多 2 份）。解包代价与包体积同阶（20GB 是分钟级），不缓存会让每次翻页都重新解一遍；上限 2 份是防止"只想对比一下"把盘写满。

**只对比元数据**（仓库名/格式、制品路径/大小/哈希、账号名/角色/状态），**不比对文件内容**——逐字节验证 20GB 不现实，那是「深度校验」的职责。界面上明确写了这一点，避免"无差异"被误读成"字节一致"。

### 恢复前先看包内容（FR-40 追加十一）

恢复会用备份包**整份覆盖**本机数据且不可撤销。此前界面只给出抽象计数（"多少制品、多少仓库"），无从判断包里到底是什么，恢复等于一次赌博。

现在列表的「⋯」菜单里第一项是「查看包内容」，展示：

- **仓库清单**（名称 / 格式 / 类型）——判断"是不是我要的那个包"最直接的依据；
- **账号清单**（用户名 / 角色 / 状态）——**不含**口令哈希或任何密钥；
- **制品路径样例**（最多 50 条）与制品总数；
- 数据库 schema 版本（低于当前程序即可直接恢复）。

该端点是**纯只读**的：把包内数据库快照解到临时目录、以 `mode=ro` 打开查询，查完立即删除；不改动包体、不写入备份目录、不产生任何标记。因此"看内容"不会影响包本身的可用性。

### 自动备份为什么默认关闭（FR-40 追加十二）

默认间隔是 `0`（**关闭**），这是**有意**的选择，不是遗漏。

原因是备份为**全量**包：每份大小≈整个数据目录。默认开启会在用户毫无预期的情况下持续消耗磁盘，而实测存在 **20GB 数据 / 40GB 磁盘**这类配置——默认开几天就能把盘写满。两者的代价不对称：

- 默认关闭、出事时没备份 → 运维事后补配置即可修复；
- 默认开启、盘被写满 → 服务写挂，是运行故障。

两害相权，默认关闭。

**保留策略不等于容量可控**：`JIAN_BACKUP_KEEP_LAST` 是"最多留几份"的上限，7 份 × 20GB = 140GB 仍可能远超可用磁盘。因此配置弹窗内按"最近一次包大小 × 保留份数"实时估算并提示；开启前请自己算一遍。

**非法值回落默认（关闭）**：与"零点即关闭"的口径一致，回落目标是本项的默认值。

### 恢复不会抹掉备份列表（FR-40 追加九）

`backup_package` 登记表与被恢复的数据库是同一个文件，而备份包体（`<id>.tar.gz`）在数据目录下、**不在**快照里。恢复一份旧快照会让登记表退回旧内容，于是恢复之后"磁盘上明明有包、列表里却看不见"——用户看到的就是"恢复一次，备份包全没了"，而 restore 恰恰最需要备份兜底。

现在 `ApplyPendingRestore` 在替换数据库后，会以**磁盘为准**补回登记：凡包体存在且 manifest 合法的包，一律重新登记（状态 `done`、来源 `manual`、备注「恢复后补回」）。包体已不存在的旧登记不补（它们本来也没法用）。补回的条数会写进启动日志的恢复摘要里：

```
已应用待生效的备份恢复：包 bk-xxx：制品 N 件，blob 合并 X / 跳过 Y，回滚备份 …, 补回备份登记 Z 个
```

看到「补回备份登记 Z 个」说明这份旧快照里没有这些包的记录——它们是被恢复操作"找回"的，不是新产生的。

**导入记录同样会被搬回**（摘要里的「搬回导入记录 N 条」）：`backup_import` 也是库内的表，恢复会把它一并回滚，于是"这次恢复失败了吗、为什么失败"的记录会被**它自己的恢复动作**覆盖掉。现在会从 `pre-restore-<时间戳>/` 那份快照里把导入记录搬回来，其中失败记录的错误码与原因一并保留。

**回滚备份会被自动回收**：每次恢复都会在数据目录下留一份 `pre-restore-<时间戳>/`（含被覆盖前的完整数据库）作为回滚退路。这些目录早先**从不清理**——实测测试站积了 61 个、96MB；按 20GB 数据算，每恢复一次就多 20GB，两次就能写满 40GB 的盘，而这恰恰发生在"用户已因磁盘紧张来恢复"的时刻。

现在由 `backup-orphan-cleanup` 作业统一回收，**保留最近 3 个**（回滚价值在"刚恢复完的那几次"）。日志里会体现：

```
备份孤儿回收完成：扫描 N 个包体 / … / pre-restore 备份回收 8 个（保留最近 3 个）
```

**另有一份文件级留痕**：`${JIAN_DATA_DIR}/restore-failures.log`。每行一条恢复失败，包含时间、导入 ID、来源、操作者、包标识、错误码与原始错误。它落在数据目录下、不在数据库快照内，因此**即使数据库被恢复覆盖也不会丢**。排查"恢复为什么失败"时优先看这个文件。

### 备份目录与"未被管理的包"（FR-40 追加八）

`${JIAN_DATA_DIR}/backups/` 里的包体与数据库登记是两套独立的持久化：包体是文件，登记在 SQLite。`BackupService.Delete` 的顺序是**先删文件、再删登记**，因此正常删除与保留策略都不会留下残留；但两者之间没有事务，进程被杀、外部工具直接改库、历史版本缺陷或手工搬运都可能让它们失步，产生"磁盘有包体、登记无记录"的包。

这类包在常规列表里**不可见**（列表读登记表），于是磁盘被占着却既不能用也不能删。为此提供两个配套能力：

- **导入（界面「导入未管理的包」）**：扫描出候选，支持单选与多选，逐个校验包内 manifest 后补建登记行。导入的包以 `origin=manual` 登记，因此**不会被保留策略自动淘汰**——是否保留由人决定。文件名与包内标识不一致时会按包内标识重命名包体。
- **回收（`backup-orphan-cleanup` 作业）**：把无人认领的残留按两道路径删掉。

两者的候选来自**同一份差集**（磁盘有、登记无），因此不会互相打架。回收作业有两道护栏：

1. **宽限期**：只回收 mtime 早于 `now - JIAN_BACKUP_ORPHAN_GRACE_HOURS` 的文件。刚拷进来、还没来得及在界面导入的包一律保留。宽限期**不允许配成 0**（`0`/负数/非法都回落 1 小时）——零宽限会让"拷入 → 导入"这个操作窗口消失。
2. **身份校验**：只回收能通过 `archive.Manifest.Validate` 的本产品备份包。无法识别的归档（用户放错的其他 tar.gz、损坏包）一律不动，计入结果的 `Invalid` 而非删除。

**重复副本**：同一份包的多个文件（manifest 标识相同）会在未管理列表里被标为「多余副本」，`valid=false`、`duplicate=true`。它们不能重复导入，但**内容有效**，因此孤儿回收**不会**删除它们——需要运维自行确认后手动删除多余的那一个。这样它们不会卡在"两条列表都看不见"的缝里白占磁盘。

命名空间护栏：只碰 `<id>.tar.gz` 与 `<id>.index`；`<id>.tar.gz.snap` 是生成中的一致性快照临时文件，由生成流程的 defer 自清理，回收作业不碰。单轮删除上限 500 个，首次在积压严重的实例上不会一次打爆磁盘 IO。

> **典型处置流程**：发现未管理包 → 先在界面确认哪些要 → 导入要保留的 → 其余的等宽限期过后由作业回收（或手动触发 `POST /api/v1/maintenance/jobs/backup-orphan-cleanup/run`）。

### 数据目录解析（CLI 与服务的对齐）

服务由 systemd 单元经 `EnvironmentFile` 提供 `JIAN_DATA_DIR`；**CLI 是独立进程，读不到它**。因此 CLI 的数据目录按以下顺序解析（每次都会把结果与依据打印到 stderr）：

1. **`JIAN_DATA_DIR` 已显式设置** → 直接使用。这是唯一权威来源，部署与自动化脚本都应用它。此时**不打印**提示（行为完全由调用方决定），但仍会去找"声明了该目录"的那份实例 env 文件并采纳其配置——见下方关于散件密钥的说明。
2. **未设置时自动发现**：按序读常见部署位置的环境文件（`/opt/jianartifact/jianartifact.env`、`/etc/jianartifact/…`、`/var/lib/jianartifact/…`）里的 `JIAN_DATA_DIR`。找到**唯一**候选就用它并打印说明；找到**多个不同**候选则列出候选并报错退出（**不猜**——猜错就是操作到另一个库）。
3. **探测不到** → 回落到默认 `./data`，并打印警告。

> **为什么需要这套机制**：早期 CLI 在 `JIAN_DATA_DIR` 缺失时**静默**回落到 `./data`。后果有两层——CLI 操作的是另一个库（看不到服务侧真实的包，报"归档不存在"，看着像功能缺陷）；且启动会 `MkdirAll` 数据目录并生成 `jwt.secret` / `jianartifact.db`，于是**一次误跑就在原地建出一个完整的空实例**。这正是"莫名其妙多出数据目录"的来源。
>
> **自动发现可以是自动的，但不能是静默的**：任何一次解析都要能回答"这次用的是哪个目录、依据是什么"。若你在服务机上跑 CLI 却没设 `JIAN_DATA_DIR`，请核对输出里的数据目录是否与服务的 `${JIAN_DATA_DIR}` 一致。
>
> **为什么"采纳实例配置"覆盖了全部三种情形**：只对齐数据目录是不够的。该实例的 `JIAN_JWT_SECRET` / `JIAN_MIGRATION_CREDENTIAL_KEY` 等仍缺失时，CLI 会以为密钥"未设置"，于是**在服务的数据目录里现场生成一份散件**（`jwt.secret` / `migration-credential.key`），并对 `backup list` 这种只读命令打印"未设置 JIAN_JWT_SECRET"的误导性警告——而服务明明设了。散件**不会覆盖**服务实际使用的密钥（env 里的值优先），所以不影响登录，但它是无用文件、且会让人误判配置状态。故只要找到该目录对应的实例 env，就整份采纳（**只补当前进程未设置的键**，显式 export 的值始终优先）。

**用 `sudo` 跑 CLI 时注意属主**（实测事故）：`sudo jianartifact backup import …` 会以 root 写入 `restore.pending`（`0600 root:root`）与 `restore-staging/`。服务通常以普通用户运行，**读不了**该标记。现在的行为是：

- 服务启动时若发现标记读不了，**跳过该恢复并打印含修复命令的告警，但正常起服**（不再因一个可延后的维护动作而整站起不来，见 §5.2）；
- CLI 导入成功后若检测到标记属主与当前进程不同，会**当场提示**去改属主——问题在产生的当口就暴露。

正确做法：以**服务运行身份**执行 CLI（通常数据目录对它有写权限，无需 sudo），或在重启前 `chown` 回服务用户。

**周期化自动备份**（FR-40）默认**关闭**：间隔为 `0` 时不生成任何备份。开启后按间隔生成**完整热备份包**（不生成差包——无人值守下基线选择不可控且易形成长链），并在每轮收尾应用保留策略（见 §5.2「备份目录保留」）。

三项配置的**真源是数据库**（`setting` 表），可在管理端「迁移与备份 → 备份与搬迁 → 自动备份与保留策略」卡上直接修改，**保存即生效、无需重启**（作业每 30 秒重读一次配置，最迟一个心跳周期内应用）。三个环境变量仅在**首次部署**（该组配置尚不存在时）作为初值写入一次，此后改环境变量不再生效——它们不再是"配置真源"，只是初始化默认值。

**已废弃的环境变量**：`JIAN_REPLICATION_ROLE`、`JIAN_REPLICATION_PRIMARY_URL`、`JIAN_REPLICATION_RELAY_ENABLED` 自复制通道退役（FR-138）起不再被解析；旧环境文件里残留这些键不影响启动（未知键被忽略），可直接删除。

## 搜索索引（FR-149）

全局搜索（`GET /api/v1/search`）在 `asset.path` 上使用 FTS5 trigram 派生索引（`asset_path_fts`）。升级到含迁移 `0049` 的版本后：

- **结构在迁移里建，数据不在迁移里灌**。迁移只建虚拟表与状态表（`search_index_state` 单行，`completed_at IS NULL` 表示未回填），历史行由启动后的后台任务按 `last_id` 水位**分批**回填。百万级库预计分钟级完成。
- **回填完成前搜索仍然可用**，但走的是旧的 `path LIKE '%词%'` 全表扫描路径——结果正确、尚未提速。这是刻意取舍（宁可慢而全，不可快而漏）。
- 进程中途重启会从水位**续跑**，不需要人工干预。
- 回填按批提交并在批间让出。写仍然只有**一个写者**（WAL 单写语义），因此长写事务会与回填互相排队；连接池（默认 8 条，`JIAN_SQLITE_MAX_CONNS`）放宽的只是**读并发**，不改变写串行化。

**观测**（仅管理员）：

- `GET /api/v1/search/index` → `{ready, indexed, total, lastId, updatedAt, indexedRows, assetRows, missingRows, orphanRows}`。
  - `ready=false` 有**两种**成因，处置方式不同：
    1. **回填未完成** → `lastId` 还在推进，等着即可（`indexed/total` 看剩余量）；
    2. **存在漏索引**（`missingRows > 0`）→ 回填早已完成，等下去永远不会好，**必须重建**。
  - `missingRows`（asset 里有、索引里没有，**危险方向**：这些制品搜不到）与 `orphanRows`（索引里有、asset 已删，只浪费空间）必须分开看；两者方向相反，合成一个「差额」会互相抵消、掩盖问题。
  - 检出漏索引时会**自动降级**：服务把 `completed_at` 清空，搜索立即退回全表检索（慢但完整），重建完成后自动恢复加速。**自动降级只发生在「启动自检」与「回填完成」这两个时机**；`GET /api/v1/search/index` 本身是只读端点、不改状态，因此**经它发现**的漂移不会自动降级，需要手动重建（两处口径的差别来源见 [API.md](API.md) 的 `ready` 段落）。
- `POST /api/v1/search/index/rebuild` → `202` 表示已受理，后台执行「清空索引 → 水位归零 → 重新分批回填」；**正在运行时重复触发返回 `409`**。重建全程在线，清空后搜索自动退回降级路径，正确性不受影响。

**该端点不要轮询**：每次请求都要做全表计数，在百万级库上是秒级开销。观测请按需手动查，或用启动日志里的自检结论。

**已知盲区**：`UPDATE asset SET path = ?` 不会重建索引。此时行数与 rowid 集合都一致，漂移指标会报 **0**，但「新名字搜不到、旧名字还能搜到」。不要写入路径可直接改 `asset`；一旦怀疑发生，只能全量重建。

**何时需要重建**：索引与 `asset` 出现漂移（例如人工改库，或将来新增了未同步索引的写入路径）。重建同时会清掉仓库级联删除留下的孤儿索引行。

**磁盘**：trigram 索引随 `path` 文本量增长，需为数据库文件预留额外空间；`path` 越长的部署增量越明显。回填期间请避免同时跑备份（备份持长读事务，会让 WAL 无法 checkpoint、持续增长）。

**升级到含本索引的版本时注意**：

- **迁移会在监听端口之前执行**。`0050` 要在 `asset` 上全表扫描建复合索引，大库上可能持续数分钟，期间进程不监听端口。每个待应用的迁移现在都会打「数据库迁移开始/完成」两行日志（含耗时），据此可判断卡在哪一个。
- **Kubernetes 部署必须配 `startupProbe`**（仓库内的 Helm 与 k8s 模板已加，容忍 10 分钟）。没有它时，迁移期间 `/healthz` 不可达 → kubelet 判定存活失败并 SIGKILL → 事务回滚 → 重启后从头重建 → **崩溃循环，升级永远完不成**。若你的部署是自定义的，请自行放宽 liveness 的 `failureThreshold` 或临时摘掉。
- **回填未完成期间搜索走全表 LIKE**，是最慢的阶段。若升级前单次搜索已接近 `JIAN_SEARCH_TIMEOUT_SECONDS`（默认 10 秒），该窗口内会返回 **504 `search_timeout`**——这是预期降级，不是故障；可在窗口内临时调大该值。
- 回填完成前请预留与索引同量级的空闲磁盘（`trigram` 索引与路径文本总量成正比，先在生产等价的预发环境实测）。

**降级（回滚到旧版本）后再升回来时**：旧版本不维护该索引，期间新增的制品不会进索引；再升级回来时 `completed_at` 已是非空、回填不会重跑，索引会**永久陈旧**且只有一行日志提示。因此降级往返后**必须**执行一次 `POST /api/v1/search/index/rebuild`。

**已知限制**：

- 短于 3 字符的查询词无法走 trigram 索引（自动退回 `LIKE`），`jar` 这类命中率极高的宽泛词可能**不比扫描快**——这类查询靠 `JIAN_SEARCH_TIMEOUT_SECONDS` 兜底。
- 搜索超时返回 **504 `search_timeout`**，前端应提示可缩小范围后重试。

### 1.2 节点搬迁（替代原主备复制）

节点搬迁的唯一手段是**一致性备份包**：旧实例生成包 → 传输 → 新实例导入 → 重启替换 → 校验。没有节点角色、配对仪式、水位或复制凭据。操作步骤见 §3.2（包格式与生成）、§3.3（Cutover 顺序）、§5.2（备份）与 §5.3（导入）。

原「主备级联树 + 逐跳凭据 + 人工提升」流程（FR-115/119/121）已随 FR-138 整体退役，本文不再保留其可执行步骤；决策与退役范围见 [`adr/0027`](adr/0027-package-based-node-backup-and-relocation.md)，历史实现见 [`specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md)。

包内**只含数据、不含密钥**，因此新实例导入后必须自行配置：`JIAN_JWT_SECRET`、`JIAN_MIGRATION_CREDENTIAL_KEY`、数据目录与对外地址（`JIAN_PUBLIC_URL` / TLS / 域名白名单 / 回源 Token）。

### 1.3 对外访问安全

#### Host 白名单

管理端设置中的允许访问域名是节点本地配置。配置后所有管理 API、协议和静态页面的 Host 必须命中白名单；本机回环放行。回环只按服务端看到的 TCP 对端地址判定，`Host: localhost`、`Host: 127.0.0.1` 或转发头都不能把外部连接声明为回环。命中显式白名单的非 TLS 协议请求是 CDN HTTP 回源兼容路径，不构成来源认证，生产必须同时启用回源 Token 或改用 HTTPS 回源。同机反向代理或隧道若以回环连接转发外部流量，应用看到的对端将是该代理；此时不得把回环例外当作来源认证，应让代理连接服务的非回环监听地址，并配合防火墙限制来源。白名单不能替代防火墙，生产仍应限制源站只接受 CDN/反向代理回源流量。

#### 内置 TLS

配置 `JIAN_TLS_ADDR`、`JIAN_TLS_CERT` 和 `JIAN_TLS_KEY` 后，服务在 HTTP 之外启动 HTTPS 监听，共用同一 handler。证书缺失或格式错误必须使进程整体启动失败，不得静默退回明文。CDN/反向代理回源应使用 HTTPS。

#### 回源 Token

安全防护设置中的回源 Token 是节点本地配置，默认关闭。启用后所有非回环请求必须携带配置的请求头和值；直连源站即使伪造 Host 也应被拒绝。重新生成后旧 Token 立即失效。

#### 仓库 git 历史中的历史地址残留（已知，接受）

2026-09-13 的脱敏提交 `b640c33` 已把工作区与后续提交中的真实基础设施地址替换为 RFC 2606 文档域（`example.com` / `example.net` / `example.org`）与 RFC 1918 内网段；2026-09-20 补上了该提交漏掉的一处大小写变体（白名单归一化用例的输入端，见 `settings_test.go`）。

但 **git 历史是不可变的**：在 `b640c33` **之前**的旧提交里，以下 9 个真实值仍可从分支历史取回（2 个公网 IP + 7 个真实域名，涉及约 55 处提交）：

- 公网 IP ×2、真实域名 ×7（子域前缀均为通用词，如 `repo` / `maven` / `bak.maven` / `tmp` / `repo1` / `t2`；域名后缀不在本文重复，以免在公开仓库里二次暴露）

**决策（2026-09-20）**：这些地址均为个人基础设施标识，**不含任何密钥、凭据或个人身份数据**；经评估判定为低风险，**接受现状、不做历史重写**。理由：

1. 重写历史（`git filter-repo` + force-push）会改写全部提交哈希、破坏既有 tag 与 Release 的对应关系，收益（隐藏服务器地址）与代价不成比例；
2. 防护不应依赖“地址保密”——这些节点的实际防线是安全组/防火墙（只放行 CDN 回源）与 Host 白名单 + 回源 Token（见上两节）；
3. 远端平台缓存、fork 与 PR 可能仍留有旧值，重写历史并不能彻底消除。

**注意**：若未来这些地址开始承载敏感服务或暴露面扩大，应优先调整其访问控制，而不是回头清理历史。对仓库运行、发布与审计无影响。

#### 依赖安全告警的跟踪

仓库已开启三道依赖安全通道（2026-09-20）：

1. **Dependabot alerts**（仓库设置级）：报告 manifest 与传递依赖的已知漏洞，入口在 GitHub 仓库的 Security 标签页；
2. **Dependabot security updates**：对每条 alert 自动开修复 PR，由 `ci.yml` 的质量门验证后人工合并；
3. **osv-scanner workflow**（`.github/workflows/osv-scanner.yml`）：PR / push dev / 每周定时扫描 lockfile，结果以 SARIF 上报 Code Scanning（软失败，不阻断）。

**当前存量（2026-09-20 登记，待处理）**：Dependabot alerts 共 30 条（critical 5 / high 20 / medium 18 的标注口径，按 alert 计 30），集中在 **vitest / vite / esbuild 等**开发工具链（`scope: development`，不进生产 bundle——产物隔离测试已验证 devmock/MSW 不入包），另有 `brace-expansion` / `fast-uri` / `js-yaml` 标为 runtime。**处置约定**：涉及 vitest 2→3、vite 5→7 等主版本升级，需按依赖升级流程（全量回归验证）另行安排，不在日常小版本更新中顺带处理；实际数字与清单以 Security 标签页实时状态为准，本文不维护快照。

### 1.4 指标导出（Prometheus，FR-39）

`GET /metrics` 以 Prometheus 文本暴露格式输出进程内指标，供标准抓取与告警：

```bash
curl -fsS http://127.0.0.1:8080/metrics | head
```

- **指标口径**：`jianartifact_protocol_requests_total{method,status,cache_result}`（制品协议请求完成计数，缓存结果取 `hit` / `miss` / `unknown`）、`jianartifact_publish_rejections_total{reason}`（发布被拒绝的累计次数，见下）、`jianartifact_upstream_block_events_total{reason}`（上游断路器阻止事件的累计次数，见下）、`jianartifact_upstream_blocked_repositories`（当前处于阻止态——自动阻止或半开试探——的上游仓库数，无标签）、`jianartifact_scheduler_job_*{job}`（定时任务作业的执行 / 失败次数、是否运行中、最近执行时间与最近是否失败）、`jianartifact_runtime_*`（协程数、堆占用、累计 GC 次数）。
- **发布拒绝原因（闭集枚举）**：`jianartifact_publish_rejections_total{reason}` 的 `reason` 是**闭集枚举**，v1 只有 `quota` 一个取值——它同时覆盖「发布额度拒绝」与「仓库存储配额拒绝」这两类共用 429 `quota_exceeded` 口径的拒绝。**闭集之外的取值会被直接忽略**（标签基数受控）；**新增取值必须先改规格**（[`specs/0.11.0-storage-governance.md`](specs/0.11.0-storage-governance.md) §3.5）并同步测试与本文，不允许就地塞入自由字符串。该指标族即使尚无拒绝样本也输出 `# HELP` / `# TYPE`，样本行只在真的发生过拒绝后出现。
- **上游断路器指标（FR-43）**：`jianartifact_upstream_block_events_total{reason}` 的 `reason` 同样是**闭集枚举**，v1 有 `probe_failed`（探测失败导致阻止窗口延长）与 `upstream_error`（回源失败触发自动阻止）两个取值；闭集之外的取值被直接忽略。该族即使尚无阻止事件也输出 `# HELP` / `# TYPE`。`jianartifact_upstream_blocked_repositories` 报告当前处于 `AUTO_BLOCKED` 或 `HALF_OPEN` 的仓库数——**按仓库名打标签会让标签基数随仓库数无界增长，故该指标刻意不带任何标签**；排障时要定位到具体仓库，请查管理界面的仓库列表 / 详情或 `/api/v1/operations/alerts` 的 `upstream_auto_blocked` 告警。**新增取值必须先改规格**（[`specs/0.12.0-upstream-circuit-breaker.md`](specs/0.12.0-upstream-circuit-breaker.md) §3.3）并同步测试与本文。
- **抓取开销**：全部取自进程内数据，**不查询数据库、不访问网络**；标签基数受控——未知方法与异常状态码归 `other`，路径 / 仓库名 / 用户等无界值不做标签。
- **匿名口径**：与 `/healthz`、`/readyz` 一致免认证，但**不含版本等指纹信息**；需要限制访问范围时在网络层收口（安全组 / 反向代理）。
- **抓取配置示例**：

```yaml
# Prometheus 抓取配置片段：一次性接入本实例指标
scrape_configs:
  # 抓取任务名，可自定义
  - job_name: jianartifact
    # 指标端点路径（固定）
    metrics_path: /metrics
    # 抓取间隔与超时按需调整；指标为进程内聚合，抓取很轻
    scrape_interval: 30s
    # 抓取目标：换成实际域名与端口（示例为文档保留域名）
    static_configs:
      - targets: ["jianartifact.example.com:8080"]
```

### 1.5 外部身份源（OIDC，FR-34）

配置下列变量即启用 OIDC 登录（**未配置 issuer 时整组不启用**，相关端点返回 404；见 [ADR-0029](adr/0029-external-identity-providers.md)）：

| 变量                        | 说明                                | 取值                                                                                   |
| --------------------------- | ----------------------------------- | -------------------------------------------------------------------------------------- |
| `JIAN_OIDC_ISSUER`          | 身份提供方 issuer（discovery 基址） | 必填，如 `https://idp.example.com`                                                     |
| `JIAN_OIDC_CLIENT_ID`       | 客户端 ID                           | 必填                                                                                   |
| `JIAN_OIDC_CLIENT_SECRET`   | 客户端密钥                          | 必填；**只从环境变量读取**，不入库、不打印、不进日志                                   |
| `JIAN_OIDC_REDIRECT_URL`    | 回调地址                            | 必填，须与 IdP 侧注册完全一致，如 `https://repo.example.com/api/v1/auth/oidc/callback` |
| `JIAN_OIDC_USERNAME_CLAIM`  | 用户名 claim                        | 缺省 `preferred_username`（以 IdP 实际下发的 claim 为准）                              |
| `JIAN_OIDC_ALLOWED_DOMAINS` | 允许自动建号的邮箱域名              | 逗号分隔；空 = 不限制；配了就只放行命中域名                                            |

- **半配置直接拒绝启动**：配了 issuer 但缺客户端 ID / 密钥 / 回调地址会启动报错，不会"以为启用了实际没有"。
- **永不锁死**：IdP 不可达只影响 OIDC 来源用户的登录，本地管理员与本地账号照常登录（离线恢复用 `jianartifact admin reset`）。
- **绑定与建号**：首次登录按 `sub` 绑定既有账号 → 按用户名绑定本地普通账号 → 建号（角色固定 `user`，可见性仍完全由 ACL 决定）；管理员账号与内置主体**不会被自动绑定**。
- **排障**：登录失败时服务端日志记录具体原因（流程过期 / state 不符 / 验签失败 / 白名单拒绝），前端只看到笼统错误码，不泄露账号是否存在。

### 1.6 外部身份源（LDAP，FR-35）

配置下列变量即启用 LDAP 登录（**未配置 `JIAN_LDAP_URL` 时整组不启用**，登录不回退目录）：

| 变量                      | 说明                                         | 取值                                                                 |
| ------------------------- | -------------------------------------------- | -------------------------------------------------------------------- |
| `JIAN_LDAP_URL`           | 目录地址                                     | 必填，`ldap://` 或 `ldaps://`，如 `ldaps://ldap.example.com:636`     |
| `JIAN_LDAP_BASE_DN`       | 用户检索基准 DN                              | 必填，如 `ou=people,dc=example,dc=com`                               |
| `JIAN_LDAP_BIND_DN`       | 检索用服务账号 DN                            | 可选；**留空表示 DN 模板模式**（不做检索，直接以模板推导的 DN 绑定） |
| `JIAN_LDAP_BIND_PASSWORD` | 服务账号口令                                 | 配了 `BIND_DN` 则必填；**只从环境变量读取**，不入库、不打印          |
| `JIAN_LDAP_USER_FILTER`   | 用户检索过滤器，**必须含 `{username}` 占位** | 缺省 `(uid={username})`；AD 常用 `(sAMAccountName={username})`       |
| `JIAN_LDAP_EMAIL_ATTR`    | 邮箱属性名                                   | 缺省 `mail`                                                          |
| `JIAN_LDAP_STARTTLS`      | 对 `ldap://` 使用 StartTLS                   | `true` / `1` 开启；与 `ldaps://` 同时配置会报错                      |
| `JIAN_LDAP_CA_FILE`       | 自定义 CA（PEM）路径                         | 可选；留空用系统根证书。**不提供跳过证书校验的开关**                 |

- **启动期直接拒绝**：非 `ldap(s)` 协议、缺基准 DN、过滤器缺 `{username}` 占位、配了服务账号却没给口令、StartTLS 与 ldaps 冲突、CA 文件不可读——都会启动报错，不静默半启用。
- **登录顺序**：本地口令优先；本地未通过且启用 LDAP 时才回退目录。两侧都失败时对外表现一致（不泄露账号存在于哪一侧）；离职在目录侧停用后**无法再次登录**（已签发会话在其过期前仍有效，与本地账号语义一致）。
- **永不锁死**：目录不可达只影响 LDAP 来源用户的登录，本地管理员与本地账号照常登录。
- **排障**：服务端日志记录目录侧失败原因（连接失败 / 检索无命中或命中多个 / 绑定被拒），前端只看到笼统失败，不泄露账号是否存在。

### 1.7 维护作业 API 与存储配额治理（FR-41）

**本批只交付 API，没有配套管理页面**——作业清单、手动触发与配额调整都通过 HTTP 调用（配额的编辑入口在仓库详情页的配置页签，见下）。

**看作业清单**（仅管理员）：

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/maintenance/jobs
```

返回本进程**已注册**的周期作业（现状为 `blob-gc`、`storage-cleanup` 与 `backup-auto`）及状态：名称、间隔秒数、是否运行中、累计执行 / 失败次数、最近开始 / 结束时间、最近错误。要点：

- 清单由注册决定——**间隔 ≤ 0 被禁用的作业不注册，也不出现在清单里**，所以"某个作业没出现"等价于"该作业在本实例被禁用"（例如 `JIAN_STORAGE_CLEANUP_INTERVAL=0`）；
- **`backup-auto` 是例外，须留意**：它的间隔改为在运行时可配（见 §1.1），因此作业**始终注册**，清单里**总是能看到它**，其 `intervalSeconds` 恒为心跳值 30。它是否真正在备份，取决于「备份与搬迁」页的自动备份间隔是否为 0——**看到 `backup-auto` 不代表自动备份已启用**；
- 计数自进程启动累计、重启归零；未执行过的作业，时间与错误字段为 `null`。

**手动触发一次**（仅管理员）：

```bash
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8080/api/v1/maintenance/jobs/storage-cleanup/run
```

- **202 只表示已受理**，不等待作业执行完成；是否跑完、是否失败，回查清单接口的 `running` / `failures` / `lastError`。
- **409 `job_running`**：作业正在运行（含周期触发的那一轮，也含本接口上一次触发尚未结束的那一轮）——不排队、不并发重入、后续不补跑。
- **404 `not_found`**：作业名未注册或已被禁用（如把间隔设成 0 后重启）。
- **403**（非管理员）/ **401**（未登录）；调度器未接线时 503 `unavailable`（兼容分支，生产装配恒为已接线）。
- 触发成功写审计动作 `maintenance.job_run`。维护命名空间在写入冻结窗口的放行清单内，冻结期间仍可查清单与手动触发。
- 手动触发**不会**让作业变得"更快"：调度语义不变，触发只是额外跑一轮。

**一键重启服务**（仅管理员，FR-40 追加七）：

```bash
# 只读探测：本机能不能自动重启、手工等效命令长什么样（不会触发重启，可安全反复调用）
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/maintenance/restart

# 真正受理重启：响应先回（202），约 delayMs 毫秒后开始关停并自替换
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/maintenance/restart
```

用于「备份恢复后生效」或按需重载进程（也用于搬迁流程的"起服"一步）。要点：

- **执行路径：进程自替换（`syscall.Exec`），不执行任何外部命令。** 这是本功能**唯一且默认**的执行方式，也是它不需要任何权限的原因：`jianartifact` 本就有权执行自己的二进制（systemd 的 `ExecStart` 就是它），`syscall.Exec` 只是"同一个进程换一份新映像"——不 fork、不新建进程、不引入任何新权限。因此：
  - **不需要 sudo、不需要 polkit 规则、不需要 sudoers 条目、不需要 lingering 之外的任何 systemd 授权**；
  - **不受 `NoNewPrivileges=true` 影响**（该加固项限制的是**提权**，而自替换压根不提权；系统级单元、rootless 用户级单元、容器里都一样可用——实机验证：在 `NoNewPrivileges=true` + `ProtectSystem=strict` + `ProtectHome=read-only` + `PrivateTmp=true` 全套加固下自替换成功，PID 不变、PPID 仍为 1、`status=0/SUCCESS`）；
  - 端口可原样重绑：Go 的 socket 默认带 `CLOEXEC`，内核在 exec 时自动关闭旧 fd，因此即使不显式关闭监听器也能重新绑定同一端口（实测）。
- **两个必须知道的语义差异**（与"用 systemctl 重启"相比，运维视角上确实不同）：
  - **① unit 状态不变**：systemd 视角上这个单元**从未停止过**——PID 不变，`ActiveEnterTimestamp` / `NRestart` 都不动，`systemctl status` 看不出重启过，`Restart=on-failure` 的重启计数也不会增加。这是**可以接受的**：本功能要的是"应用重新执行一遍启动流程"（备份恢复落地、搬迁起服就靠这条），而不是"让 systemd 记一笔重启"。要判断是否真的重启过，请**看应用日志**（启动横幅 `JianArtifact <version> 正在监听 …`）而不是 `systemctl status`。反过来这也意味着：**`systemctl restart` 能观察到的东西，这里观察不到**；需要那种语义时就手工执行 `command` 里的命令。
  - **② 存在一个短暂的中断窗口**：顺序是"先优雅关停（停止接受新请求、等在途请求收尾，上限 15 秒）→ 再替换映像"。这期间服务不可用，通常几秒；关停失败会**如实报错并放弃自替换**（不静默带病换映像）。
- **`command` 字段是"手工等效命令"，不是本进程会执行的命令。** 自动重启走自替换、不执行任何命令；`command` 回答的是"若你想**在终端里手工**重启本节点，敲这条"，因此它随部署形态与运行身份而变，由后端按 `JIAN_RESTART_SCOPE` 生成，任何情况下都返回、可直接复制（配置非法时是一句说明性注释，不回显配置原文）。
- **`GET` 是只读探测，绝不排定重启**：它返回与 `POST` 相同的结构（`supported` / `command` / `reason` / `scope`，但**不含 `delayMs`**，因为没受理就没有延迟），探测口径与 `POST` 完全一致。**不要用 `POST` 代替探测**——`POST` 在 `supported:true` 时会当场排定重启，"打开页面即重启"。`GET` 不写审计（只读），`POST` 才写。
- **界面上的可复制命令来自这个只读探测，因此与本机部署形态一致**：只有后端知道本机该敲哪条手工命令，四种形态见下。界面不会给出照做必失败的 `sudo` 命令——例如**仓库自带的 rootless 用户级单元**下探测给出的是 `systemctl --user restart <unit>`，而不是那条在该形态下必然被拒的 `sudo systemctl restart <unit>`。
- **先回响应，再重启**：返回 **202** 时重启**尚未开始**，真正开始（优雅关停 → 自替换）在 `delayMs` 毫秒后（约 1.8 秒），留出响应送达与前端展示的余量。**看到 202 后连接随即断开是预期行为**，不是失败——请停止轮询本节点，等它重新起来。
- **`supported: false` 是正常业务结果，用 HTTP 200 返回**（不是 4xx/5xx）：服务在受理前会**同步预检**四条判据——① 本平台是否支持自替换（`syscall.Exec` 仅 Unix）；② `os.Args[0]`（自替换要加载的路径）能否解析为可执行文件；③ 重启触发点是否已接线；④ 单元名与作用域是否合法。不通过时如实返回 `reason`（原因）与 `command`（手工等效命令），供界面渲染降级 UI。**服务不会假装成功**。
- **四种部署形态对应的"手工等效命令"**（**注意**：这只是"若你要手工重启，敲哪条"，**不是自动重启的前提**——自动重启在任何形态下都走自替换、都不需要这些命令背后的授权）：
- **① rootless 用户级单元（仓库自带模板就是这一种）**：`JIAN_RESTART_SCOPE=systemd-user` 时手工命令为 `systemctl --user restart <unit>`（单元名默认 `jianartifact`）。仓库自带模板 [`deploy/systemd/jianartifact.service`](../deploy/systemd/jianartifact.service) 是**用户级单元**（装到 `~/.config/systemd/user/`，用 `systemctl --user` 管理），[`deploy/deploy.sh`](../deploy/deploy.sh) 的默认 `SYSTEMD_SCOPE="user"` 与之配套——**这才是仓库的正规部署形态**。**手工执行这条命令也不需要 sudo、不需要 polkit，且不受单元里 `NoNewPrivileges=true` 的影响**：该加固项限制的是提权，而用户级重启走的是该用户自己的 systemd 实例，本就不涉及提权（实机验证：在与服务进程完全相同的限制下执行 `systemctl --user restart`，`exit=0`）。两个前提只影响**手工命令**能否跑通（以及你需要 SSH 登入后执行它）：
  - **可达性**：`systemctl --user` 要靠 `XDG_RUNTIME_DIR`（通常为 `/run/user/<uid>`）找到用户级 systemd 实例。你自己的交互式 shell 通常已有这个变量；若在脚本 / cron 里执行，可能需要显式给出 `XDG_RUNTIME_DIR=/run/user/$(id -u)`。
  - **常驻性**：用户级单元默认随该用户最后一个会话结束而停止，**要让它无人登录时也常驻，必须开 lingering**：`loginctl enable-linger <user>`（核对：`loginctl show-user <user> -p Linger` 应为 `Linger=yes`）。这是"用户级能替代系统级"的硬前提。
- **② 系统级单元（进程非 root）**：`JIAN_RESTART_SCOPE=system` 时手工命令为 `sudo -n systemctl restart <unit>`。**这属于偏离自带设计的形态**，手工执行它需要额外授权：

  ```sudoers
  # /etc/sudoers.d/jianartifact-restart（用 visudo -f 编辑）
  # 只放行这一个单元的重启。不要写 ALL。
  admin ALL=(root) NOPASSWD: /usr/bin/systemctl restart jianartifact
  ```

  另一条硬约束：**该单元里不能有 `NoNewPrivileges=true`**——该 flag 置位时 sudo 直接报 `The "no new privileges" flag is set, which prevents sudo from running as root`（实测），手工命令必然失败。**注意这只影响手工命令**：界面上的"一键重启服务"按钮走自替换，在带 `NoNewPrivileges=true` 的系统级单元下**照样可用**（这正是本功能改为自替换的直接起因）。

- **③ 系统级单元（进程本身是 root）**：手工命令为不带 sudo 的 `systemctl restart <unit>`（root 本就有权管单元），不需要任何额外授权。
- **④ 容器 / Docker / K8s**（`JIAN_RESTART_SCOPE=docker`）：**明确不支持自动重启**——容器生命周期由外部编排掌握，自替换只能换掉容器内的一个进程，编排层该做的重建不会被触发；容器里也没有 systemctl，`docker restart` 需要宿主机的守护进程（通常未挂载进容器）。预检一律回 `supported:false`，界面降级为显示手工命令。**降级命令里那一串由后端固定给出为 `docker restart <unit>`，而它只是形态示意**：实例实际由 compose 或 k8s 编排时，真正能让改动生效的命令要换成 `docker compose restart <service>` 或 `kubectl rollout restart deployment/<name>`，并在**宿主机**（不是容器内）执行。宁可让运维多点一次，也不假装成功。
- **预检与降级**：判据见上（平台支持 / `argv[0]` 可解析 / 触发点已接线 / 配置合法）。任一不满足即降级为 `supported:false`，`reason` 说明具体是哪一条不满足——例如 `argv[0]` 不可解析时原因会**点名那条路径与底层错误**（"二进制被换掉 / 部署目录的符号链接悬空 / 权限不对"是"重启没生效"最常见的根因，必须让人一眼看到）。`command` 里的命令仍可手工执行。
- **关于 `argv[0]` 与部署目录符号链接**（排查时最值得先看的一条）：自替换加载的是 **`os.Args[0]`**，即 systemd `ExecStart` 里写的那个路径（例如 `/opt/jianartifact/current/jianartifact`），其中 `current` 是部署脚本维护的符号链接。内核在 exec 时**重新解析**该符号链接，因此"先切 `current` 再一键重启"会真正加载到新版本。这也意味着：**若你把 `current` 指向别处、或让那个路径不可执行，一键重启会如实降级**（并在 `reason` 里说明），而不会静默地加载错的东西。
- **配置**：`JIAN_RESTART_UNIT`（默认 `jianartifact`）与 `JIAN_RESTART_SCOPE`（默认 `system`，可选 `systemd-user` / `docker`，非法值回落 `system`）。**推荐把 `systemd-user` 与仓库自带的用户级单元配套使用**（见上面第 ① 种形态），这样界面上的手工命令与实际部署形态一致。两者都只影响 `command` 这条**手工命令的展示**，不影响自替换本身（自替换加载的是本进程自己的 `argv[0]`，与单元名无关）。
- **安全**：单元名**只**从环境变量读取，**绝不**从请求体读取（那是直通命令注入的路径），且须匹配 `[A-Za-z0-9._@-]`——含 `;` / `$()` / 空格 / 换行等元字符时一律降级，`command` 只给说明性注释、不回显配置原文（它会被人复制到终端粘贴执行）。**自替换本身没有注入面**：`argv0` / `argv` / `env` 全部来自本进程自身，没有任何外部输入参与。
- 审计动作 `maintenance.service_restart`：受理记 `ok`（详情含 `command`）、降级记 `failed`（详情含原因）。

**仓库存储配额与代理缓存保留**：

- 仓库级配额 `quotaBytes`（字节，计量口径为**逻辑字节** `SUM(asset.size)`）与 `quotaAssets`（制品数）在 **`hosted` 仓库**的详情页「配置」页签可编辑（留空 / 0 = 不限），也可直接 `PATCH /api/v1/repositories/{name}`；越限后该仓库的写入被拒绝（**429 `quota_exceeded`**，消息含当前占用与上限）。**配额只对 `hosted` 仓库生效**（非 `hosted` 仓库不提供该输入，PATCH 非 0 配额会 400），且计量口径与去重后的物理磁盘占用不等价。
- 迁移导入与备份恢复**豁免**配额（属管理员批量操作），豁免只记日志、不进审计。
- 代理缓存保留 `cacheRetentionDays`（**仅 `proxy` 仓库**、**默认关闭**）：开启后由 `storage-cleanup` 作业淘汰该仓库内"最后一次写入早于「当前时间 − 保留天数」"的缓存资产，删除走既有资产删除通道；每轮删除有数量上限（当前 200 个），达到上限的部分留到下一轮。**系统发起的删除只记日志、不写审计**。
- 排障：对"配额被打满"设告警可用 `jianartifact_publish_rejections_total{reason="quota"}`（见 §1.4）；清理作业的动作与计数看作业日志（形如"存储治理清理完成：终态操作 N 行 / …"）与 `jianartifact_scheduler_job_*{job="storage-cleanup"}`。

## 2. 部署路径

### 2.1 Docker Compose（单实例主路径）

```bash
Copy-Item deploy/.env.example deploy/.env
# 编辑 deploy/.env，填入真实密钥和本地路径
docker compose -f deploy/docker-compose.yml up -d
curl -fsS http://127.0.0.1:8080/readyz
```

Compose 使用命名卷保存 `/data`。首次启动后通过 `POST /api/v1/auth/bootstrap` 或本地 `jianartifact admin reset` 创建管理员。生产的 `deploy/.env` 不入库。

### 2.2 rootless systemd / SSH 二进制

**部署形态**（三者缺一不可，改动任一处都要同步另外两处）：

1. **二进制走版本目录**：每次部署解包到 `${DEPLOY_DIR}/releases/<时间戳>/`，再把 `current` 符号链接原子切过去；回滚只需切回上一版本目录。
2. **进程由 systemd 托管**：unit 的 `ExecStart=%h/jianartifact/current/jianartifact run`（**指向 `current`，不是固定路径**——否则切了 `current` 服务仍跑旧二进制）。**作用域由环境文件里的 `DEPLOY_MODE` 决定**，部署脚本据此选重启命令：`systemd`=系统级单元（`/etc/systemd/system`，`systemctl restart`）、`systemd-user`=用户级 rootless 单元（`~/.config/systemd/user`，`systemctl --user restart`，需 lingering）。选错作用域会报 `Unit not found` 或 `Failed to connect to bus`，脚本会在重启处直接失败并提示该改哪个值，不会静默切版本。发布根取自环境文件的 `DEPLOY_DIR`（自定义路径必须显式给出，不要依赖 home 推导）。
3. **环境变量的真源是 `EnvironmentFile`**：`${DEPLOY_DIR}/jianartifact.env`（600 权限，含 `JIAN_DATA_DIR` / `JIAN_HTTP_ADDR` / `JIAN_PUBLIC_URL` / `JIAN_ENABLED_FORMATS` / `JIAN_JWT_SECRET` 等）。**部署脚本不会写入或覆盖它**——写入会丢掉格式配置、并把 JWT 换成脚本内置值（导致所有登录态失效）。unit 里不再写 `Environment=`，密钥也不出现在 unit 文件中。

**脚本分工**：

```bash
# 密钥（首次）：生成密钥并打印公钥，粘贴到主机 ~/.ssh/authorized_keys
bash deploy/remote-ssh.sh setup-key

# 部署：构建 Linux 二进制 → 委托 deploy.sh（切 current + systemctl 重启 + 探活失败自动回滚）
# DEPLOY_ENV 选择环境文件：DEPLOY_ENV=prod → deploy/.env.prod（不入库，按环境存放）
DEPLOY_ENV=prod bash deploy/remote-ssh.sh deploy

# 探活 / 回滚（后者切回上一版本目录）
DEPLOY_ENV=prod bash deploy/remote-ssh.sh health
DEPLOY_ENV=prod bash deploy/deploy.sh rollback
```

系统级与用户级实例走同一条路径，差别只在环境文件里的 `DEPLOY_MODE`（系统级写 `systemd`、用户级写 `systemd-user`）与 `DEPLOY_DIR` / `DEPLOY_SERVICE` / `HEALTH_URL` 的取值；两站的部署命令完全相同。

`remote-ssh.sh` 只做密钥、构建与登录入口，部署一律委托 `deploy.sh`——历史版本曾自行 `kill + nohup + pid` 启动，与 systemd 托管冲突（双进程抢端口）且只写 3 个环境变量，已移除。

`KEEP_RELEASES`（默认 5）控制保留的历史版本目录数，超出部分在部署成功后清理。

重启、查进程和回滚必须限定部署用户与服务实例，禁止使用无范围的 `pkill` 或清理命令影响同机其他服务。

### 2.3 Helm / Kubernetes

`deploy/helm/` 和 `deploy/k8s/` 当前是单实例、RWO 持久卷和 `Recreate` 策略；它们不是多副本 HA 模板。生产 Secret 通过 Secret 管理系统注入，不在清单中提交真实值。探针使用 `/healthz` 和 `/readyz`。

### 2.4 测试实例隔离

测试站必须使用独立的服务单元、发布目录、环境文件、`JIAN_DATA_DIR`、SQLite、blob、日志和测试制品。不得挂载、复制、清理或复用生产数据目录。真实域名、端口、密钥和远端目录只能写入部署机忽略的本地环境文件。

## 3. 迁移运维

### 3.1 在线 Nexus 迁移

管理员可以提交已校验的 Nexus 基址和匿名、Basic 或 Bearer 认证。新任务的来源认证以 AES-256-GCM 密文保存，重启恢复必须保持 `JIAN_MIGRATION_CREDENTIAL_KEY` 不变；旧 `sourceRef`/`credentialRef` 任务继续从运行时引用解析。

来源 URL 不得包含用户信息、查询、片段或凭据。Nexus 返回的 `downloadUrl` 必须与来源基址同 origin，否则在发送凭据前拒绝。任务、报告、日志和审计只保留脱敏错误分类。

### 3.2 离线包与离线目录

> 本节有两种含义完全不同的"离线包"，勿混用：
>
> - **外部导入包（Nexus 迁移用）**：从外部制品库搬入，见下面第一种布局。
> - **节点备份包（搬迁/备份用）**：把本实例整体搬到另一台机器，见第二种布局。

**外部导入包布局（Nexus 迁移来源）**：

```text
bundle/
  manifest.json
  content/<repo>/<path...>
```

**离线目录夹具布局（Nexus 迁移来源）**：

```text
<data>/repositories/<repo-name>/.format
<data>/repositories/<repo-name>/content/<path...>
```

全量迁移可能占满磁盘；优先在发现或显式启动时选择仓库，完成后检查报告和抽样 SHA-256。

**节点备份包布局（搬迁/备份，FR-132 起）**：

```text
jianartifact-backup-<packageId>.tar.gz
  manifest.json          # kind=jianartifact-node-backup，含计数与摘要
  jianartifact.db        # VACUUM INTO 一致性快照
  blobs.index            # 每行 "<sha256> <size>"
  blobs/<xx>/<yy>/<hash> # 仅快照库 asset 表引用到的 blob
```

包内**仅含数据，不含任何密钥或节点本地配置**：排除各类密钥文件（JWT / 迁移凭据 / 历史复制凭据密钥）、环境文件、`*.db-wal`、`*.db-shm`、`blobs/tmp/`、`blobs/quarantine/`。因此新机器导入后必须自行配置密钥与对外地址。

包落在 `${JIAN_DATA_DIR}/backups/`。两种生成模式：

- **热备份**（`hot`）：不停服。用独立连接对 SQLite 执行 `VACUUM INTO` 取一致性快照；blob 集合以快照库 `asset` 表为准，快照之后新写入的 blob 不被引用、自然排除。
- **冻结窗口**（`frozen`）：先停写再取快照，语义更严格。Web 端由服务端执行冻结/解冻；CLI 的 `--mode frozen` 假定本地写入已停止，若服务仍在运行请改用 Web 的冻结窗口，或先停止服务。

包为明文（不含密钥，但含全部业务数据）。**下载链接必须走 HTTPS，并使用短有效期**。

### 3.3 Cutover 检查

以节点备份包搬迁时的切换顺序：

1. 旧机生成**热备份包**并传输（在线完成，可提前数小时甚至数天）。
2. 让 CI/客户端指向 JianArtifact 前，先冻结旧机写入。
3. 生成**增量差包**（只含新增 blob 与新 db）并传输。
4. 新机导入（校验 → 暂存 → 重启替换），抽样检查关键路径下载、协议响应和校验和。
5. 确认新机数据、制品下载与账号登录与旧机一致后切 DNS，并解除旧机冻结或直接退役旧机。

若不做增量，也可在冻结窗口内传一个完整包；切换窗口会相应变长。

**用增量差包把停机窗口缩到分钟级**（推荐）：冻结旧机 → 以基线包为基准 `jianartifact backup create --base <基线包标识>` 生成差包（只含新增 blob + 新 db）→ 仅传输差包 → 新机导入 → 起服 → 解冻。这是增量差包的主要运维价值：停机只覆盖"生成差包 + 传输差包 + 导入 + 起服"这一小段，而非整个完整包的传输；基线包可在搬迁前提前传到新机并完成导入（状态 `done`、侧车索引就位），切换当天只需补一份差包。

### 3.4 写入冻结窗口操作步骤（FR-135）

搬迁切换先用冻结窗口停写，再生成差包/导入，最后解冻起服。仅管理员。

1. **冻结写入**：`POST /api/v1/maintenance/freeze`，可给 `until`（绝对时间，须晚于当前且不超过 `now+24h`）或 `ttlSeconds`（相对秒，缺省 7200，范围 [60, 86400]）；两者都省略时用缺省 7200。窗口**必须有界**——故意不提供无限期冻结，因为忘记解冻会让服务退化成假死（业务与管理写全被 503 拦死）。
2. **确认写被拦**：尝试任意业务/管理写（如新建仓库、改设置），应得到 `503` + 错误码 `write_frozen`。`GET /api/v1/maintenance/freeze` 可随时查当前状态（`frozen`/`until`/`frozenAt`/`reason`；未冻结时省略 `until`/`frozenAt`）。
3. **生成差包 / 拉取导入**：在冻结窗口内生成增量差包（见 §3.3）或通过 `POST /api/v1/backups/import` 拉取外部包；导入只写 `restore-staging/` 与 `restore.pending`，重启才生效，不破坏冻结语义。
4. **解冻**：`DELETE /api/v1/maintenance/freeze`（幂等，未冻结也返 200）。若用了 `until`/`ttlSeconds`，到期也会自动解冻。

**冻结期放行清单**（避免"冻上就解不开"）：读方法（GET/HEAD/OPTIONS）、`POST /api/v1/auth/login`、维护命名空间 `/api/v1/maintenance/`、备份导入/上传路径（`/api/v1/backups/import{s}`、`/api/v1/backups/uploads`）。这些路径只写恢复暂存与标记、重启才生效，所以放行它们不会破坏"停写"的语义；拦掉它们反而会让冻结期间无法完成搬迁切换。

## 4. CLI 运维

单二进制必须显式传入子命令；无参数或未知命令只打印用法。

**先确认数据目录**：所有 CLI 子命令都作用于 `${JIAN_DATA_DIR}`。未显式设置时，CLI 会尝试自动发现正在运行的实例并打印所用目录与依据（见 §3「数据目录解析」）。**在服务机上执行运维命令前，请先核对这一行输出**——它决定了你操作的是哪一个实例。

- `jianartifact run`：启动服务。
- `jianartifact status`：读取本地/在线状态、版本、就绪和数据摘要。
- `jianartifact healthcheck`：读取 `/readyz`，供容器探活。
- `jianartifact admin reset`：离线创建或重置管理员。
- `jianartifact admin backfill-checksums`：流式补齐历史资产校验和。
- `jianartifact admin backfill-times`：按 Nexus 时间回填资产时间。
- `jianartifact admin emit-asset-times`：为存量资产重新登记带创建/更新时间的资产变更记录（FR-91 遗留能力，当前无复制对端消费）。
- `jianartifact backup create [--mode hot|frozen] [--label <备注>]`：生成节点备份包并登记。`frozen` 假定本地写入已停止。
- `jianartifact backup create --base <packageId> [--mode hot|frozen] [--label <备注>]`：以某基线包生成**增量差包**，只携带新增 blob + 新 db，把搬迁停机窗口从"传整个包"缩到"只传新增 blob + 新 db"（分钟级）。基线必须存在、状态 `done`、且其**侧车索引** `${JIAN_DATA_DIR}/backups/<packageId>.index` 存在，否则明确报错。
- `jianartifact backup list [--json]`：列出本机备份包（状态、大小、创建时间、失败摘要）。
- `jianartifact backup verify <包标识|归档路径> [--deep]`：校验完整性。不带 `--deep` 只比对 db 与 `blobs.index` 摘要；带 `--deep` 逐 blob 比对内容摘要（耗时与包体积同阶）。目标既可以是已登记的包标识，也可以是外部传入的归档文件路径。
- `jianartifact backup drill <包标识> [--level shallow|deep]`：**恢复演练**（FR-40），**默认 deep（深校验）**——浅校验只验归档结构，对「内容被静默篡改但 tar/gzip 结构完好」的包会判为通过，而演练回答的是"这份包真的能恢复吗"，故默认取更严格的一档；需快速预检时显式传 `--level shallow`。在隔离临时目录内真解包并校验（归档完整性 → 解包 → 解出的 SQLite `quick_check` + 关键表可查询 → 解出的 blob 数与 manifest 一致），产出结构化报告并落库；未通过时非零退出且报告仍落库。**只读路径：不写 `restore.pending` / `restore-staging/`，不改动本实例、无需重启**。与 `backup verify` 的区别：verify 只校验归档字节（可对任意文件路径），drill 回答的是"这份包真的能恢复吗"。详见 §5.2「恢复演练」。
- `jianartifact backup link <包标识> [--ttl 30m] [--base https://对外地址]`：签发带时效的下载链接，供新机器直接拉取。`--base` 缺省时回退 `JIAN_PUBLIC_URL`；两者都为空则报错（不签相对链接）。有效期钳制在 1 分钟 ~ 24 小时。
- `jianartifact backup delete <包标识>`：删除包体与登记；被增量包引用的基线会被拒绝。
- `jianartifact backup import <归档路径> [--overwrite] [--deep] [--yes]`：校验备份包并暂存，**需重启服务后生效**。`--overwrite`：目标实例非空时必需（内置 `anonymous` 主体不计入"非空"）；`--deep`：逐 blob 比对内容摘要（耗时与包体积同阶）；`--yes`：交互式终端下跳过"确认覆盖"的二次确认（无 TTY 时 `--overwrite` 即视为已确认）。失败不会留下待生效标记。重启后启动日志会打印已应用的恢复，并保留 `pre-restore-<ts>/` 作为回滚退路。**不要用 `sudo` 跑**：标记文件若属主为 root，服务（通常以普通用户运行）读不了它，需在重启前 `chown` 回服务用户（见 §3「数据目录解析」）。

## 5. 升级、备份与恢复

### 5.1 升级

1. 阅读目标版本 `CHANGELOG.md`、PRD 状态和迁移说明。
2. 备份 SQLite 与 blob。
3. 停止单实例或使用部署脚本原子切换版本。
4. 启动并等待 schema 迁移完成。
5. 检查 `/readyz`、管理员登录、关键仓库读取和制品抽样；可跑 `scripts/acceptance-spotcheck.py` 做一轮契约回归抽查（免认证断言对正式站也适用）。

SQLite + blob 是一个数据边界，默认不做滚动多副本升级。

**部署前的一致性快照**（部署失败要能立刻退回升级前的库）：

```bash
sqlite3 "${JIAN_DATA_DIR}/jianartifact.db" ".backup '${JIAN_DATA_DIR%/data}/pre-deploy-backup/jianartifact-$(date +%Y%m%d%H%M%S).db'"
# 目标机无 sqlite3 时用 Python 标准库的在线备份 API（等价于 .backup，不阻塞写入）
python3 - "$JIAN_DATA_DIR/jianartifact.db" "$dst" <<'PY'
import sqlite3, sys
s = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
d = sqlite3.connect(sys.argv[2])
with d:
    s.backup(d)
PY
sqlite3 "$dst" "PRAGMA quick_check;"   # 确认快照可读
```

**部署什么**：优先用该版本 tag 对应的 Release 附件，而不是本地重建，并用随附的 `SHA256SUMS` 校验；目标机出网良好时让它自行下载再校验，比经过运维机隧道搬运大文件更稳：

```bash
curl -fsSL -o /tmp/jianartifact "<Release 附件 URL>" && sha256sum /tmp/jianartifact
```

**确认跑的是哪个版本**：看启动日志行 `JianArtifact <版本> 正在监听`，对运行产物 `sha256sum` 与附件比对，或带管理员凭据请求 `GET /api/v1/status` 读 `version` 字段。

注意 `version` **按认证状态脱敏**：匿名请求一律返回空串（`/healthz`、`/readyz`、`/api/v1/status` 同此规则），这是防指纹的预期行为，**不是「版本没注入」**——用匿名请求判版本，会把正常实例误读成没有版本。

**含不可逆 schema 变更的升级（FR-36 起，迁移 `0046_user_groups.sql`）**：用户组与六档动作那次变更会**重建 `acl` 表**（SQLite 不允许 `ALTER TABLE` 改 CHECK 或追加带 REFERENCES 的列，故按「建新表 → 复制 → 删旧 → 改名 → 重建索引」整表重建），并新增 `user_group` / `user_group_member` 两表。既有 ACL 行按用户主体原样复制、`id` 与授权三元组逐条不变，但这是一次**一次性全表复制**（耗时随 `acl` 行数增长，重建期间占用写锁），且**不提供 down migration**：

- 升级**必须**先做一致性快照（上文 `sqlite3 .backup` 或节点备份包），并确认快照可读（`PRAGMA quick_check;`）后再切版本；
- 回滚**不能只回滚代码**——旧版本代码不认识 `acl` 新增的 `subject_type` / `subject_group_id` 两列，也无法理解六档动作，只回退二进制会让服务在新库上跑不起来或判错权限。降级必须**从升级前的备份恢复数据库**（与 §6 回滚「若新版本执行了不可逆 schema 迁移，必须同时恢复对应数据备份」同一口径）；
- 升级后可对若干仓库抽样核对 ACL 条目数与可见性未变（`user_group` / `user_group_member` 在全新库上为空属预期）；
- **权限口径变更需管理员关注**：动作由三档扩为六档后，**`write` 不再蕴含 `delete`**（`delete` 现在要单独授予）。本次不会让既有账号失去删除能力——升级前协议层删除本就**仅全局管理员可用**，所以能删的人照旧能删；变化是删除权**放开**成了可授予的仓库级动作。若此前为了「让某人能删某个仓库」而把该账号加成了全局管理员，升级后建议改为按仓库授予 `delete` 并回收全局管理员，避免实例级权限被当仓库级权限发放。

**迁移 `0050_asset_size_index.sql`：新增 `(repository_id, size)` 覆盖索引并删除被取代的 `idx_asset_repository`**：本次给 `asset` 加一个复合覆盖索引，让两条高频聚合不再回表读整行——仪表盘容量聚合（`COUNT(*)` + `SUM(size)`）改走完整覆盖索引扫描，仓库存储配额（`WHERE repository_id = ?` + `COUNT(*)` + `SUM(size)`，每次上传预检与提交点复检各跑一次）改走前导列等值定位的覆盖索引扫描。**不改表结构、不回填数据**，也不改变任何查询的返回口径，因此升级只需常规快照，不必按上面 `0046` 那样准备降级用的库恢复：

- 迁移耗时随 `asset` 行数增长，期间短暂占用写锁（与其他 DDL 迁移同口径）；
- 索引带来约 **+1.4 MB / 10 万行** 的库文件增长，这是用磁盘换聚合延迟的固定成本；同时**删除了 `idx_asset_repository`**（被复合索引的前缀完全覆盖），因此净增空间小于「新增一个索引」的字面值；
- **删除 `idx_asset_repository` 是替换而不是减少覆盖**：SQLite 对「只按 `repository_id` 过滤」的查询可以直接使用复合索引的前导列，原本走旧索引的查询改走 `idx_asset_repo_size`，查询计划不受影响。若观察到某条仓库维度查询变慢，请用 `EXPLAIN QUERY PLAN` 确认其计划——正常情况下应仍显示 `USING [COVERING] INDEX idx_asset_repo_size`；
- 降级不受阻：`DROP INDEX idx_asset_repo_size;` + `CREATE INDEX idx_asset_repository ON asset (repository_id);` 即可回到升级前的空间占用与查询计划；旧版本代码不认识新索引也不影响启动与读写（索引由 SQLite 自主维护，与代码无关）；
- 升级后仪表盘容量卡片与每次上传的配额检查会立刻受益；若观察到容量数字与升级前不一致，那是聚合重新计算的正常结果，索引只改变读法、不改变 `COUNT` / `SUM` 语义。

> **开发/预发环境注意：迁移编号曾被顺延过。** 本批的搜索索引与覆盖索引迁移在开发期间最初写作 `0047` / `0048`，与后来合入主线的 FR-40 备份迁移撞号，已顺延为 **`0049`**（`0049_asset_path_fts.sql`）与 **`0050`**（`0050_asset_size_index.sql`）。
>
> 迁移器只按 `schema_migrations.version` 判重、不校验文件名，因此**若某个库曾用顺延前的分支起过服务**，它的 `schema_migrations` 会记着 `0047`/`0048` 指向搜索迁移。升级到本版本后会出现两类故障：主线的备份迁移被误判「已应用」而跳过（`backup_package.origin` 缺失，运行时失败），以及 `0049`/`0050` 重新执行时报 `table asset_path_fts already exists` / `index idx_asset_repo_size already exists`（启动失败）。
>
> 受影响时按以下方式确认与处理（仅开发/预发可能需要）：
>
> ```sql
> -- 若该库的 0047 / 0048 记录对应的是搜索迁移（而非备份迁移），需先删除这两行
> SELECT version, applied_at FROM schema_migrations WHERE version IN ('0047','0048');
> ```
>
> 确认属于撞号情形后，删除这两行记录并删除已建的 `asset_path_fts`（含其影子表）与 `idx_asset_repo_size`，再重新启动让迁移按新编号执行；**生产环境不存在该问题**（从未发布过顺延前的编号），直接从备份恢复也可以。

### 5.2 备份

**首选做法：生成节点备份包**（FR-132 起）。它把"数据边界"打成一个可校验、可离线搬运的自包含归档：

```bash
jianartifact backup create --mode hot --label "每周例行"
jianartifact backup verify <包标识> --deep     # 可选的深度校验
jianartifact backup drill <包标识> --level deep # 恢复演练：确认这份包真的能恢复
```

包落在 `${JIAN_DATA_DIR}/backups/`，也可从管理台「迁移与搬迁 → 备份与搬迁」生成与下载。热备份不停服，适合例行备份；要求严格一致性时改用冻结窗口。

也可以让服务按周期自动备份（FR-40）：在管理端「迁移与备份 → 备份与搬迁 → 自动备份与保留策略」卡上设置间隔即可（**默认关闭**，需显式开启），并用保留份数 / 天数约束占用（见 §1.1 与下文「备份目录保留」）。

> ⚠️ **开启前务必算一遍容量**。自动备份是**全量**包，每份大小≈整个数据目录。
> 保留策略只是"最多留几份"的上限，**不是"占用量可控"的保证**：
> 例如 20GB 数据 + 保留 7 份 ≈ 140GB，磁盘只有 40GB 时开几天就会写满，而盘满会连带把服务写挂。
> 配置弹窗内会按"最近一次包大小 × 保留份数"实时估算并提示。三项均**保存即生效、无需重启**；首次部署也可经 `JIAN_BACKUP_AUTO_INTERVAL` / `JIAN_BACKUP_KEEP_LAST` / `JIAN_BACKUP_RETENTION_DAYS` 提供初值。

**包内不含密钥**，因此除包之外仍必须另行受保护地保存：生产显式配置的 `JIAN_JWT_SECRET`、`JIAN_MIGRATION_CREDENTIAL_KEY` 与服务环境文件（`deploy/.env`、systemd 环境文件）。这些密钥不随包迁移，新实例必须自行注入。

**备份目录保留**（FR-40）：`${JIAN_DATA_DIR}/backups/` 的自动淘汰只作用于**自动备份**（`origin=auto`，即 `backup-auto` 作业生成的包），两条上限**同时生效、取更严格者**：

| 配置                         | 默认 | `0` 的含义   | 说明                    |
| ---------------------------- | ---- | ------------ | ----------------------- |
| `JIAN_BACKUP_KEEP_LAST`      | `7`  | 不按份数限制 | 保留最近 N 份自动备份   |
| `JIAN_BACKUP_RETENTION_DAYS` | `30` | 不限天数     | 保留最近 N 天的自动备份 |

两条都配 `0` 等于不做任何淘汰（只生成、不清理，需自行盯磁盘）。语义要点：

- **手动备份永不被自动删除**：`jianartifact backup create`、管理台与 API 生成的包都是 `origin=manual`，是人工决策产物，保留策略不触碰它们（不做手动包的自动保留策略）。清单里"淘汰 0 份"因此是正常结果。
- **被增量差包引用为基线的包跳过不删**，并在作业日志中计入"跳过数"。日志形如 `自动备份完成：包 bk-…；保留策略：候选 3 份 / 淘汰 1 份 / 因被差包引用跳过 1 份 / 失败 0 份`——**跳过数非 0 不是故障**，它表示有差包依赖该基线。
- 淘汰是 `backup-auto` 作业的**收尾步骤**，随作业一起跑；首个自动备份生成后立即应用一次，不需要等下一轮。
- 保留策略**只统计状态为 `done` 的自动包**：生成中 / 失败的包不占份数（否则会挤掉一份真正可用的备份），也不被本策略删除。
- 手工清理仍可用 `jianartifact backup delete <包标识>`；被差包引用的基线会被拒绝（`该备份包仍被增量包引用，不能删除`），与保留策略同一约束。

**恢复演练**（FR-40）：对任意已完成的包在**隔离临时目录**内真解包、真校验，产出结构化报告并落库：

```bash
jianartifact backup drill <包标识>                # 浅校验（默认）
jianartifact backup drill <包标识> --level deep   # 深校验：逐 blob 比对内容摘要
```

演练依次检查四项：归档完整性、解包、解出的 SQLite（`PRAGMA quick_check` + 关键表可查询）、解出的 blob 数与 manifest 一致。耗时与包体积同阶（深校验要逐 blob 读一遍），大包请留足时间。**演练是只读路径**：它**不会**产生 `restore.pending` 或 `restore-staging/`，与 §5.3 的真导入有本质区别——演练不改动本实例，不需要重启。演练未通过时命令以非零退出码结束，报告仍会落库（失败报告正是演练最该留下的东西）；报告随包级联删除。临时目录位于 `${JIAN_DATA_DIR}/backup-drill/`，无论成败都会被清理。

若不用备份包而手工兜底，需一致备份：

- `${JIAN_DATA_DIR}/jianartifact.db` 及必要的 `-wal`/`-shm`；
- `${JIAN_DATA_DIR}/blobs`。

建议停写或使用一致性快照；定期在非生产目录做恢复演练并抽样校验制品。

### 5.3 从节点备份包导入（恢复 / 搬迁，FR-137）

把一份节点备份包应用到本实例：校验 → 合并 blob → 暂存 db → 写待生效标记 → **重启替换**。导入不修改运行中的 SQLite，只写 `restore-staging/` 与 `restore.pending`；下次启动由 `ApplyPendingRestore` 在 `persistence.Open` 之前先做 `pre-restore-<ts>/` 回滚备份、再原子替换数据库。

**四条通道**（前三条要求包来自外部，第四条用本机已有的包）：

- **本机已有的包**：管理端「备份与搬迁」事件表 → 某行「⋯」→ **「用这个包恢复」**（`POST /api/v1/backups/{id}/restore`）。补的是「页面上明明列着可用的包，却没有任何按钮能用它恢复」这个缺口——实测反馈的「只能备份无法恢复」正源于此。语义与其它通道完全一致（同一条编排、同样需重启生效）。
- **CLI 直传**：`jianartifact backup import <归档路径> [--overwrite] [--deep] [--yes]`（见 §4）。适合已把包传到本机的场景。
- **Web URL 拉取**：`POST /api/v1/backups/import` 由服务端从 `sourceUrl`（http/https，必填）拉取并异步导入（202 + 记录），进度经 `GET /api/v1/backups/imports`（分页，最近优先）与 `GET /api/v1/backups/imports/{id}` 查看。仅管理员。**SSRF 防护**：拒绝回环/私网/链路本地/云元数据地址，每次拨号重新解析（防 DNS 重绑定），重定向重新校验。
- **Web 分片上传（FR-137 第三通道）**：前端把 GB 级备份包按服务端约定的 **8 MiB** 分片顺序上传，落盘后组装并交本地导入状态机（仍需重启生效）。状态机 `initialized → receiving → completed`，随时可 `aborted`；会话有效期 **24 小时**，单包上限 **50 GiB**（与 URL 拉取同一护栏）。流程：
  - `POST /api/v1/backups/uploads`（`{fileName, totalBytes, sha256?}`）发起会话，返回 `uploadId`/`chunkSize`/`uploadedChunks`/`status`/`expiresAt`；**分片大小以 init 返回的 `chunkSize` 为准，前端不要硬编码**。
  - `PUT /api/v1/backups/uploads/{id}/chunks/{index}`：上传单个分片，请求体为原始字节（`application/octet-stream`）；分片须 `> 0` 且 `<= chunkSize`，序号越界 / 单片超额 / 空体统一 400。
  - `GET /api/v1/backups/uploads/{id}`：查询会话（**供续传**）。`uploadedChunks` 由**磁盘上真实存在的分片**推导（以磁盘为准，缺哪片补哪片、重复片幂等覆盖），故只要磁盘分片还在就能断点续传，不必只信数据库记录。
  - `POST /api/v1/backups/uploads/{id}/complete`（`{sha256?, overwrite?, deep?}`）拼装并交导入（202）；**缺片会带上缺失序号**；边拼装边算 sha256，与声明不符删除半成品，再交导入状态机（仍需重启生效）；`overwrite`/`deep` 已支持。
  - `POST /api/v1/backups/uploads/{id}/abort`（204）取消并清理磁盘；**数据库记录保留**（状态 `aborted`，供审计），只删磁盘目录，故 `GET` 返回 200 + `aborted` 而非 404。

校验与规模护栏（写盘前）：包格式版本可识别、`dbSchemaVersion` 不高于本程序、db sha256 与 blob 抽样 sha256；声明规模超上限（快照 8 GiB / blob 总量 50 GiB / blob 条目 500 万）即中止（防解压炸弹）；目标非空且未 `--overwrite` 拒绝（内置 `anonymous` 主体不计入"非空"）；增量差包导入当前不支持。

状态机 `queued → fetching → staging → pending_restart → done`，失败转 `failed` 并带 `error_code`：`fetch_failed` / `package_oversize` / `sha256_mismatch` / `manifest_invalid` / `target_not_empty`（409）/ `incompatible` / `restore_pending`（409）/ `internal`。

失败处置：任何阶段失败都**不留下待生效标记、不写 `restore.pending`、不留暂存目录**，记录以 `failed` 终态表达错误码；运维按错误码排查——`target_not_empty` 需加 `--overwrite` 或先重启已有 `restore_pending` 的实例；`incompatible` 需先升级本程序；`fetch_failed` 检查来源可达性与 SSRF 策略；`sha256_mismatch` 核对 `expectedSha256` 与来源完整性；`restore_pending` 表示已存在待生效恢复，先重启使其生效或清掉标记。重启后启动日志会打印已应用的恢复，并保留 `pre-restore-<ts>/` 作为回滚退路。

**待生效恢复期间的行为**（`restore.pending` 存在时）：后续导入/上传一律以 **409 `restore_pending`** 拒绝，**不允许叠加第二次恢复**——这是刻意的，避免两次替换互相覆盖。界面会据此把两个导入按钮转橙并可点开说明（含可复制的重启命令），因为它此前只体现在导入记录行的 `pending_restart` 徽章上，徽章滚出视野后就再也看不出"为什么导入一直被拒"。查询该状态的端点：`GET /api/v1/backups/restore-status`（仅管理员，返回 `{pending, packageId?, origin?, stagedAt?}`）。

**标记读不了时服务仍能启动**（实测事故的修复）：若 `restore.pending` 存在但当前进程读不了它（典型成因是用 `sudo` 跑过 CLI 导入、标记属主成了 root），服务**跳过该恢复、打印含 `chown` 修复命令的告警，并正常起服**；标记与暂存目录原样保留，改完属主重启即可完成替换。早期实现把读取失败当致命错误，结果是启动失败 → systemd 重启 → 仍读不了 → 再失败的重启循环，现场只看到全站 502 而看不出真正该做的事。取舍是有意的：待生效恢复是"稍后要应用的一次维护动作"，不是服务提供读服务的前提，不该用可用性去换一个可延后动作。

## 6. 回滚

- 二进制/镜像回滚到上一个已知良好版本；systemd 回切 `current`，Compose 回切镜像 tag。
- 若新版本执行了不可逆 schema 迁移，必须同时恢复对应数据备份，不能只回滚代码。
- 搬迁后回滚：导入时留下的 `pre-restore-<ts>/` 是数据库回滚点；未消费的 `restore.pending` 可在重启前删除以放弃本次恢复。切 DNS 前的旧实例保持冻结即可随时切回，一旦解冻并恢复写入就不再是可回滚的一致点。

## 7. CI、发布与排障

### 7.1 质量门与发布

本地质量入口是 Windows 原生 `make check` / `scripts/check.ps1`；应覆盖前端格式、类型、lint、测试、构建、Go vet/lint/race/vuln、契约和静态构建。已发布版本以 `CHANGELOG.md` 与远程 tag 为准（本文写作时最新为 `0.10.1`）；下一个版本在版本提交、tag、远程 CI 和 Release 完成前都不得标正式交付。

开发预览和正式发布使用不同版本标识；发布资产必须有对应校验和。交叉编译成功不能代替原生运行或真实服务验收。

### 7.2 常见问题

- **启动失败**：检查数据目录权限、SQLite 路径、blob 目录、TLS 证书和必要密钥；不要打印密钥值。
- **`/readyz` 失败**：检查 SQLite 是否可打开、blob 目录是否可写、磁盘空间和服务用户权限。
- **业务写入返回 503 `write_frozen`**：实例处于写入冻结窗口（搬迁切换中）。查询 `GET /api/v1/maintenance/freeze` 看 `until`，确认搬迁完成后 `DELETE` 解冻，或等待到期自动解冻。冻结期间读方法、登录、维护命名空间与备份导入/上传放行。
- **备份包导入未生效**：导入只写暂存与 `restore.pending`，**必须重启服务**才替换数据库；重启前可查看导入记录状态（`pending_restart` 即已就绪）。
- **备份包导入被拒**：按 `error_code` 排查——`target_not_empty` 需 `--overwrite`，`incompatible` 需先升级本程序，`sha256_mismatch` 核对包完整性，`fetch_failed` 检查来源可达性与 SSRF 策略，`restore_pending` 表示已有待生效恢复。
- **迁移卡住**：检查任务状态、报告和密钥是否保持不变，使用显式恢复，不要把来源凭据或内部地址复制到工单。

排障输出只保留稳定错误码、阶段、脱敏原因、恢复提示和必要时间/序号；禁止粘贴令牌、Authorization、环境变量值、内部地址或系统路径。
