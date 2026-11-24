# 功能规格：协议与迁移发布准入

> 状态：开发完成，待用户验收；Raw PUT、Maven PUT、Maven 网页批次、npm、Cargo、OCI、NuGet、PyPI 与迁移写回全部写入口已接入准入管线，未获准不公开字节、不伪报成功；各原生协议客户端真实兼容验收未执行　·　关联 PRD：FR-170　·　分支：`feat/security-two-factor`

## 1. 背景与目标

计划 F。面向发布者、CI 和 Nexus 迁移管理员：让客户端的发布成功对应已提交、在相应读取权限下可见的对象；让直接领域调用、重试、移动、元数据更新等不经 HTTP 的入口也无法绕过准入。建议 P1，最终随 PRD 登记。

依赖 [私有候选与有界静态检测](security-content-admission.md)、[可信审计](security-trusted-audit.md)、[凭据生命周期](security-credential-lifecycle.md)和 [事故处置](security-incident-controls.md)；人工例外复用[双因素认证](security-two-factor-authentication.md)。架构取舍见 [ADR-0034](../adr/0034-private-candidates-and-publication-admission.md)。

## 2. 需求（要什么）

- 所有首次新增 hosted 可见引用和改变可见对象/指向的写入统一复核；有协议分步的不强造一个跨请求的大事务。原生语义正确优先于候选尽快返回成功。
- 统一目标授权、真实身份、限时批准和本地持久回执；重放已有回执是历史查询，不是发布或下载授权，不能重放副作用。
- 范围内：Raw、Maven、npm、OCI、Cargo、PyPI、NuGet 实际写入口、管理端上传、Raw 操作、格式元数据及 Nexus 导入；Go 只覆盖当前实际支持的能力。
- 不做：恢复退役复制、开放未支持的 Go hosted/导入或 OCI 跨仓挂载、整站恢复当逐包发布、把 proxy 缓存标为已扫描。整实例恢复走 D。
- 这是对历史交付的增强：关联 FR-16a、FR-20～23、FR-31、FR-104/105、FR-138、FR-146/149；不改变原有 PRD 状态。FR-104 的代码/文档语义归真见 ADR-0034。

## 3. 设计（怎么做）

### 3.1 最终提交与权限

1. 入口借 A/B 获取真实 `actorUserId,credentialId,sessionId,authMethod,authStrength,requestId` 和内部 `accountEpoch,securityEpoch`，规范化 `repositoryId,canonicalPath,digest`。候选、上传会话和迁移任务持久绑定 A 的不可变来源快照 `{originRequestId,clientIp,proxyTrust,sourceTrustVersion}`；后台没有直接 HTTP 时不伪造请求或来源。客户端输入和系统进程名不能补造可信主体；机器凭据用途、范围与实时 ACL 求交。
2. 私有候选封存、扫描、批准按 E。派生索引/标签如依赖现状，在锁外生成并纳入 `manifestDigest`，绑定依赖对象和已提交版本。最终事务发现索引/前镜像已变时，回到私有重算，不覆盖并发提交，也不沿用旧批准。
3. 最终公开事务强制核验实际变更集与同一操作的有效批准或裁决依据：候选、规则、引擎、解析配置、策略、清单版本、覆盖、有效期、目标及闭包、实时主体/凭据/代次/ACL、事故锁/恢复闸/搬迁写闸、配额及不可变规则。仅 `direct/trusted`、规范 `clientIp` 且 `sourceTrustVersion` 仍为当前版本才有效；同一事务按当前发布 IP/CIDR 规则重判原发起 IP。分段上传另查封口请求，不能覆盖会话发起来源。规则不可判定、来源未知/无效或信任版本变更时暂停，不使用新请求、worker、代理 peer、回环或恢复操作者地址代替。限制先提交则阻断发布；发布先提交则如实保留其先后关系。
4. SQLite 一个短事务写公开资产、格式元数据、FTS、已有本地 operation 记录、A 成功事件及新的准入回执，消费批准并标 `published`，`COMMIT` 为唯一可见性线性化点。不得由 handler 自插审计模拟成功；不得用文件存在、outbox 或任意 hook 代替准入。
5. 没有配置/初始化准入、缺失可信主体/批准、无法可靠写账、恢复未通过均拒绝 hosted 公开提交。不允许因功能开关或协调器为空走 `AssetRepo.Upsert`；未完成接入的 hosted 写入口须阻断，不能声称整批启用。已经在公开写事务中完成的历史操作，不能因快照/通知/响应失败回滚重发。
6. 出口保持发布/读分权。对活动资产的读先查有效已提交引用与 D 读限制，再判断 304/206、返回 HEAD 或打开摘要内容；group 按实际来源复核，不能把被隔离的引用代理出去。派生输出仅从当前允许可见的资产生成；新候选不参与覆盖已有对象或负缓存失效。

