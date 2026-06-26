# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增
- 主机 / 系统监控采集（FR-98，ADR-0023）：新增**仅 Admin** 端点 `GET /api/v1/monitor/host`，经新增依赖 `sysinfo`（裁 features 仅 `system` + `disk`）跨平台采集**这台主机**的基础资源画像——CPU 全局使用率 + 逻辑核数、内存 / 交换的已用 / 总量、磁盘逐盘挂载点 / 总量 / 可用及汇总、系统 uptime。**按请求单次采样**（共享 `sysinfo::System` 经 `Mutex` 串行刷新、磁盘按请求刷新），不后台轮询、不落库、不留历史时序；纯本机内部数据、**绝不外发、不向外部遥测 phone-home**（守 ADR-0009 / 0015 基调），GET 读取类不入审计。新增 `monitor` 模块承载采集（「sysinfo 读数 → DTO」纯映射抽为无副作用纯函数）；`cpu.usage_percent` 首样可能为 0（CPU 使用率需两次采样间隔）属已知取舍
- 设置可编辑与运行时热替换（FR-88，ADR-0022，取代 ADR-0020 的「代理只读 / 运行时不热替换」取向）：网络代理 `[network.proxy]` 与在线更新可调字段（`enabled` / `repo` / `api_base_url` / `restart_mode` / `token`）改为经控制台「设置」页或 `PATCH /api/v1/settings`（仅 Admin）在线编辑、**即时生效、无须重启**。新增随 `AppState` 共享的出站网络热替换槽 `config::NetworkState`（std `RwLock<Arc<NetworkSnapshot>>`，快照含代理配置 + 据其构造的 `reqwest::Client`）：5 处出站点（proxy 回源 / Nexus 迁移 / 漏洞库镜像 / OIDC / 在线更新）不再持启动期 client，改持 `Arc<NetworkState>`、每次出站取当前 client；PATCH 改代理后锁外重建 client、原子换槽，下个出站请求即用新代理。设置页从只读改可编辑（代理 http/https/no_proxy 表单 + 在线更新 enabled/repo/api_base_url/restart_mode/token），保存调 `PATCH`。校验失败 400 且不改现有生效值；代理凭据与 token 只入内存槽、**不写回 TOML / 不入 DB / 不进日志 / 不回显**，重启回落文件 + env 配置
- 审计日志全面补齐（FR-97，增强 FR-31，扩展 ADR-0015）：审计中间件覆盖从"精选事件"扩为"**全量非读**"——**所有变更类请求（POST/PUT/PATCH/DELETE）一律产一条审计**，已知端点归精确语义 `action`（用户 / Token / 仓库 / ACL / 组管理、制品上传 / 删除，及新覆盖的设置 PATCH `settings.update`、防护配置 PATCH `protection.config.update`、迁移控制 `migrate.job.control` / 迁移搬运 `migrate.run`、在线更新 `update.apply`、登出 / 刷新 `auth.logout` / `auth.refresh`、组 / 组成员 / 组 ACL 增删等），未显式归类的非读路径走兜底 `change.{method}`（新增端点不漏记）；**读取类（GET/HEAD，含下载 / 浏览 / 搜索 / 详情）一律不入审计**（交使用分析计数，避免刷屏与性能损耗）；登录仍由登录 handler 显式发 `login`。保留既有异步有界 channel 投递 + 批量落库 + 保留期轮转；主路径只做一次非阻塞 enqueue，采集 / 写入失败仅 WARN、不影响业务。密码 / Token / JWT / 上游 / 代理凭据绝不进审计（`actor` 只记用户名，中间件不读请求体与 `Authorization` 值本体）
- Nexus 迁移任务控制（FR-91，增强 FR-83）：在线拉取异步任务新增取消 + 暂停 / 继续——新增 `POST /api/v1/migrate/jobs/{id}/cancel`、`/pause`、`/resume`（均仅 Admin）；后台逐 asset 循环在 asset 边界响应信号：取消即停止后续搬运、任务标 `cancelled`（不算失败、已搬运保留），暂停即挂起不推进、继续即恢复；进度快照新增 `paused` 布尔、阶段枚举新增 `paused` / `cancelled`；对已结束任务的控制为幂等空操作（200），未知 id 返 404。用进程内 `AtomicBool` + `tokio::sync::Notify` 实现（不引入新依赖）。Web 迁移页进度面板新增「取消 / 暂停 / 继续」按钮，按任务态启停
- 首启自动生成默认 `config.toml`（FR-90）：二进制启动时若配置文件路径（默认 `./config.toml`，或 `--config` 指定）不存在，自动写一份带中文注释的默认配置到该路径并记 INFO 日志（已存在则绝不覆盖），运维拿到单二进制后开箱即有可编辑配置——解决「config 不释放、想开启在线更新 / 改代理却无处下手」的痛点。默认模板编译期嵌入仓库已维护的 `config.example.toml`（保真带注释）；写入失败只记 WARN、不阻断启动（回落默认值加载）
- 在线更新 prerelease / 测试通道（FR-89，增强 FR-85，扩展 ADR-0021）：在线更新新增 `[update] channel`（`stable` | `prerelease`，默认 `stable`，env `JIANARTIFACT_UPDATE_CHANNEL`），同样经控制台「设置」页 / `PATCH /api/v1/settings`（仅 Admin）在线编辑、即时生效、无须重启。`stable`（默认）维持现状只查 `/releases/latest`、只认稳定版；`prerelease` 查 `/releases` 列表、取最新一条非草稿 release（含预发布），用于真机 / 灰度拉取预发布版。是否升级仍按版本号比较（仅当目标版本更高才升级）。设置页在线更新区新增「更新通道」选择项
- 控制台折叠图标导航条 + 信息密度基线（FR-92，UX 重构 epic 地基，纯前端）：侧栏由固定宽度图标+文字列表改为**可折叠图标导航条**——默认窄（仅图标，经 Tooltip + `aria-label` 保证窄态读屏 / 键盘可用），导航顶部切换控件可展开为图标+文字、再收回；管理类入口（用户管理 / 用户组管理 / 使用分析 / 防护配置 / 审计日志 / 防护监控 / Nexus 迁移 / 设置）仅 Admin 可见（沿用 `useAuth().isAdmin`），active 高亮维持按路径段精确匹配（不串台）。新增 `theme/density.ts` 集中信息密度基线 token（导航宽度 / 内容区 padding 收紧 / 卡片瘦身 / 栅格 gap），落地到 shell 外壳与仪表盘页作示范（其余页面密度细化交后续 UX 重构 FR）。页眉为后续全局搜索（FR-94）留禁用占位框（不接逻辑）；未新增前端依赖
- 控制台统一监控页（FR-99，UX 重构 epic，纯前端）：新建 `/monitor` 统一「监控」页，顶部 `Tabs` 切换四区——**主机监控**（新，消费 FR-98 `GET /api/v1/monitor/host`，按请求采样展示 CPU / 内存 / 磁盘占用环形 + 逐盘明细 + uptime，提供手动刷新）、**使用分析**（整合 FR-58）、**审计**（整合 FR-77）、**防护**（整合 FR-78）。三个被整合视图**复用既有页组件**（`AnalyticsPage` / `AuditPage` / `ProtectionMonitorPage` 作为 tab 面板挂载、数据层零改动、既有测试不回归），各面板按需挂载切到才拉数据。导航把原「使用分析 / 审计日志 / 防护监控」三个独立入口**收敛为单一「监控」入口**（仅 Admin），保持 FR-92 折叠 / 角色门控 / 段精确高亮不回归。图表（CPU/内存/磁盘占用环形 `RingChart`、热门项条形 `BarList`）**手搓零依赖 SVG/CSS**，不引图表库。前端 `api/{types,endpoints}` 加 `HostMetrics` 类型与 `getHostMonitor`（对齐后端 `src/monitor` DTO）。纯本机内部数据、不外发；未新增前端依赖
- 控制台仓库浏览重构为 Nexus-like 文件树 + 右侧详情（FR-93，增强 FR-66/68/75/76，纯前端、不改后端）：仓库详情页把原「制品浏览 / 文件浏览」两个并列表格 Tab 合并为单一「浏览」视图——左侧可逐级展开的**文件树**（仓库根 / 文件叶子按格式显示专属 icon：maven/npm/docker/pypi/cargo/go/nuget/raw），点文件在右侧加载**制品详情面板**（元数据 / 四校验和 / 后端使用方式片段，复用既有 `listArtifacts` 索引与 `getArtifactDetail` 端点）。详情面板新增**多格式依赖坐标**下拉（前端 JS 模板生成，仅对能反解出 GAV 的 Maven 主构件产出全套：Apache Maven / Gradle Groovy DSL / Gradle Kotlin DSL / Scala SBT / Apache Ivy / Groovy Grape / Leiningen / PURL，逐项可复制；GAV 反解规则与后端 `Gav::from_path` 对齐，非 Maven / 无法反解者不渲染坐标区）、**HTML View 外链**（指向 FR-75 的 HTML 仓库索引视图，即制品所在目录加尾斜杠的索引 URL）与**下载按钮**。私有 / 无权场景沿用既有端点过滤（不泄露存在性）；制品详情独立深链页（`/artifact`）复用同一详情面板组件；未新增前端依赖
- 控制台设置页信息密度 + 页内 tab 重排（FR-96，UX 重构 epic，纯前端）：设置页由卡片自上而下纵向铺开改为**左侧页内 tab 导航 + 右侧高密度可编辑表单**——三个页内 tab：网络代理（http/https/no_proxy 紧凑表单）/ 在线更新（enabled/repo/api_base_url/restart_mode/channel + token，含检查 / 升级入口与二次确认流）/ 关于·版本（当前版本 + 「保存后运行时即时生效、无须重启」说明）。复用 `theme/density.ts`（卡片瘦身 `cardPadding` + 堆叠间距 `gridSpacing`）提升密度；panel 用 `keepMounted` 保表单态不随切换丢失。仅重排呈现：沿用 FR-88/89 既有数据加载 / 保存逻辑与代理凭据 / token 脱敏，不改 `GET` / `PATCH /api/v1/settings` 契约；未新增前端依赖

