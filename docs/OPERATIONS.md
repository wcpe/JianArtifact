# 运维手册：JianArtifact

> 本文只记录当前可执行的部署、升级、备份、恢复、回滚和排障流程。版本真源是根目录 `VERSION`；当前发布版本为 `0.11.0`（M3 认证与运维条目已交付，下一个开发窗口尚未开启）。节点搬迁与备份以一致性备份包为单位（见 §3.2、§3.3、§5）；实时复制通道已随 FR-138 整体退役，本文不含任何复制拓扑、角色或凭据操作。

## 1. 运行与配置

### 1.1 当前支持的基础配置

配置通过环境变量、部署 Secret 或 systemd 环境文件注入；真实环境文件不入库。

| 变量                                   | 作用                                    | 默认/约束                                                          |
| -------------------------------------- | --------------------------------------- | ------------------------------------------------------------------ |
| `JIAN_HTTP_ADDR`                       | HTTP 监听地址                           | `:8080`                                                            |
| `JIAN_DATA_DIR`                        | SQLite 与 blob 数据根目录               | `./data`；生产必须使用持久化绝对路径                               |
| `JIAN_JWT_SECRET`                      | JWT(HS256) 签名密钥                     | 生产必填、强随机、不得打印                                         |
| `JIAN_MIGRATION_CREDENTIAL_KEY`        | 在线迁移凭据 AES-256-GCM 密钥           | Base64 编码 32 字节；生产应显式固定                                |
| `JIAN_UPSTREAM_TIMEOUT`                | proxy 回源整体超时（秒）                | `30`                                                               |
| `JIAN_ENABLED_FORMATS`                 | 启用的协议格式                          | 缺省 `raw,maven,npm`；显式空值关闭全部                             |
| `JIAN_PUBLIC_URL`                      | 对外基础 URL                            | 节点本地配置；影响下载链接与 usage 片段                            |
| `JIAN_SYNC_INTERVAL`                   | 设置页「同步间隔」的初始默认值（秒）    | `5`；仅在 setting 键不存在时写入，当前无调度器消费（复制退役遗留） |
| `JIAN_BLOB_GC_INTERVAL`                | 清理遗留 / 孤儿 blob 的间隔（秒）       | `86400`；`0` 禁用                                                  |
| `JIAN_STORAGE_CLEANUP_INTERVAL`        | 存储治理清理作业的间隔（秒）            | `86400`；`0` 禁用（作业不注册）；非法 / 负数回落默认               |
| `JIAN_STORAGE_METADATA_RETENTION_DAYS` | 终态操作与隔离元数据的保留天数          | `7`；`0` 禁用元数据裁剪；非法 / 负数回落默认                       |
| `JIAN_STORAGE_TEMP_MAX_AGE_HOURS`      | 过期 OCI 上传临时文件的最长滞留（小时） | `24`；`0` 禁用该项清理；非法 / 负数回落默认                        |
| `JIAN_TLS_ADDR`                        | 内置 HTTPS 监听地址                     | 为空表示不启用                                                     |
| `JIAN_TLS_CERT` / `JIAN_TLS_KEY`       | TLS 证书和私钥路径                      | 配置 `JIAN_TLS_ADDR` 时必填                                        |

派生路径固定为 `${JIAN_DATA_DIR}/jianartifact.db` 和 `${JIAN_DATA_DIR}/blobs`。启动会创建数据目录并执行 schema 迁移；不会在启动时扫描活动 blob；遗留 / 孤儿 blob 的定时清理由本实例按 `JIAN_BLOB_GC_INTERVAL` 独立运行；存储治理清理（隔离区空目录、终态操作与隔离元数据裁剪、过期 OCI 上传临时文件、代理缓存保留）由 `storage-cleanup` 作业按 `JIAN_STORAGE_CLEANUP_INTERVAL` 独立运行，两者按目录命名空间各管一块、互不重叠。以上作业**都只在第一个间隔到期后执行**（启动不清理、不扫描历史）。

**已废弃的环境变量**：`JIAN_REPLICATION_ROLE`、`JIAN_REPLICATION_PRIMARY_URL`、`JIAN_REPLICATION_RELAY_ENABLED` 自复制通道退役（FR-138）起不再被解析；旧环境文件里残留这些键不影响启动（未知键被忽略），可直接删除。

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