### 3.2 从最终公开事务反查的入口矩阵（基线实际代码）

下列文件路径相对 `apps/server/internal/`，行号用于审核草稿时定位；须把矩阵用于 F 的服务级覆盖验收，不等同已接入。基线末端是 `repository/asset_mutation_repo.go:229–258` 的 `complete`；`applyMutationItem` 可新增/换址/替换 asset 并更新 FTS。上一层为 `domain/asset_mutation.go:141–215` 的 `applyLockedWithEnvelopeAndHook`；公开变更不能仅在常见 HTTP 前置检查。

| 倒查到的实际入口                                                                                                                                                        | 经由或直接效果                                                                                                                         | F 归类与必拦面                                                                                                                                                                            |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `domain/asset_mutation.go:65–139,253–294`：`Apply*`、`Put*`、事务 hook 与信封变体；`repository/asset_mutation_repo.go:168–198`：`Complete*`                             | 多种内部入口汇合 `complete`；hook/信封可选                                                                                             | 强制围绕真实 hosted 新增/替换和格式引用差量，无批准的直接调用必须失败；禁止构造通用 hook 绕过，测试事务掉线和未接线                                                                       |
| `protocol/raw.go:277–319`：`RawHandler.Put`，原生 Maven 复用 Raw PUT                                                                                                    | `AssetService.Put` → `putWithPrimaryOperation` → `PublishAssets`（`domain/asset_service.go:390–430`）                                  | Raw 标准/管理上传及 Maven 规范化别名写均逐 PUT；不得只接 Maven 表单                                                                                                                       |
| `protocol/maven_upload.go:81,231` `UploadForm`；`protocol/npm.go:262,429` `publish`；`protocol/npm_registry.go:60,188,250,598` registry PUT / dist-tag / 撤版 packument | 表单和 npm 主发布经 `PublishAssets`；packument 写助手另经 `assets.Put`；原子撤包走 `ApplyOperation`                                    | 批次、tag 元数据、仅改 packument、撤版派生重写均分类，不允许附加 JSON 指向候选或外来内容                                                                                                  |
| `protocol/oci.go:101,271,302,379`：直接 POST、会话封口、manifest PUT                                                                                                    | `domain/oci.go:56,93,175–200` `PutBlob/PutManifest` → `PublishOCIAssets`，包括 digest、tag；分块暂存不新增引用                         | blob 与 manifest/tag 分别准入；现有 POST 没有跨仓 mount 分支，`mount/from` 不代表成功挂载；不得加跳检挂载                                                                                 |
| `protocol/cargo.go:75`：publish、yank/unyank                                                                                                                            | `domain/cargo.go:89–178,309–314`：`PublishWithGuard` 提升 crate+index，状态重写另一次 `PublishAssets`                                  | index 指向增删/变化也须核验，yank/unyank 不应扫无变化旧字节但不得引入未准入引用                                                                                                           |
| `protocol/pypi.go:167–220`、`protocol/nuget.go:175–235`：`LegacyUpload`/`Push`                                                                                          | `domain/format_metadata_service.go:94–179,699–751`：`PublishPyPI*`/`PublishNuGet*` 经 `PublishAssetsWithEnvelope` 提交文件和格式元数据 | 发布/包索引同步；web/API/迁移复用领域方法必须走同一闸                                                                                                                                     |
| `domain/asset_service.go:479–520,556–631,694–696`：直接 `PutWithCommitHook`、内部批量 `PublishAssets*`、`PublishOCIAssets`                                              | 分别经协调器单件/批次/信封重载，有调用方自定义元数据 hook                                                                              | 服务可直接调用，分别验缺回执不提交；真实变更含 hook 的引用，整体回滚，不留 asset、FTS、格式元数据或成功事件                                                                               |
| `api/asset_operations_handlers.go:17–52`、`domain/asset_operations.go:113–166,248,570,634–654,806`：`ApplyOperation/planRawMove/planDelete`                             | Raw 移动/改名新增目标路径（即使复用旧摘要）；格式感知删除可重写 Maven metadata/校验和及 npm 索引；纯删除不新增                         | 目的地按首次可见目标查当前权限和目标准入；纯删除不要求新内容批准，不阻断 D 的安全处置；派生改写必须验证不新增未获准指向。未见独立制品复制动作：若未来新增必须按目标准入，不复活跨节点复制 |
| `migration/runner/runner.go:114,129,206,580–609` `StartAsync/Finalize/run/importItem`；`domain/migration_service.go:350,406–422,639`                                    | 在线/离线分支、启动/继续/最终增量经格式 importer 或 `AssetServiceAdapter.Put*` → `AssetService.Put*`；恢复/重试沿同路径                | 两条写路径、每次异步提升和未完成任务重放都核原发起身份；回执重放不新增，失效/未知发起主体不写                                                                                             |
| `domain/migration_format_importer.go:47–79` 分派 Cargo、OCI、PyPI、NuGet；`domain/migration_cargo_importer.go:19–68`                                                    | 按格式还原索引与内容，Cargo 的导入索引可另经 `PublishAssets`                                                                           | 不能只包 runner 通用 Put；迁移索引如未形成依赖闭包只能待审/待依赖，不提前挂引用                                                                                                           |
| `protocol/go_proxy.go:39`，`domain/asset_service.go:771–800,1138,1414`                                                                                                  | Go 仅 proxy；`persistAssetWithHook` 与代理落库存在 `Upsert` 备用支路；`repository/asset_repo.go:51` 为真实直接 SQL 资产插入路径        | 校验目标类型，备用写不许用于 hosted；proxy 标代理来源而非 hosted 回执，仍执行 D 读限制。不得新增 gomod 发布/导入                                                                          |
| `api/search_index_handlers.go:179` → `domain/search_index_service.go:269–290` `Rebuild`/回填；`repository/search_index_repo.go`                                         | 只清空/回填由 `asset` 派生的 FTS，不新增 asset/协议可下载引用；单纯时间/校验和补录不改指向                                             | 不签发布回执；索引权限和 D 限制仍有效，禁止读取候选表/暂存区；若未来重建协议索引会新挂可见引用，则走正式准入                                                                              |
| `domain/repository_service.go:739,884`；`domain/asset_mutation.go:369–464`、`cmd/jianartifact/wiring.go:156`；`repository/asset_mutation_repo.go:65,200,351`            | 仓库删除执行删除计划；启动从意图和快照收尾/补偿；存在遗留 received/回滚代码，不是当前发布通道                                          | 不借补偿普通新增客户可见引用；只复原明确的已提交前镜像，受 D 闸。已完成只清理、未完成只补偿或重新准入；整实例恢复另走 D；不接通 retired replication                                       |