### 变更
- 控制台设置页（FR-87 → FR-88）：`GET /api/v1/settings` 改为读**运行时可编辑设置热替换槽当前值**（含运行时 PATCH 在内，原读 `state.config`）；前端「设置」页从只读展示改为可编辑表单 + 保存按钮，文案由「真源为 config.toml / 环境变量、运行时不可改」改为「保存后运行时即时生效、无须重启」

### 修复
- 控制台侧栏导航高亮串台：导航 active 判定由前缀匹配改为按路径段精确匹配，修复进入「防护监控」（`/protection-monitor`）时「防护配置」（`/protection`，前者前缀）被一并高亮的问题；其它有前缀关系的路由同样不再串台，仓库等子路径仍正确高亮
- 发布流水线 dev 预发布资产堆积（FR-86）：滚动 `dev` 开发版快照的资产名内嵌 `{run_number}+{sha}`、各次不同，原先 `action-gh-release` 只追加不删旧资产致其在 `dev` 预发布里无限堆积。发布前显式删除旧 `dev` 预发布及其 tag（仅 prerelease 渠道，正式 `v*` 不删），使每次推 master 的 `dev` 预发布只含当前快照资产

### 移除
- 无

### 安全
- 无

## [0.4.0] - 2026-06-26

### 新增
- Nexus 在线拉取制品迁移（FR-82，补齐 ADR-0006 在线入口的「搬运」）：新增 `POST /api/v1/migrate/nexus/online/migrate`（仅 Admin），源 Nexus 在线时经其 `service/rest/v1/components`（`continuationToken` 分页）枚举所选 **Maven hosted** 仓库的全部 asset，按各 asset `downloadUrl` HTTP 流式下载、经既有制品机理落为本系统 hosted 制品——**无需离线 blob store 目录**，补足远程 Nexus（无磁盘访问）的迁移路径。落定后比对源报告 sha256 保证文件字节一致（`.sha1`/`.md5`/`.sha256`/`.sha512` sidecar 作为独立 asset 一并搬运），目标仓库名可自定义（默认同源名）；下载 / 写入瞬时失败（网络中断 / 流式解码失败）自动重试、指数退避（确定性失败不重试），单 asset 失败记录跳过、不中断整批、可重入；仅 `maven2` hosted 参与、其余整体跳过。Web 迁移页新增「在线拉取 / 离线目录」方式选择与每仓库目标改名
- 迁移任务异步化与进度可观测（FR-83，ADR-0019）：在线拉取迁移改**进程内异步任务**——`POST /api/v1/migrate/nexus/online/migrate` 同步只做枚举源 + 匹配选仓 + 解析凭据后**立即返回 `job_id`（202）**，asset 枚举（先枚举全量得知总数）+ HTTP 下载 + 落地在后台任务跑、边搬边上报进度；新增 `GET /api/v1/migrate/jobs/{id}`（单任务进度：阶段 / 总数 / 已迁 / 已跳过 / 当前仓库与文件 / 各仓库结果 / 错误）与 `GET /api/v1/migrate/jobs`（任务列表，供重连），均仅 Admin。任务为进程内有界注册表（**不落库**，服务器重启即丢失、靠迁移幂等重跑恢复，保留 ADR-0006「无须持久化迁移任务表」）。Web 迁移页在线执行改异步轮询，展示导入队列进度条与当前文件，支持客户端断开后经本地存档的 `job_id` 重连续看
- 统一出站网络代理（FR-84，ADR-0020）：新增 `[network.proxy]` 配置（`http` / `https` / `no_proxy`，环境变量前缀 `JIANARTIFACT_NETWORK_PROXY_*`）作为出站正向代理的唯一真源，`config` 层抽共享出站客户端 helper `build_outbound_client` 统一注入全部出站 reqwest 客户端（proxy 回源 / Nexus 在线迁移 / 漏洞库镜像 / OIDC），保留既有 rustls / stream 特性。配置给值即为真源（压过系统代理环境变量）、三键全空时不注入保持现状（仍 honor 系统 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`）；代理 URL 凭据不入库、不进日志 / 错误信息
- GitHub Actions CI/CD 发布流水线（FR-86）：新增质量门工作流（push/PR 到 `master` → 前端构建 + `cargo fmt`/`clippy`/`test`，依赖审计独立非阻断）与发布工作流（push `master` → 滚动 prerelease 开发版快照；push tag `v*` → 正式 Release）；三目标原生编译（Linux x86_64 / Windows x86_64 / macOS arm64），每资产产出 `jianartifact-{version}-{target}{ext}` 及配套 `.sha256`，命名契约与 FR-85 在线自更新下载约定对齐
- 在线更新（FR-85，ADR-0021）：管理员手动触发的完整自更新——新增 `GET /api/v1/update/check`（查 GitHub 最新稳定 Release 并与当前版本比对）与 `POST /api/v1/update/apply`（按本机平台下载对应资产、流式校验 sha256、原子替换运行中的二进制并触发自动重启），均仅 Admin。校验失败（sha256 不一致 / 缺资产）即拒绝替换、保留旧二进制；新增 `[update]` 配置（`enabled` / `repo` / `api_base_url` / `restart_mode` / `download_timeout_secs`，可选 token 真源 env `JIANARTIFACT_UPDATE_TOKEN`），**出站默认关闭**（`enabled=false` 时两端点拒绝、不联网），出站经 `[network.proxy]`（FR-84）注入的代理
- 控制台设置页（FR-87）：新增**仅 Admin** 只读聚合端点 `GET /api/v1/settings`（脱敏返回网络代理 + 在线更新配置与当前版本——代理 URL 去 `user:pass@` 凭据、更新 token 仅以 `has_token` 暴露，绝不回显凭据），与前端「设置」页（仅 Admin 可达，侧栏入口）：只读展示网络代理（http/https/no_proxy）与在线更新（状态 / 仓库源 / 当前版本）配置并标注「真源为 config.toml / 环境变量、运行时不可改」，提供「检查更新」（消费 `GET /api/v1/update/check` 展示版本对比）与有更新时「升级到 vX.Y.Z」（二次确认后调 `POST /api/v1/update/apply`、成功进入「正在重启」提示态），`enabled=false` 展示「未启用」并禁用检查按钮，各错误码（409/502/422/400）友好提示

### 变更
- 无

### 修复
- 无

### 移除
- 无

### 安全
- 无

## [0.3.0] - 2026-06-25

### 新增
- 通用制品上传 API（FR-73）：新增 `POST /api/v1/repositories/{id}/upload`（multipart/form-data），向 hosted 仓库统一直传 Maven / npm / Raw 三格式制品——Maven 按表单 GAV 拼坐标、npm 按表单 name + 上传文件名定位 tarball（不解包 .tgz）、Raw 用表单 path；复用既有写授权与流式落 blob 机理，proxy 仓库与不支持格式拒绝（400），覆盖语义沿用各格式策略（release/已发布 409），超上传上限返回 413
- Web 控制台制品上传页面（FR-74）：新增“制品上传”页与导航入口，选 hosted 仓库后按格式渲染动态表单（Maven: GAV / npm: name+version / Raw: path），选文件后带进度条上传，成功/失败有提示
- 目录列表 API 与 HTML 仓库索引视图（FR-75）：以路径尾斜杠作为目录请求信号，按 `Accept` 头返回 JSON 目录项或类 Apache 的 HTML 索引页；仅通用格式参与，私有仓库对匿名 / 无权一律 404 不泄露存在性，结果按读权限过滤。
- Web 控制台文件浏览器（FR-76）：仓库详情页新增「文件浏览」标签，按目录树逐级浏览、面包屑导航，点目录进入下一层、点文件跳制品详情。
- 审计日志查询页面（FR-77，ADR-0015）：控制台新增「审计日志」页（仅管理员可见），对接已有 `GET /api/v1/audit` 端点，以分页表格按时间倒序展示写 / 管理 / 授权拒绝类审计事件，支持按操作者 / 动作 / 仓库过滤，点击任意行查看包含请求 ID / 来源 IP / 对象 / 补充字段的详情弹窗；纯前端只读页面，不改后端
- 防护状态监控页面（FR-78，ADR-0017）：控制台新增「防护监控」页（仅 Admin 可见，路由 `/protection-monitor`），消费既有 `GET /api/v1/protection/status` 与 `GET /api/v1/protection/alerts`，展示七层防护各维度（限流 / 自动封禁 / CC 挑战 / WAF 阻断 / 慢速攻击）窗内计数快照、当前封禁 IP 数、告警评估开关与评估窗口，并以表格列出告警历史（时间 / 维度 / 严重度 / 观测值 / 阈值 / 详情）；以 5 秒定时轮询刷新快照实现「实时」（无需 websocket）。纯展示本机内部聚合数据，不接任何外部遥测 / 导出
- 防护配置 API（FR-79，扩展 ADR-0008，新增 ADR-0018）：新增 `GET /api/v1/protection/config` 与 `PATCH /api/v1/protection/config`（仅 Admin），Admin 可在线读取 / 整体替换七层防护各维度配置（限流阈值 / 并发上限、IP 黑白名单、异常封禁、慢速攻击、CC 挑战、WAF 规则、监控告警），**校验通过即时生效、无须重启**；运行时配置经新增的进程内热替换槽（`RwLock<Arc<ProtectionSnapshot>>`，std 实现、不引入外部依赖）承载，PATCH 后锁外按新配置重建派生态（IP 名单匹配器、WAF 规则集），下一个请求即按新值判定，限流计数 / 封禁登记 / 告警去抖等运行态不清零；非法配置（如某时间窗为 0、CC 难度超 64 位）返回 400 且不改变现有生效配置；防护配置无密码 / Token / 凭据等敏感项，整体回显不泄露。运行时改动为进程内热替换、不写回 TOML，重启回落文件配置（配置真源仍是文件 + 环境变量）
- 防护配置管理页面（FR-80）：控制台新增「防护配置」页（仅 Admin 可见），把七个防护维度拆为分区表单（启停开关 + 阈值 / 难度 / 名单 / WAF 规则编辑），保存即调 `PATCH /api/v1/protection/config` 整体回传、即时生效并回显；后端 400 校验错误在页面展示中文文案
- Nexus 迁移管理页面（FR-81，对接 ADR-0006 已有迁移端点，仅 Admin）：控制台新增「Nexus 迁移」页（仅管理员可见），以多步流程引导迁移——① 选迁移形态（在线 REST / 离线 blob store）并填源（在线填源 Nexus 地址 + 可选凭据引用；离线填本地 blob store 路径）→ 预览可迁移仓库列表（不搬运）；② 勾选要搬运的仓库并填离线 blob store 路径（制品本体来源）→ 执行 proxy / hosted 搬运；③ 展示迁移报告（各仓库新建标记、已迁 / 跳过制品数、整仓跳过列表）。源 Nexus 凭据仅以引用名（auth_ref）输入、用口令型输入框承载，明文不入库、不回显、不持久化

### 变更
- 无

### 修复
- 通用上传 Maven 制品补齐校验和 sidecar（FR-73）：经 `POST /api/v1/repositories/{id}/upload` 向 Maven 仓库上传主构件后，服务端自动生成 `.sha1` / `.md5` / `.sha256` / `.sha512` 四个 sidecar（内容为对应摘要的小写十六进制）。此前服务端上传无客户端逐文件 PUT 的 sidecar，导致 `mvn` 下载时校验和文件 404；补齐后产出制品与 `mvn deploy` 一致、可被官方客户端独立校验。仅 Maven 生成，npm / Raw 无此约定不受影响

### 移除
- 无

### 安全
- 无

## [0.2.0] - 2026-06-25

### 新增
- Go 模块格式（hosted + proxy）经统一 Format trait 注册接入通用机理：按 GOPROXY 协议暴露 `@v/list` / `.info` / `.mod` / `.zip` / `@latest`，模块路径大小写 bang 编码（`!x` ↔ `X`），版本不可变（重复上传同版本 409），多校验和与流式存取；hosted 据已存版本聚合 `@v/list` 与 `@latest`、`.info` 缺失时按 `.mod` 合成，proxy 对 `.mod`/`.zip`/`.info` 走 cache-miss 单飞缓存、对 `@v/list`/`@latest` 回源透传；授权复用既有编排（上传需 write、private 对无权一律 404）
- Cargo 格式（hosted+proxy，FR-26）：按 Cargo 稀疏索引协议接入，支持 `cargo publish` 发布、稀疏索引与 `.crate` 下载、yank/unyank、registry config.json；同版本不可覆盖（409）、索引 cksum 用 sha256；proxy 回源上游索引（不缓存）并缓存 `.crate`（cache-miss→hit）；发布/yank 需写权限、private 对无权一律 404
- PyPI 格式（FR-27，hosted + proxy）：Simple Repository API（PEP503 HTML / PEP691 JSON）项目与文件索引、twine multipart 上传、pip 下载；hosted 已发布文件不可覆盖（409），proxy 回源上游 Simple 并重写文件链接、包文件单飞缓存（cache-miss → hit 不重复回源）
- NuGet 格式（hosted + proxy）经统一 Format trait 接入：NuGet v3 服务索引、扁平容器版本列表与 .nupkg / .nuspec 存取、`nuget push`（multipart 解析 .nupkg 内嵌 .nuspec 取 id/version）、已发布版本不可覆盖（重复 push 同版本 409）、四校验和、id/version 小写规范化；proxy 回源服务索引重写指向本仓库、版本列表回源、.nupkg cache-miss 缓存；支持 `dotnet nuget push` / `dotnet add package`
- S3 兼容对象存储后端（FR-30，可选 opt-in，默认关闭）：新增 Cargo 特性 `s3` 与 `[data.storage]` 配置节（`backend = "fs"`（默认）/`"s3"` + endpoint/region/bucket/prefix/path_style）；启用 `s3` 特性并配置 `backend = "s3"` 后 blob 本体改存对象存储，写入语义与本地等价（本地临时文件算 sha256 → 内容寻址 key 流式 multipart 上传，失败清理不留孤儿对象），下载流式 GET 不整体载入内存；本地文件系统仍为默认后端，默认构建不含任何 S3 代码与依赖、保持单一二进制零外部运行时依赖；客户端 aws-sdk-s3 裁为纯 rustls + ring（不引入 aws-lc-rs）。详见 ADR-0014 与 docs/OPERATIONS.md「启用即引入外部依赖」
- 审计日志（FR-31，ADR-0015）：新增 `audit_log` 表，经审计中间件采集精选的写 / 管理 / 授权拒绝事件（登录、Token 与用户管理、仓库与 ACL 变更、制品上传 / 删除），普通匿名读取不入审计；事件经进程内有界 channel 异步批量落 SQLite，主路径只做非阻塞投递、采集失败不影响业务、队列满则丢弃 + 计数 + WARN；后台任务按保留天数（`observability.audit.retention_days`，默认 90）与行数上限（`observability.audit.max_rows`，默认 100 万）轮转；新增 `GET /api/v1/audit` 仅 Admin 分页查询；密码 / Token / JWT / 上游凭据一律不入审计
- Nexus OSS 迁移在线 REST API 入口（FR-36）：新增 `migrate` 模块与 `POST /api/v1/migrate/nexus/preview` 端点（仅管理员），连接在线 Nexus 并经其 `service/rest/v1/repositories` 枚举可迁移仓库列表与基本元数据（名 / 格式 / 类型 / proxy 上游地址），作为迁移的发现 / 预览步骤；REST 交互经 `NexusClient` trait 抽象、生产实现复用 reqwest 纯 rustls，访问凭据真源环境变量（`JIANARTIFACT_MIGRATE_<NAME>_USERNAME/PASSWORD`，DB 仅存引用、不入库不进日志）；连接 / 鉴权 / 解析失败映射为 502，不泄露源系统内部细节。仅做发现 / 预览，不搬运制品
- Nexus OSS 迁移离线 blob store 入口（FR-37）：新增 `POST /api/v1/migrate/nexus/offline/preview` 端点（仅管理员），当源 Nexus 已下线、只剩其文件型 blob store 目录时，从给定本地目录解析磁盘布局（`content/` 分片目录 + 每个 blob 一份 `.properties` 元数据），按所属仓库枚举可迁移 blob 及基本元数据（坐标 / sha1 / 大小），作为离线迁移的发现 / 预览步骤；软删 / 损坏 / 缺必要字段的元数据容错跳过、不中断整次枚举，路径不存在 / 缺 `content/` 目录映射为 400；阻塞文件 IO 经 `spawn_blocking` 不阻塞异步运行时。仅解析 `.properties`、不读取也不搬运 blob 本体
- Nexus 迁移 proxy 仓库配置 + 缓存制品搬运（FR-38，ADR-0006）：新增 `POST /api/v1/migrate/nexus/proxy/migrate` 端点（仅管理员），把源 Nexus 的 proxy 类型仓库搬到本系统——据在线 REST 枚举的 proxy 仓库配置（映射 Nexus 格式名 → 本系统已实现格式，如 `maven2`→`maven`；同名仓库复用、未实现格式或缺上游地址整体跳过）在本系统建仓，再从离线 blob store 按仓库名取该仓库的缓存制品本体（成对 `.properties` + `.bytes`，缺本体跳过），经既有 `ArtifactService::ingest_cached` 流式写入缓存（blob 先落盘并校验 sha256 再写元数据索引并标记 `cached`，写索引失败回滚不留孤儿，不整体载入内存）；搬运幂等可重入（同坐标同内容跳过），单制品失败（路径非法 / 读本体失败 / 写入失败）记录跳过、不中断整批，无须持久化迁移任务表；迁移不搬运源系统上游凭据（凭据真源 env / 配置）。仅迁移 proxy 仓库，hosted 仓库制品完整搬运（FR-39）尚未实现
- Nexus 迁移 hosted 仓库配置 + 完整制品搬运（FR-39，ADR-0006）：新增 `POST /api/v1/migrate/nexus/hosted/migrate` 端点（仅管理员），把源 Nexus 的 hosted 类型仓库完整搬到本系统——据在线 REST 枚举的 hosted 仓库配置（映射 Nexus 格式名 → 本系统已实现格式；同名仓库复用、未实现格式整体跳过）在本系统建 hosted 仓库，再从离线 blob store 按仓库名取该仓库的全部制品本体，经新增 `ArtifactService::ingest_hosted` 流式写入（blob 先落盘并校验 sha256 再写元数据索引，落为正常 hosted 制品 `cached=false`，写索引失败回滚不留孤儿，不整体载入内存）；按各格式覆盖 / 不可变策略处理重复（同坐标不同内容且不可覆盖如 Maven release 跳过、可覆盖则落定新内容），超 `limits.max_artifact_size` 的制品跳过不留半截 blob；搬运幂等可重入（同坐标同内容跳过），单制品失败记录跳过、不中断整批。proxy 与 hosted 搬运共用格式映射与 blob 归一化逻辑。至此 Nexus 迁移框架（FR-36/37/38/39）完整
- 漏洞库离线镜像（FR-70，ADR-0012）：新增 `vuln` 模块，按配置周期把公开漏洞数据集（OSV，按生态 `all.zip`）整体镜像下载到本机，流式落盘后解压、逐条解析公告并经 `meta` 幂等落库（公告表 + 受影响坐标表 + 刷新状态表）；下载只携带公开生态名、不外发本机制品坐标。新增 `[vuln]` 配置（默认关闭，含数据源、生态列表、刷新周期）。本批仅镜像/落库，制品坐标匹配标记（FR-71）尚未实现
- 访问 / 下载统计采集（FR-57，ADR-0009）：新增 `usage_stats` 聚合计数表与可选 `usage_events` 明细表；制品 GET 下载记 `download`、详情查看记 `access`，事件经进程内有界 channel 异步批量聚合落 SQLite（UPSERT 累加、并发下计数准确），主路径只做非阻塞采集、采集失败不影响业务、队列满则丢弃 + 计数 + WARN；明细默认关闭，开启后按行数上限兜底裁剪。新增 `[observability.usage]` 配置（`detail_enabled` 默认关闭、`max_detail_rows` 默认 100 万）。统计数据本机内部、默认不外发、不向外部遥测 phone-home，不提供外部导出。本批仅采集 / 落库并提供内部聚合查询入口，数据面板展示（FR-58）见下条
- 使用分析数据面板（FR-58，ADR-0009）：新增 `GET /api/v1/analytics/usage`（仅 Admin）聚合查询端点，薄 handler 消费 FR-57 采集的本机 `usage_stats`，返回访问 / 下载总量、热门制品（按下载量倒序，前 N 条）与仓库用量（按下载汇总到仓库，前 N 条），`top` 可选（默认 10、上限 100）；控制台新增「使用分析」页（仅管理员可见）以统计卡片 + 表格 + 进度条展示总览、热门制品与仓库用量。纯查本机内部聚合数据、不接任何外部导出，**绝不外发、不向外部遥测 phone-home**
- Prometheus 指标端点（FR-32，ADR-0015）：用 `metrics` facade + `metrics-exporter-prometheus` 进程内 recorder（pull 模型，不引外部时序库、不 push / remote-write），新增 `GET /metrics` 渲染进程内注册表为 Prometheus 文本；指标中间件在请求热路径无锁采集 HTTP 维度（method / status_class / format 计数与延迟直方图、上传 / 下载字节、并发上传 gauge），`proxy` 回源边界埋点缓存命中 / 未命中与上游耗时 / 失败，标签均为有界枚举（严禁仓库名 / 路径 / 用户名 / 坐标作标签）；端点默认仅 Admin（`401` / `403`），新增 `[observability.metrics]` 配置 `enabled`（默认开，关闭则 404）与 `allow_anonymous`（默认关，开启须限内网 / 反代后）；指标本机内部、仅抓取时渲染、不主动外发
- 用户组/团队与对组授予仓库 ACL（FR-49，ADR-0007）：新增 `groups` / `user_groups` / `repo_group_acl` 三张表与组管理端点（仅 Admin）——建组 / 删组（级联清成员与组 ACL）/ 加移成员、对组授予 / 撤销仓库读 / 写 / 删 / 管理四级 ACL；授权判定中用户对某仓库的有效权限改为「直接 ACL ∪ 其所属各组的组 ACL」取并集后按动作蕴含判定，既有直接-ACL 判定结论与鉴权矩阵保持不变；私有仓库列表过滤与详情 / 浏览同步纳入经组继承的读权限。增强管理 UI/API（FR-50）尚未实现
- 制品漏洞标记（FR-71，ADR-0012）：制品详情 `GET /api/v1/repositories/{id}/artifacts/{path}` 响应新增 `vulnerabilities` 数组，列出该制品命中的已知漏洞公告（id / 严重度 / 摘要）。`format` 各处理器经 `vuln_coordinate` 从制品路径反解生态坐标（Maven `groupId:artifactId`、npm 包名，含版本；Raw / Docker 无坐标不参与），`meta` 按 `(ecosystem, package)` 查本地候选受影响行，`vuln::select_hits` 用纯函数据 OSV `affected` 范围语义（`introduced` 起含、`fixed` 止不含、`last_affected` 止含，另含显式 `versions` 列表）判定命中并去重。即时查本地受影响坐标表匹配、不落制品-漏洞缓存表；全程只比对本机已镜像数据，制品坐标绝不外发到外部漏洞服务（守数据不外发红线）
- 基础速率限制（FR-33，ADR-0008）：新增 `[protection.rate_limit]` 配置与限流中间件，按 **IP 维度**（连接来源地址）与 **身份维度**（已认证用户及其所有 Token / 会话）用进程内固定时间窗计数，任一维度单窗超阈值即在进入业务前返回 `429 Too Many Requests`（错误码 `too_many_requests`，带 `Retry-After`）；中间件置于身份解析之内、业务之前，热路径只取一次锁做整型自增与窗口比较（无锁外 IO / 无格式化），窗口表过期键按表大小阈值顺带清理防无界增长。来源 IP 取连接级 `ConnectInfo`、**不采信 `X-Forwarded-For`**（伪造来源不绕过），轮换 IP 的同一主体仍受身份维度阈值约束。配置 `enabled`（默认关闭）、`window_secs`（默认 60）、`ip_max_requests`（默认 1200）、`identity_max_requests`（默认 2400），**默认阈值保守、不误杀正常包管理器批量拉取**，配置热替换下个请求即按新值判定。仅应用层（L7）基础限流；多维（用户 / 仓库）限流与并发/连接上限属 FR-51、慢速 / 封禁 / CC / WAF / 告警属 FR-52~56，均不在本批，L3/L4 体积型攻击仍由前置设施承担
- 多维限流与并发/连接上限（FR-51，ADR-0008）：在 FR-33 基础限流上扩展 `[protection.rate_limit]`，新增**仓库维度**固定窗限流（按格式路径首段仓库名计数，保留前缀 `api`/`v2`/`health`/`metrics`/`assets` 不计入）与 **IP / 用户 / 仓库 三档在途并发上限**——任一维度单窗超阈值或超并发上限即在进入业务前返回 `429`。并发计数走分片 `Mutex`（按键散列分片降争用），入站 +1、由 RAII `ConcurrencyGuard` 在请求结束（含出错 / panic）`Drop` 时 -1，**可靠归还、不泄漏在途计数**，计数归零的键随即移除防无界增长。来源 IP 仍取连接级 `ConnectInfo`、**不采信 `X-Forwarded-For`**（伪造来源不绕过）。新增配置 `repo_max_requests`、`ip_max_concurrent`、`user_max_concurrent`、`repo_max_concurrent`，**默认 0（不启用 / 不限并发）**，旧配置无需改动即向后兼容，**默认保守不误杀正常包管理器批量并发拉取**，配置热替换下个请求即按新值判定。仅应用层（L7）多维限流与并发/连接上限；慢速 / 封禁 / CC / WAF / 告警属 FR-52~56，均不在本批，L3/L4 体积型攻击仍由前置设施承担
- 慢速攻击（slowloris）超时与通用请求体大小限制（FR-52，ADR-0008）：新增 `[protection.slowloris]` 配置与慢速防护中间件（置于身份解析之外、读取请求体前介入），把请求体包成带超时与累计字节上限的数据流——等待首个数据块超过 `header_timeout_secs`（慢起始）、或相邻数据块间隔超过 `body_read_timeout_secs`（慢 drip）即以 IO 错误**终止流并断开连接**，避免慢速连接长期占用。**超时按「块间空闲」而非「整体时长」判定**：正常持续发数据的大文件流式上传（mvn deploy 大 jar、docker push 大层）不触发，只惩罚长时间不发数据的 slowloris。同时新增对**所有请求**的请求体通用大小上限 `max_body_bytes`（区别于仅约束制品上传体的 `limits.max_artifact_size`）：带 `Content-Length` 时在进入业务前即拒 `413`（不读体），分块传输则边读边计、累计超限即断流。中间件未启用时直接放行、零包裹开销；启用时仅给请求体套一层流式计时 / 计数包装（逐块惰性处理，不缓冲整个体），超时 / 超限后立即终止本流、不再 poll 慢速底层流。配置 `enabled`（默认关闭）、`body_read_timeout_secs` / `header_timeout_secs`（默认 30 秒）、`max_body_bytes`（默认 0 不启用），**默认保守不误杀正常大制品流式上传**，配置热替换下个请求即按新值判定。仅应用层（L7）；封禁 / CC / WAF / 告警属 FR-53~56，均不在本批，L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担
- 角色与权限管理增强 UI（FR-50，ADR-0007）：Web 控制台接入用户组与四级动作管理（均仅 Admin）。仓库详情「权限」页签拆为「用户授权」与「用户组授权」两块，授权动作下拉从读 / 写扩为读 / 写 / 删除 / 管理四级（对接 FR-48 既有 ACL 端点）；新增「用户组管理」页支持建组 / 删组、经成员弹窗加入 / 移出成员，仓库「用户组授权」面板对组授予 / 撤销四级 ACL（对接 FR-49 既有组管理与组 ACL 端点）。前端 API 客户端补齐组管理、组 ACL 与四级动作的类型与调用；四级动作的下拉选项 / 中文标签 / 徽章配色抽为共享辅助供用户与组两套面板复用。本批为纯前端增强，不新增后端端点
- OIDC 认证集成与认证 provider 抽象（FR-34，ADR-0016）：新增统一认证 provider 抽象（`AuthProvider` trait + `AuthenticatedSubject`，本地口令为默认且始终启用，为 OIDC 与后续 LDAP 留统一接口），接入 OIDC 授权码流 + PKCE——新增 `GET /api/v1/auth/oidc/login`（生成 state + PKCE + nonce 并重定向 IdP）与 `GET /api/v1/auth/oidc/callback`（校验 state、换码、校验 ID Token 签名（JWKS RS256）/ iss / aud / exp / nonce、解析外部身份），经「外部身份 → 本地用户」映射后照常签发既有会话 JWT（TTL / 刷新 / 登出与既有一致），外部身份只在登录边界出现、收敛为本地会话，既有四通道与鉴权矩阵不变；新增迁移 `0009_external_auth.sql` 给 `users` 加可空 `external_idp` / `external_subject` 列（仅存非敏感身份标识、不存外部凭据）。即时开通（JIT）默认关闭（`auto_provision=false`，无对应本地用户则拒登录，守不自助注册红线），开启时即时建用户默认角色固定 `User`、绝不自动 `Admin`；外部用户口令哈希为占位串、不能经本地口令登录。新增 `[auth.oidc]` 配置（issuer / client_id / client_secret / redirect_uri / auto_provision），`client_secret` 真源 env / 配置、绝不入库 / 进日志 / 进 DB 明文，ID Token 等脱敏。网络 IO 走纯 rustls 的 reqwest、ID Token RS256 校验复用既有 jsonwebtoken（`rust_crypto`），不引入新依赖、不拉 openssl / native-tls。LDAP 认证集成（FR-35）尚未实现，provider 抽象已为其留好接口
- LDAP 认证集成（FR-35，ADR-0016）：经同一认证 provider 抽象的口令型 `authenticate_password` 接入 LDAP **bind 校验**——仅参与既有口令型登录入口（Web 表单 `POST /api/v1/auth/login`、Basic Auth，含 Docker v2 令牌端点），本地口令 / API Token 均未命中时委托 LDAP：服务账号（`bind_dn` + bind 口令）连接目录，按 `user_search_base` + 过滤模板（`{username}` 占位、RFC 4515 转义防注入）搜出唯一用户 DN，再用该 DN + 用户提交口令做一次 bind，成功即认证通过（外部 `subject` 取用户 DN），经既有「外部身份 → 本地用户」映射（复用 FR-34 的 `external_idp` / `external_subject` 列与 `resolve_external_login`，无新迁移）收敛为本地会话 / 身份；既有四通道与鉴权矩阵不变，与 OIDC provider 并存不串味。即时开通（JIT）默认关闭（`auto_provision=false`，守不自助注册红线 ADR-0010），开启时默认角色固定 `User`、绝不自动 `Admin`。连接走 LDAPS / StartTLS，TLS 由 **rustls（ring）** 提供（新增依赖 `ldap3`，仅启用 `tls-rustls-ring`、关 native-tls / sync，不拉 openssl / aws-lc）；默认拒绝明文 `ldap://`（除非显式 `allow_insecure`，限可信内网），空口令前置拒绝防匿名 bind 误判。新增 `[auth.ldap]` 配置（url / bind_dn / bind_password / user_search_base / user_filter / username_attr / starttls / allow_insecure / conn_timeout_secs / auto_provision），`bind_password` 真源 env / 配置、绝不入库 / 进日志 / 进 DB 明文，用户提交口令仅用于一次 bind、不留存。真机互通（对接 AD / OpenLDAP）待真机验（需 LDAP 目录）
- 访问异常检测与自动封禁 + IP 黑/白名单（FR-53，ADR-0008）：新增 `[protection.ip_list]`（`allow` / `deny`，支持 IP 与 CIDR、IPv4 / IPv6）与 `[protection.ban]`（`enabled` / `window_secs` / `threshold` / `duration_secs`）配置，及置于热路径前端的异常封禁中间件。**IP 黑/白名单**：白名单优先级最高、命中即豁免一切应用层防护（限流 / 封禁 / 异常统计），黑名单命中即在进入业务前直接拒 `403`；名单启动时预解析为网段集合，非法项记 WARN 跳过不阻断启动。**访问异常检测 + 自动封禁**：在固定时间窗内按来源 IP 统计异常信号（响应 4xx，含 401/403 鉴权失败与限流 429；5xx 不计），单 IP 一窗内异常信号达阈值即自动封禁一个时长、封禁期内一律拒 `403`、到期自动解封。封禁与信号计数为进程内内存（与登录失败计数同源、重启即清、不落 DB），各经 `Mutex` 保护、并发下计数一致，过期键按表大小阈值顺带清理防无界增长；未启用（名单空 + `ban.enabled=false`）时走零开销快路径。来源 IP 取连接级 `ConnectInfo`、**不采信 `X-Forwarded-For`**（伪造来源不绕过黑名单 / 封禁、也不能借伪造头逃避异常统计）。默认关闭、阈值保守宽放（默认窗 60 秒 / 阈值 100 / 封禁 900 秒），正常包管理器偶发 404 / 鉴权重试不触顶，配置热替换下个请求即按新值判定。仅应用层（L7）；CC 挑战 / WAF 规则引擎 / 监控告警属 FR-54~56，均不在本批，L3/L4 体积型攻击仍由前置设施承担
- CC 挑战（工作量证明 PoW，FR-54，ADR-0008）：新增 `[protection.cc_challenge]`（`enabled` / `difficulty` / `ttl_secs` / `exempt_authenticated`）配置与置于身份解析之内的 CC 挑战中间件。对疑似 CC（HTTP 洪水）攻击的**匿名**来源下发工作量证明挑战：客户端须找到 `nonce` 使 `sha256(challenge_token + ":" + nonce)` 的二进制前导零位数达 `difficulty`，以请求头 `X-CC-Solution: <token>:<nonce>` 带证明重试方放行；无 / 错误证明返回 `429`（错误码 `cc_challenge_required`）并在响应体携带挑战参数（令牌 / 难度 / 过期 / 提交头名）。**服务端无状态校验**：挑战令牌用 HMAC-SHA256 签名（密钥复用会话 JWT 派生的域分隔子密钥、不直接暴露 JWT 密钥本体），载荷绑定**连接级来源 IP** + 签发时刻 + 难度，不存挑战态；校验经签名（常量时间比对）、过期、来源 IP 绑定、PoW 难度四关。**默认关闭**——正常包管理器 CLI（mvn / npm / docker / curl）不会解 PoW，无差别拦截会打断正常拉取，故启用与否由运维显式承担；**默认豁免已认证（Bearer / Basic / 会话）请求**（带凭据的 CLI 不受挑战影响），挑战只面向匿名可疑流量。来源 IP **不采信 `X-Forwarded-For`**（换 IP 的证明不可复用，防绕过）；未启用时直接放行、零开销，启用时匿名请求仅一次 HMAC + 一次 SHA256 校验（无锁 / 无 IO / 无 DB），配置热替换下个请求即按新值判定。优先 PoW 而非验证码（CAPTCHA 需第三方 / 前端交互，不适配 API / 包管理器场景，且会引入外部依赖）。仅应用层（L7）；WAF 规则引擎 / 监控告警属 FR-55~56，均不在本批，L3/L4 体积型攻击仍由前置设施承担
- 可配置 WAF 规则引擎（FR-55，ADR-0008）：新增 `[protection.waf]` 配置（`enabled` + 有序 `rules` 数组，每条 `field`（method/path/query/header）、`header_name`、`pattern`、`match_type`（literal/wildcard/regex）、`action`（block/allow））与置于热路径前端的 WAF 中间件。规则在启动期**编译一次**（正则经 `regex-lite` 预编译、通配 `*`/`?` 转译为锚定正则、字面走子串包含），**非法规则记 WARN 跳过、不阻断启动**；中间件按请求 method / path / query / 指定 header **有序匹配、首个命中生效**——命中 `block` 即在进入业务前返回 `403`（错误码 `forbidden`），命中 `allow` 即放行并短路后续规则。未启用或空规则集走**零开销快路径**直接放行、不影响正常包管理器请求；WAF 按请求属性匹配、**不依赖来源 IP、不采信 `X-Forwarded-For`**。默认**空规则集 + 关闭**，启用与规则由运维显式承担，配置热替换重建规则集后下个请求即按新规则判定。新增直接依赖 `regex-lite`（纯 Rust 轻量正则，无原生依赖，编入默认产物）。仅应用层（L7）；CC 挑战 / 监控告警属 FR-54 / FR-56，不在本批，L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担
- 防护监控与告警（FR-56，ADR-0017）：为五类 L7 防护新增低基数 `jianartifact_` 指标并接入既有 `GET /metrics`——`rate_limit_rejected_total`（标签 `dimension=ip|token|repo|concurrency`）、`ban_triggered_total`、`ban_active_ips`（gauge）、`cc_challenge_issued_total`、`cc_challenge_failed_total`、`waf_blocked_total`、`slowloris_timeout_total`，各防护命中点用 `metrics` 宏原子累加（recorder 未装时为 no-op），严禁以 IP / 仓库名 / 规则模式串作标签。新增进程内**阈值告警引擎**：固定时间窗内按维度累加防护事件计数，单维度窗内计数达阈值即按严重度记中文分级日志（WARN/ERROR）并经有界 channel **异步不阻塞落 SQLite**（新增 `protection_alerts` 表 / 迁移 `0010`），同一维度窗内**去抖**（一窗只告警一次、不刷屏），热路径只做原子累加 + 一次内存计数（锁内无 IO），落库与审计 / 使用分析同款范式（写任务批量落库、行数兜底裁剪、失败仅 WARN 不阻塞）。新增管理员只读端点 `GET /api/v1/protection/status`（各维度窗内计数、当前封禁 IP 数、最近告警）与 `GET /api/v1/protection/alerts`（分页查询告警历史，统一 offset/limit），均仅 Admin（匿名 401 / 非管理员 403）、纯本机聚合零外发。新增 `[protection.alerts]` 配置（`enabled` 默认关闭、`window_secs` 默认 300、各维度阈值默认保守宽放、`max_rows` 默认 10 万）。告警是本机内部数据：只落本地、经 `/metrics` 被动 pull 或状态端点本地查询，**不内置外发型通知（Webhook / 邮件等若未来要做须另写 ADR）**；默认关闭 + 保守阈值避免无人值守刷告警与正常高频访问误报。仅应用层（L7）；L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担