- **指标口径**：`jianartifact_protocol_requests_total{method,status,cache_result}`（制品协议请求完成计数，缓存结果取 `hit` / `miss` / `unknown`）、`jianartifact_publish_rejections_total{reason}`（发布被拒绝的累计次数，见下）、`jianartifact_scheduler_job_*{job}`（定时任务作业的执行 / 失败次数、是否运行中、最近执行时间与最近是否失败）、`jianartifact_runtime_*`（协程数、堆占用、累计 GC 次数）。
- **发布拒绝原因（闭集枚举）**：`jianartifact_publish_rejections_total{reason}` 的 `reason` 是**闭集枚举**，v1 只有 `quota` 一个取值——它同时覆盖「发布额度拒绝」与「仓库存储配额拒绝」这两类共用 429 `quota_exceeded` 口径的拒绝。**闭集之外的取值会被直接忽略**（标签基数受控）；**新增取值必须先改规格**（[`specs/0.11.0-storage-governance.md`](specs/0.11.0-storage-governance.md) §3.5）并同步测试与本文，不允许就地塞入自由字符串。该指标族即使尚无拒绝样本也输出 `# HELP` / `# TYPE`，样本行只在真的发生过拒绝后出现。
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

返回本进程**已注册**的周期作业（现状为 `blob-gc` 与 `storage-cleanup`）及状态：名称、间隔秒数、是否运行中、累计执行 / 失败次数、最近开始 / 结束时间、最近错误。要点：

- 清单由注册决定——**间隔 ≤ 0 被禁用的作业不注册，也不出现在清单里**，所以"某个作业没出现"等价于"该作业在本实例被禁用"（例如 `JIAN_STORAGE_CLEANUP_INTERVAL=0`）；
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
- `jianartifact backup link <包标识> [--ttl 30m] [--base https://对外地址]`：签发带时效的下载链接，供新机器直接拉取。`--base` 缺省时回退 `JIAN_PUBLIC_URL`；两者都为空则报错（不签相对链接）。有效期钳制在 1 分钟 ~ 24 小时。
- `jianartifact backup delete <包标识>`：删除包体与登记；被增量包引用的基线会被拒绝。
- `jianartifact backup import <归档路径> [--overwrite] [--deep] [--yes]`：校验备份包并暂存，**需重启服务后生效**。`--overwrite`：目标实例非空时必需（内置 `anonymous` 主体不计入"非空"）；`--deep`：逐 blob 比对内容摘要（耗时与包体积同阶）；`--yes`：交互式终端下跳过"确认覆盖"的二次确认（无 TTY 时 `--overwrite` 即视为已确认）。失败不会留下待生效标记。重启后启动日志会打印已应用的恢复，并保留 `pre-restore-<ts>/` 作为回滚退路。

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

### 5.2 备份

**首选做法：生成节点备份包**（FR-132 起）。它把"数据边界"打成一个可校验、可离线搬运的自包含归档：

```bash
jianartifact backup create --mode hot --label "每周例行"
jianartifact backup verify <包标识> --deep     # 可选的深度校验
```

包落在 `${JIAN_DATA_DIR}/backups/`，也可从管理台「迁移与搬迁 → 备份与搬迁」生成与下载。热备份不停服，适合例行备份；要求严格一致性时改用冻结窗口。

**包内不含密钥**，因此除包之外仍必须另行受保护地保存：生产显式配置的 `JIAN_JWT_SECRET`、`JIAN_MIGRATION_CREDENTIAL_KEY` 与服务环境文件（`deploy/.env`、systemd 环境文件）。这些密钥不随包迁移，新实例必须自行注入。

若不用备份包而手工兜底，需一致备份：

- `${JIAN_DATA_DIR}/jianartifact.db` 及必要的 `-wal`/`-shm`；
- `${JIAN_DATA_DIR}/blobs`。

建议停写或使用一致性快照；定期在非生产目录做恢复演练并抽样校验制品。

### 5.3 从节点备份包导入（恢复 / 搬迁，FR-137）

把一份节点备份包应用到本实例：校验 → 合并 blob → 暂存 db → 写待生效标记 → **重启替换**。导入不修改运行中的 SQLite，只写 `restore-staging/` 与 `restore.pending`；下次启动由 `ApplyPendingRestore` 在 `persistence.Open` 之前先做 `pre-restore-<ts>/` 回滚备份、再原子替换数据库。

三通道：

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