生成并重写可下载的服务端派生字节（如 Maven metadata/校验和、npm packument、OCI tag、Cargo index），允许以受控生成事实参与清单：确定性生成、绑定已批准或允许继续服务的既有公开依赖、事务重查依赖和预期版本。不对客户端任意字节开「派生」白名单，不把旧存量等同新扫描通过；新目标的新引用必须有自身的本地准入回执。

### 3.3 逐入口封装与内容解析归属

必须在所有入口（含 importer 和直接服务调用）把隔离前置到**首次制品内容解析**，而非仅包住最终提交。主进程只做认证、路由、有界协议封装和流式落盘计量；来自协议封装的名称、长度、媒体类型等只作声明，只有隔离结果可供坐标、完整性和闭包裁决。先确认该路径的硬隔离能力；缺失或启动/附加失败时最多收为私有候选（若预算允许），记录 `scan_environment_blocked`，不得执行旧解析器或写成功回执。

| 写入口                                        | 主进程：有界封装与字节传输                                                                                                                                                                               | 隔离 worker：格式与内容解析                                                                                                                                                  |
| --------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Raw（原生、管理端）                           | 路由、简单字段/表单边界、摘要计数，文件原样流入私有暂存                                                                                                                                                  | 实际类型探测、归档与嵌套内容、安全特征检查；不按扩展名豁免                                                                                                                   |
| Maven 原生 PUT、网页批次、导入                | 单文件或有限 multipart，路径/GAV/分类器声明与纯校验和文本长度检查                                                                                                                                        | 客户端 POM/metadata XML、JAR/归档及内容，验证声明与包内坐标；旧来源索引若需重新内容解析也在 worker。主进程只基于已核验结构生成新派生 XML/校验和                              |
| npm publish、dist-tag/撤版                    | 发布 JSON 增量封装扫描，不 `ReadAll`/整包 `Unmarshal`；只保留有界元数据，将 `_attachments[].data` 增量送 base64 解码并直接落私有文件，不使用 `DecodeString` 或整体字符串令牌。tag/撤版仅收有限标识及参数 | tarball/嵌入 manifest、脚本和版本差异；从封存字节核对附件、版本、tag 和摘要声明。主进程基于经核验结构合并 packument，不从不可信 tarball 提取坐标                             |
| OCI 会话、封口、manifest/tag                  | 检查会话/偏移/摘要语法；流式封存 blob 及请求 manifest 原字节，封口核实际摘要；传输步骤不解析层或 manifest                                                                                                | blob 类型/归档与 manifest/index/config 内容（有界 JSON、依赖描述）；主进程按结构化描述验证本仓引用，再生成 tag，不执行镜像                                                   |
| Cargo publish、yank/unyank、导入              | 校验长度前缀、读取受限协议字段，crate/导入 index 原样暂存；状态变更命令只收坐标                                                                                                                          | `Cargo.toml`、crate 归档、服务所需的格式元数据与外部 index 记录解析；主进程依据结构结果合并索引                                                                              |
| PyPI 原生、导入（以及复用服务入口）           | 有界 multipart、表单字段与源路径名称作为声明；wheel/sdist 流式暂存                                                                                                                                       | wheel/sdist 结构、内部元数据与声明核对；主进程按可信结果投影 Simple 及相关元数据                                                                                             |
| NuGet 原生 push、Nexus `importMigrationNuGet` | 仅处理 multipart 或源字节流并封存输入；来源文件名作为声明，不因源已登记信任包内容                                                                                                                        | 两入口统一在 worker 执行 nupkg ZIP/nuspec 解析，替代 `parseNuPkgReaderAt` / `nugetpackage.Parse` 前置调用；返回有限 `id,version` 与结构元数据，主进程再做坐标 ACL 与写入计划 |
| 通用迁移 Put、内部单件/批量、直接协调器       | 传递私有对象句柄、实际摘要、真实来源快照；不允许 hook 把解析伪装成派生生成。纯移动可基于合格事实和新目标做裁决                                                                                           | 需要解读新字节或不可信格式元数据时走对应 worker；没有待解析输入的删除、FTS 投影不触发扫描。Go proxy 不构成 hosted 入口                                                       |