### 变更
- 仓库 ACL 权限动作细化为四级 `read` / `write` / `delete` / `admin`（FR-48 / ADR-0007）：授权判定纯函数按动作蕴含关系（admin ⊇ delete ⊇ write ⊇ read）综合可见性、全局角色与 ACL 给出结论；既有读 / 写授权语义与判定结论保持不变，既有 `read` / `write` 数据原样兼容；ACL 管理端点（`POST /api/v1/repositories/{id}/acl`）接受四级动作取值。本次仅落地动作模型与判定，删除 / 管理动作的具体业务端点未接入

### 修复
- NuGet 发布兼容 `dotnet nuget push` 原生鉴权（FR-29）：身份解析中间件新增识别 NuGet 规范的 api-key 头 `X-NuGet-ApiKey`，无 `Authorization` 头时按 API Token 校验该头值，使 `dotnet nuget push -k <token>` 原生互通（此前仅接受 `Authorization: Basic`，dotnet 默认仅发 api-key 头而返回 403）。非法 api-key 仍按匿名处理、不绕过鉴权
- Nexus 离线 blob store 迁移识别真实仓库名键（FR-37 / FR-38 / FR-39）：离线 `.properties` 解析改用 Nexus 3.x 实际写出的 `@Bucket.repo-name` 键（回退兼容历史 `@Repo.repo-name`）。此前仅认 `@Repo.repo-name`，对真实 Nexus（实测 3.70.2）blob store 枚举为空、proxy/hosted 迁移搬运 0 制品；修复后离线预览与制品搬运对真实 blob store 正常工作（实测：hosted 制品与 proxy 缓存制品均成功搬运、sha1 一致、可从本系统取回）