npm 拟议硬限额独立计量：发布 JSON 线传总量 352 MiB，非附件数据 2 MiB、结构深度 24、附件最多 64；编码输入每附件 172 MiB/全请求 344 MiB，解码输出每附件 128 MiB/全请求 256 MiB。按实际读写核计；声明值不可扩预算，较严现有配额优先。扫描/解码固定缓冲，不为附件的 JSON 字符串预分配；拒绝歧义/重复键、无效 base64、任一阶段超限，立即终止并释放未封存暂存引用，无部分发布。其他入口受 E 的输入、对象、解析上限及现有更严协议上限约束（Maven multipart/PyPI 等同时限制字段数和单字段字节）。

### 3.4 分协议成功界线及请求重试

| 协议/入口                       | 这一请求何时返回成功（拟议合同）                                                                                                                                                   | 不变的失败/重试语义                                                                                                                                                                            |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Raw / 原生 Maven                | PUT `201`，单个当前 PUT 字节与目标通过并可读；POM/JAR/classifier/校验和各自单元；服务端派生校验和和 metadata 使用已批准公开内容                                                    | 不等待未来 PUT，也不承诺标准 Maven 全 GAV 原子；传入 checksum 对已可见同件校验，metadata 不暴露待审/未提交版本；同一坐标不同 SHA-256 不能当重试                                                |
| Maven 网页批次、管理端 Raw 表单 | 可 `202`（仅受理，返回 `operationId,state`），完整批次全部获准后一次提交；状态查询才确认发布。若同步已提交可回 `201` + 回执                                                        | 单件待审整批保持私有，保留旧公开视图；同键同清单查同操作，不重复提交                                                                                                                           |
| npm                             | publish `201 {ok:true,id}`，tarball attachment、版本元数据和 dist-tag 一起；单纯 tag `201` 或删除原协议成功，仅在目标已有合法可见版本或更新已提交后                                | 固定用户意图键，合并并发 packument 以前镜像条件提交，冲突后重算并重验；不覆盖其他版本；撤版不得意外恢复另一个候选                                                                              |
| OCI blob 传输                   | 启动 POST、PATCH `202` + `Location/Range` 只确认会话/字节，不暗示 digest 可读。直接上传 POST 或分块最终 PUT 封口 `201` + `Location/Docker-Content-Digest`，须先在该仓允许 HEAD/GET | 待审/超时最终封口必须非 2xx；相同会话封口重试只认原绑定字节、同资源和 digest，不追加最后分块两次。会话丢失可重新开传输，仍复用相同准入操作；当前 mount 只能走正常上传（`202`），不返回挂载成功 |
| OCI manifest、index、tag        | 独立 PUT `201` + `Location/Docker-Content-Digest`；校验 manifest digest 及本仓内有权可读的 config/layers/子 manifest 的介质类型、大小和 digest 闭包；digest/tag 作为一个提交单元   | blob 成功不是镜像成功；缺依赖用分格式错误而非提前发布 tag，重试时重查闭包，不因共享 bytes 默许跨仓引用                                                                                         |
| Cargo                           | publish `200` 原 JSON，crate 和 index 记录同时提交；yank/unyank 的 `200` 仅表示已提交索引更改                                                                                      | checksum 与同仓 crate 匹配；依赖发布不得靠等待未来索引请求；相同请求回原回执，不重复追加行                                                                                                     |
| PyPI                            | Twine 单个 wheel/sdist `200 OK`，只提交该请求文件及其 Simple/格式元数据闭包                                                                                                        | 同版本其他文件独立；不能用 skip-existing 伪装新文件获准；同文件新字节按冲突/重新准入                                                                                                           |
| NuGet                           | nupkg `201`，flat container 和 registration 同一结果                                                                                                                               | 待审不映射为 duplicate 或成功；同 id/version 新字节不得复用旧记录                                                                                                                              |
| Go modules                      | 无原生 hosted PUT 成功或新增导入；保持 Go proxy 读取，未支持写入拒绝                                                                                                               | 不能借同路由 Raw path、迁移通用 Put 或共享 CAS blob 突破格式/仓库类型                                                                                                                          |
| Nexus 迁移                      | 作业启动/继续仅受理；逐单元的本地回执才计本次已发布，已有对象可独立记已存在                                                                                                        | 待审/待依赖非完成；重试只从已提交回执或受验证已存在对象推进成功水位                                                                                                                            |

普通原生发布不需要自定义头。可传 `Idempotency-Key` 的调用按 `(actorUserId,repositoryId,actionType,key)` 唯一，服务端持久绑定首次封存的意图和摘要。同键同意图返回已有状态/回执；摘要、目标或语义字段不同返回 `admission_key_conflict`。无头原生请求用服务端归一化意图键：原主体、仓库 ID、动作、规范目标、请求字节摘要、协议业务字段（OCI 会话另绑封口偏移和 digest）；不使用时间戳、网络重试次数或系统生成的随请求随机数改键。派生清单更新有新版本，但同请求意图仍指向原操作；不能复用失效批准。

稳定回执含 `operationId,eventId,repositoryId,actorUserId,actionType,manifestDigest,manifestVersion,approvalId,policyRevision,committedAt` 及已提交目标 `canonicalPath,digest,targetVersion`；为协议保留可重建的成功状态和必要 Location。与 A 共用 `committed=not_applicable/not_committed/committed/unknown`，独立于 `httpStatus/outcome`。响应丢失时先查原回执；若 `unknown` 只查不补发。原发起主体且当前有对应目标发布权限，或有相应调查权限的管理员，才可查询；过期/撤销凭据不可用。目标受限、删除或覆盖之后，重试只关联历史操作，不返回暗示当前可读的成功，也不自动新建候选；公开读不凭回执放行。