### 移除
- 无

### 安全
- 无

## [0.1.0] - 2026-06-24

首个正式版本，交付第一期（P1）全部 36 项功能需求（FR-01..25、FR-59..69），含四种高频格式（Maven / npm / Docker、OCI / Raw）的 hosted 与 proxy、认证鉴权、Web 控制台与单一二进制打包。

### 新增
- 项目文档与治理脚手架初始化（PRD、架构、ADR、防漂移规则、工程化配置）
- 运行地基：TOML + 环境变量配置加载、嵌入式 SQLite 元数据库与迁移、文件系统 blob 存储（多校验和）、空库首启管理员引导、健康检查端点
- 认证与身份层：本地口令登录与 JWT 会话（TTL / 刷新 / 当前用户 /me）、API Token 签发/列表/吊销（哈希存储）、Basic Auth 鉴权、全局角色与管理员用户管理、统一身份解析中间件（Bearer-JWT / Bearer-Token / Basic / 匿名 四通道）、登录暴力破解防护（失败锁定 / 限流）
- 仓库模型与授权层：仓库创建/配置/删除（格式、hosted/proxy 类型、public/private 可见性）、每仓库读写 ACL 管理、按全局角色×可见性×ACL 综合判定的授权纯函数、仓库列表（按身份过滤）/详情/制品浏览端点；私有仓库对匿名与无权用户一律返回 404 隐藏存在性
- 制品通用机理与统一格式 trait + Raw 参考格式：hosted 制品流式直传/下载、proxy 代理上游并缓存（cache-miss 回源→校验→落盘→写索引、命中不回源、并发单飞合并、上游失败回退不写坏缓存）、blob 先落盘再写索引（失败回滚不留孤儿）、上传大小限制（超限 413）、四校验和计算与暴露、制品删除与按格式覆盖策略、Raw 格式端点（PUT/GET/DELETE 路径直存直取）、制品详情（四校验和 + 使用方式片段）、跨仓库搜索（结果按读权限过滤、不泄露无权私有制品）
- 三种高频格式（hosted+proxy）经统一 Format trait 注册接入通用机理：Maven（仓库布局、maven-metadata.xml、.sha1/.md5/.sha256 sidecar、release 不可覆盖 409 / snapshot 可覆盖）、npm（packument/tarball、publish 解析 _attachments、已发布版本不可覆盖、dist shasum/integrity 摘要、scoped 包）、Docker/OCI（Registry v2：blob 上传状态机与 digest 校验、manifest 存取、同 tag 可覆盖、tags/list 列出镜像 tag）
- Docker Registry v2 Bearer 令牌认证：新增 `/v2/token` 范围令牌端点（Basic 凭据换取短期 docker 令牌、按 scope 逐项判定授予动作），`GET /v2/` 未带凭据时返回 `401 + WWW-Authenticate: Bearer` 发起认证发现、受保护操作未认证时返回带 scope 的 Bearer 质询，docker 操作接受 `Authorization: Bearer` 令牌；复用会话 JWT 的 HS256 密钥；匿名拉取 public（透明换取匿名令牌）与预先 Basic（curl）照旧可用。让真实 OCI 客户端（skopeo / docker）的认证推送可用
- React Web 控制台（登录与基础仪表盘、仓库管理、用户与每仓库 ACL 管理、Token 管理、制品浏览与跨仓库搜索及详情）：React + Vite + TypeScript + Mantine，登录拿 JWT 放 Authorization 头、401 跳登录、统一错误与分页解析、按角色显隐管理界面；经 rust-embed 编译期嵌入前端产物，axum 提供静态资源与 SPA 客户端路由回退（不拦截 API / 格式 / 健康检查端点）

### 变更
- 无

### 修复
- 无

### 移除
- 无

### 安全
- 记录 RUSTSEC-2023-0071（rsa crate Marvin 攻击，计时侧信道，中危，无修复版本）受控忽略：该依赖经 jsonwebtoken 的 rust_crypto 伞形特性传入，本项目 JWT 仅用 HS256（HMAC）、从不执行 RSA 运算，计时侧信道在实际执行路径不可达；理由与复核条件见 `.cargo/audit.toml`

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