**合法删除后的显式重发**：当前有目标发布权限的交互主体通过管理端预览、近期 MFA 确认 `actionType=republish`（`targetVersion,confirmationToken,Idempotency-Key` 沿 C；绑定原 `operationId`、删除后的持久目标版本、仓库/规范目标、请求摘要及协议语义）。确认事务核对目标确因已提交合法删除而无当前绑定，D 限制、不可变规则等照常适用；原目标被替换、版本变化或删除不可证均不发新操作。成功只签发新的待接收 `operationId` 与短期接收期限，不公开、不复用旧批准。动作幂等重试返回同一个新 ID，不能同时开同目标版本的两次重发。

- 管理上传或原生客户端的显式适配请求可携 `X-Jian-Republication-Operation-ID` 关联新操作；同一已授权发布主体/凭据绑定、有效来源、目标、摘要及协议意图全部匹配才可接收，ID 不是凭据。该关联只在显式提交时生效；无此关联的原生旧请求永远只命中旧回执，不自动绑定最新重发。原生工具不能设头时用管理端或受支持的上传适配，不改普通客户端合同。
- 新操作绑定本次受信入口快照，重新封存、按当前 E/F 要求重新准入，终提交重检删除后 `targetVersion`、授权、限制和闭包。重放同新 ID 不重复写；超期、主体/摘要/目标不符不得借机重开。旧回执、审计和幂等映射保持可追溯，不能删除旧记录来解锁重发。

### 3.5 契约、错误与预算

候选详情沿用 E；显式重发预览/确认拟接到 E 的管理操作路径，成功响应 `actionId,operationId,targetVersion,expiresAt`。网页受理响应拟为 `operationId,requestId,repositoryId,state,manifestDigest?,expiresAt`。管理错误沿用 A 的 `{error:{code,message},requestId?,operationId?}`；只有有权者可得 `operationId`，事件 `errorCode` 独立。普通原生请求体不变；授权响应可附 `X-Jian-Operation-ID`，请求关联用服务端 `X-Request-ID`，不在 `Location`/URL 放令牌或样本信息。具体字段由主代理在 `api/openapi.yaml` 建模。

| 条件                                       | HTTP、权威原因码与其他行为                                                                                                                                            |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 未登录/无目标权限/凭据代次失效             | 保留 401 challenge 或 403；按 B 区分凭据失败与 `admission_forbidden`，不回候选存在性                                                                                  |
| 显式重发等敏感动作近期 MFA 不足 / 确认失效 | 403 `recent_mfa_required` / 409 `security_confirmation_stale`                                                                                                         |
| 待审/有批准但状态或依赖冲突                | 409 `admission_pending_review` / `admission_stale` / `admission_dependency_pending`；OCI 已缺依赖的 manifest 用相应原生 `MANIFEST_BLOB_UNKNOWN` 并保留该协议 400 语义 |
| 候选拒绝 / 硬完整性失败                    | 422 `admission_rejected` / `admission_integrity_failed`；已有协议规定的无效 digest 保留原生 400（如 `DIGEST_INVALID`），不归类为 duplicate                            |
| 幂等键碰撞 / 已提交目标不再一致            | 409 `security_idempotency_conflict`（通用动作）或 `admission_key_conflict`（准入内容）/ `admission_stale`（目标变化）；不自动重发                                     |
| 超上传体积 / 并发、队列、主体配额满        | 413 `admission_limit` / 429 `admission_busy`；限流可给 `Retry-After`；区分已有不可变/配额拒绝                                                                         |
| 准入服务或硬隔离不可用；审计不可用         | 503 `admission_unavailable` / `scan_environment_blocked`；审计按 A 为 503 `security_audit_unavailable`，不得退化到旧解析或旧写路径                                    |
| 搬迁冻结 / 恢复隔离                        | 503 `write_frozen` / 503 `recovery_isolated`，各保留独立原因与原生协议封套                                                                                            |
| 事故实例写锁 / 事故目标或来源规则拒绝      | 503 `incident_write_locked` / 403 `incident_restricted`，遵循 [D](security-incident-controls.md) 的授权及防枚举（无权获知存在性时保留原生 404 语义）                  |
| 有界等待用尽（绑定候选仍可能处理）         | 503 `admission_wait_timeout` + `Retry-After`，非发布成功；提交判定未知时只查同一回执                                                                                  |

适配只改变封套，不替换安全权威原因码：Raw/Maven 保持现有可读错误，npm JSON `error/reason`，OCI `errors[{code,message,detail}]`（保留原生 `code`，`detail` 带权威 `errorCode`）；Cargo 保留 `errors` 结构；PyPI/NuGet 保留原错误体。封套没有标准原因字段时通过响应头承载；防枚举规则优先，不对无权主体泄露引用存在性。客户端兼容性仍待真客户端验收。

拟议预算：完成上传后的同步等候上限 10 秒、全实例同时等待上限 32、单主体 2；不能持 SQLite 写事务或资产锁等待。在候选入场或等待饱和时给 429/503；初始 `Retry-After` 5 秒，不保证那个时刻已有结果。现有协议更严体积限额优先，OCI 会话另受 E 私有配额、字节限额和有效期约束。公开提交限制候选清单 256 项；更大的内部操作只在不破坏协议原子单元时分批，否则明确拒绝。下载热路径不扫描或外联。

### 3.6 Nexus 迁移、升级与恢复

- 已有任务记录 `InitiatorUserID/Username` 只是基础字段。由本地可信入口记录不变的原发起 `actorUserId,credentialId,sessionId,authMethod,authStrength,taskId`、仓库/路径范围、内部 `accountEpoch/securityEpoch` 及 A 的 `{originRequestId,clientIp,proxyTrust,sourceTrustVersion}`；格式 importer 与通用 Put 的候选继承同一快照。源 Nexus 凭据、源地址只用于读取源端，不表示本地授权；后台执行者、启动/续跑操作者及其来源另记。
- 开始、继续、最终增量及重启后，每次提升复核原身份及凭据/权限，按当前 D 发布规则重判原有效 IP；旧任务来源缺失、无效、信任版本变化或不能证明可放行时暂停。不能用 cron、`system`、新请求或导入源账号/IP 默认补写。只有受授权操作者可按当前条件从可信入口显式发起新任务并产生审计；不改旧任务原发起快照，不借旧候选/回执自动公开。
- `taskId,itemId,sourceDigest,repositoryId,canonicalPath` 建立稳定条目与 `operationId` 绑定；源对象变化视为新内容。逐项状态 `published/pending_review/failed/rejected`，已有合法对象另记 `existing`；未提交不能计完成/切换。恢复按回执和条目状态对账，拒绝同路径替换来源后复用批准。
- 已有配额豁免只限现有授权范围内的业务配额，不豁免私有暂存/worker 硬预算、目标不可变规则、身份或检测。冲突策略不能提升待审候选。取消不新发任务，已提交回执仍可追溯，不冒充从未发布。
- 升级前列出 Basic 使用者与实际客户端，按 B 签发受限机器 Token，验证 Basic 携 Token 等兼容方式后，独立批准关闭密码发布；管理员不留绕过 MFA 的等权密码旁路。存量显示旧覆盖，复检不将其全部撤下；代理/搬迁/退役范围保持不变。
- 搬迁/灾备只有 D 的外部可信检查点和安全时代闸通过才开放对外读取、后台发布与清理；恢复的历史回执不自动授予当前授权或发布，回滚应用也不得重新开放旧写旁路。

## 4. 任务拆分

- [ ] 逐入口落实最终写闸、强制的准入回执绑定、单提交幂等查询，并显式封堵 direct Upsert/hook 备用路径。
- [ ] 落地原生协议成功边界、错误封套与受理状态；明确元数据与索引派生闭包，分协议使用客户端验证兼容。
- [ ] 接通迁移的格式 importer 和通用 Put，审计发起人、恢复对账、任务报告与切换前置条件。
- [ ] 主代理统一契约/生成物、PRD、ARCHITECTURE、API、CHANGELOG，核对 ADR 取代范围；不在本文宣称实现。

## 5. 验收标准

1. **本地实现验收（部分执行）**：全部协议写入口接入准入管线并有端到端回归；主进程旧解析调用零探针矩阵与各原生客户端真实兼容未执行。验收条目原文：：隔离 SQLite 与回环服务逐矩阵测直接写缺回执、派生索引与迁移两分支。关闭硬隔离或注入 worker 附加失败，对原生 NuGet、迁移 `importMigrationNuGet`、npm 和其他内容解析入口装解析调用探针：主进程旧解析调用须为零；无公开引用和成功事实。npm 分别触发线传/元数据/编码/解码限额及无效附件，确认内存有界、无部分发布。候选/任务接收后新增原 IP/CIDR 禁止或提高 `sourceTrustVersion`，即使 worker、续跑请求或代理地址允许也须暂停；确认无 IP/不可信来源不被补写。验证代次、冻结/恢复错误（`incident_write_locked`、`incident_restricted`、`recovery_isolated`、`write_frozen`）保留原生封套。原生客户端验证 Maven 逐件、npm 并发合并、OCI 封口 HEAD/GET 与 tag 闭包，覆盖成功、待审、忙、超时及重启。合法删除后让延迟旧请求与显式重发竞态：旧请求只查历史；未确认、换主体/目标/字节、过期和版本竞争均不能提交；有效新 ID 重新准入且只新增一条发布事实；未装客户端标未执行。
2. **真实部署环境验收（未执行，需独立授权）**：实际部署代理、Linux/Windows 扫描隔离、客户端/CI 版本和超时兼容；验证 HEAD/Range/304、别名、group、摘要直读、负缓存和待审无泄露；授权样本迁移双分支及失败重启续跑，核对包/文件/任务报表。客户端不能按约定完成时对应格式不得标验收。
3. **生产可发布判定（未执行，需用户批准）**：凭据切换、只读/恢复闸与回滚安全时代演练通过，确认待审不会误判完成、失败升级路径和容量已验证。未覆盖格式、目标和环境明确列出；模拟成功不替代二、三级。

## 6. 风险 / 待定

- 需独立复核事务层对 hook 引入的额外引用、迁移状态与真实授权的绑定，以及已提交回执与原生「当前可见」之间的区分。
- 原生失败体、等待/重试预算、Web 接口接线及 OCI 客户端恢复分块行为尚未验证；不得为兼容返回假成功，也不得擅自新增 mount 或 unsupported Go 写入口。
- 已实现并通过质量门；原生客户端兼容、部署级与生产可发布判定未执行，见 §5。
