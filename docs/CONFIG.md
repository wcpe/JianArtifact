# 配置参考：JianArtifact

> 运行期配置的单一参考。配置由单个 TOML 文件加载，环境变量可覆盖。配置项随实现演进时，本文与 `config.example.toml` 同步更新。

## 1. 加载与覆盖

- 配置文件：默认读取部署目录下的 `config.toml`（可经启动参数指定路径）。仓库仅提供 `config.example.toml` 占位示例，真实 `config.toml` 不入库。
- 环境变量覆盖：以 `JIANARTIFACT_` 为前缀的环境变量优先于 TOML 文件中的同名项。
- 映射约定：TOML 的 `[节] 键` 映射为大写、下划线连接的 `JIANARTIFACT_<节>_<键>`。例如 `[server] listen_addr` ↔ `JIANARTIFACT_SERVER_LISTEN_ADDR`。
- 敏感项（口令、上游凭据等）建议走环境变量注入，不写入入库文件。

## 2. 配置项清单

> 下表为配置项的参考结构与默认取向；具体键名与默认值以实现与 `config.example.toml` 为准，新增 / 变更配置项时同步本表。

### [server]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| listen_addr | 监听地址 | 127.0.0.1 | JIANARTIFACT_SERVER_LISTEN_ADDR |
| port | 监听端口 | 8080 | JIANARTIFACT_SERVER_PORT |
| public_base_url | 对外基础 URL（用于生成链接） | 按监听推断 | JIANARTIFACT_SERVER_PUBLIC_BASE_URL |

### [data]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| data_dir | 数据目录（SQLite 与 blob 根） | ./data | JIANARTIFACT_DATA_DATA_DIR |
| blobs_dir | blob 存储子目录 | data_dir 下的 blobs | JIANARTIFACT_DATA_BLOBS_DIR |

### [data.storage]（blob 后端选择，FR-30 / ADR-0014）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| backend | blob 存储后端：`fs`（本地文件系统，默认）/ `s3`（S3 兼容对象存储） | fs | JIANARTIFACT_DATA_STORAGE_BACKEND |

> `backend = "s3"` 需使用启用 `s3` 编译特性的构建，否则启动直接报错退出（不静默回退本地）。S3 为可选 opt-in 后端，启用即引入外部对象存储运行时依赖，详见 docs/OPERATIONS.md。本地文件系统仍是默认与开箱即用形态。

### [data.storage.s3]（仅 backend = "s3" 时使用）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| endpoint | S3 端点 URL（兼容 MinIO 等自建网关；指向 AWS 时可省略由 region 推断） | 空（由 region 推断） | JIANARTIFACT_DATA_STORAGE_S3_ENDPOINT |
| region | 区域（如 us-east-1；MinIO 等可填占位值） | — | JIANARTIFACT_DATA_STORAGE_S3_REGION |
| bucket | 存储桶名 | — | JIANARTIFACT_DATA_STORAGE_S3_BUCKET |
| prefix | 对象 key 前缀（与 sha256 内容寻址键拼接） | 空 | JIANARTIFACT_DATA_STORAGE_S3_PREFIX |
| path_style | path-style 寻址（MinIO 等自建网关需 true） | true | JIANARTIFACT_DATA_STORAGE_S3_PATH_STYLE |

> S3 凭据（access key / secret key）**不在上表**：其真源沿用 AWS SDK 标准环境变量（`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` 等），绝不写入入库配置、绝不进日志或 DB 明文（ADR-0014 §7）。

### [auth]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| session_ttl_secs | Web 会话 / JWT 有效期（秒） | 3600 | JIANARTIFACT_AUTH_SESSION_TTL_SECS |
| login_max_failures | 触发锁定的连续失败次数 | 5 | JIANARTIFACT_AUTH_LOGIN_MAX_FAILURES |
| login_lockout_secs | 锁定时长（秒） | 900 | JIANARTIFACT_AUTH_LOGIN_LOCKOUT_SECS |

> 首启管理员引导（仅空库首次启动）：`JIANARTIFACT_ADMIN_USERNAME` 与 `JIANARTIFACT_ADMIN_PASSWORD`。建议仅用环境变量提供，不写入入库配置；未提供则系统生成随机口令打印到启动日志（见 ADR-0010）。

### [auth.oidc]（OIDC 认证集成，P2 / FR-34 / ADR-0016）

可选；配置后才启用 OIDC 登录端点（未配置即不存在）。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| issuer | IdP 签发者标识（issuer），同时用作 discovery 基址与 ID Token `iss` 校验值 | 必填 | JIANARTIFACT_AUTH_OIDC_ISSUER |
| client_id | OIDC 客户端 ID | 必填 | JIANARTIFACT_AUTH_OIDC_CLIENT_ID |
| client_secret | 客户端密钥（敏感） | 必填 | JIANARTIFACT_AUTH_OIDC_CLIENT_SECRET |
| redirect_uri | 回调地址（须与 IdP 注册的 redirect_uri 完全一致） | 必填 | JIANARTIFACT_AUTH_OIDC_REDIRECT_URI |
| auto_provision | 即时开通（JIT）：无对应本地用户时是否自动建用户（默认角色固定 User，绝不 Admin） | false（关闭） | JIANARTIFACT_AUTH_OIDC_AUTO_PROVISION |

> `client_secret` 是密钥：真源在 env / 配置，**绝不入库、不进日志、不进 DB 明文**；建议仅经环境变量 `JIANARTIFACT_AUTH_OIDC_CLIENT_SECRET` 提供，不写入入库 TOML。`auto_provision` 默认关闭：外部认证成功但本地无对应用户时拒绝登录（守不自助注册红线，ADR-0010）；显式开启时即时建用户、默认角色 `User`，到 `Admin` 的提升只能由现有管理员显式操作。

### [auth.ldap]（LDAP 认证集成，P2 / FR-35 / ADR-0016）

可选；配置后才启用 LDAP 登录（未配置即不存在）。仅参与口令型登录（Web 表单 / Basic Auth），bind 校验成功后收敛为本地会话。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| url | 目录服务 URL（`ldaps://host:636` 或 `ldap://host:389`） | 必填 | JIANARTIFACT_AUTH_LDAP_URL |
| bind_dn | 搜索绑定 DN（服务账号），连接后先用其查用户 DN | 必填 | JIANARTIFACT_AUTH_LDAP_BIND_DN |
| bind_password | 搜索绑定口令（敏感） | 必填 | JIANARTIFACT_AUTH_LDAP_BIND_PASSWORD |
| user_search_base | 用户搜索基准 DN（如 `ou=people,dc=example,dc=org`） | 必填 | JIANARTIFACT_AUTH_LDAP_USER_SEARCH_BASE |
| user_filter | 用户搜索过滤模板，含 `{username}` 占位符（按 RFC 4515 转义防注入） | `(uid={username})` | JIANARTIFACT_AUTH_LDAP_USER_FILTER |
| username_attr | 取作建议用户名的属性名（如 `uid` / `cn` / `sAMAccountName`） | `uid` | JIANARTIFACT_AUTH_LDAP_USERNAME_ATTR |
| starttls | 是否在明文端口上经 StartTLS 协商升级 TLS | false（关闭） | JIANARTIFACT_AUTH_LDAP_STARTTLS |
| allow_insecure | 是否允许明文 `ldap://`（无 TLS）；仅可信内网显式开启 | false（关闭） | JIANARTIFACT_AUTH_LDAP_ALLOW_INSECURE |
| conn_timeout_secs | 连接超时（秒） | 10 | JIANARTIFACT_AUTH_LDAP_CONN_TIMEOUT_SECS |
| auto_provision | 即时开通（JIT）：无对应本地用户时是否自动建用户（默认角色固定 User，绝不 Admin） | false（关闭） | JIANARTIFACT_AUTH_LDAP_AUTO_PROVISION |

> `bind_password` 是密钥：真源在 env / 配置，**绝不入库、不进日志、不进 DB 明文**；建议仅经环境变量 `JIANARTIFACT_AUTH_LDAP_BIND_PASSWORD` 提供，不写入入库 TOML。连接默认走 LDAPS / StartTLS（TLS 由 rustls 提供，不引 openssl）；`allow_insecure` 默认关闭，明文 `ldap://` 仅在可信内网显式开启。`auto_provision` 默认关闭，语义与 `[auth.oidc]` 一致（守 ADR-0010）。

### [limits]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| max_artifact_size | 单个制品上传大小上限（超限返回 413） | 按需设定 | JIANARTIFACT_LIMITS_MAX_ARTIFACT_SIZE |

### [proxy]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| upstream_timeout_secs | proxy 仓库回源上游的整体请求超时（秒），避免慢速上游拖垮代理 | 60 | JIANARTIFACT_PROXY_UPSTREAM_TIMEOUT_SECS |

### [observability.audit]（审计日志，P2 / FR-31）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| retention_days | 审计日志保留天数；后台任务按此周期删除更早的审计行 | 90 | （经 TOML 配置） |
| max_rows | 审计日志行数硬上限；超限删最旧行，兜底防止撑爆 SQLite | 1000000 | （经 TOML 配置） |

> 审计保留期不是敏感项，按 TOML 嵌套节 `[observability.audit]` 配置即可（环境变量前缀仅对单层节名做嵌套映射，本两层键以 TOML 为准）。审计日志数据本机内部、默认不外发（ADR-0009 / ADR-0015）。

### [observability.usage]（使用分析采集，P2 / FR-57）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| detail_enabled | 是否记录逐条访问 / 下载明细（`usage_events`）；默认关闭，仅采集聚合计数 | false | （经 TOML 配置） |
| max_detail_rows | 明细行数硬上限；超限删最旧行，兜底防止明细撑爆 SQLite | 1000000 | （经 TOML 配置） |

> 聚合计数（`usage_stats`）始终采集（开销小、量级可控）；明细默认关闭，开启后量级由 `max_detail_rows` 兜底裁剪。统计数据本机内部、**默认不主动外发、不向外部遥测 phone-home**；不提供任何外部导出 / 上报开关（本批不做导出，ADR-0009）。本两层键以 TOML `[observability.usage]` 为准（环境变量前缀仅对单层节名做嵌套映射）。
### [observability.metrics]（Prometheus 指标端点，P2 / FR-32）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用 `GET /metrics` 端点；关闭后端点返回 404 且不安装进程内 recorder | true | （经 TOML 配置） |
| allow_anonymous | 是否允许匿名抓取 `/metrics`；关闭时要求认证且仅 Admin 可访问 | false | （经 TOML 配置） |

> 指标为进程内自采（pull 模型），仅在 `/metrics` 被抓取时渲染，不向任何外部端点 push / remote-write，数据本机内部、默认不外发（ADR-0009 / ADR-0015）。`allow_anonymous=true` 会把端点对匿名开放——**仅在把端点限定在内网 / 反向代理 / 防火墙之后时启用**，否则运行画像可能被外部探知（见 OPERATIONS 风险说明）。按 TOML 嵌套节 `[observability.metrics]` 配置即可（本两层键以 TOML 为准）。

### [protection.rate_limit]（多维速率限制与并发上限，P2 / FR-33 + FR-51 / ADR-0008）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用速率限制；关闭时中间件直接放行、零计数开销 | false | （经 TOML 配置） |
| window_secs | 固定时间窗时长（秒）；每窗内独立计数、跨窗清零 | 60 | （经 TOML 配置） |
| ip_max_requests | 单 IP 每窗请求数上限；超过即对该 IP 返回 429 | 1200 | （经 TOML 配置） |
| identity_max_requests | 单身份（用户及其所有 Token / 会话）每窗请求数上限；超过即对该主体返回 429。即 FR-51 的「用户维度」 | 2400 | （经 TOML 配置） |
| repo_max_requests | 单仓库每窗请求数上限（按格式路径首段仓库名计数）；超过即对该仓库返回 429。0 表示不启用该维度 | 0 | （经 TOML 配置） |
| ip_max_concurrent | 单 IP 在途并发请求数上限；超过即返回 429。0 表示不限并发 | 0 | （经 TOML 配置） |
| user_max_concurrent | 单用户在途并发请求数上限；超过即返回 429。0 表示不限并发 | 0 | （经 TOML 配置） |
| repo_max_concurrent | 单仓库在途并发请求数上限；超过即返回 429。0 表示不限并发 | 0 | （经 TOML 配置） |

> 默认关闭，启用与调阈值由运维显式承担。默认阈值**保守宽放**，**FR-51 新增的仓库维度与三档并发上限默认 0（不启用）**，不误杀正常包管理器批量并发拉取（如 CI 高频拉取）；调小阈值前请评估正常峰值。来源 IP 取**连接级地址**、**不采信 `X-Forwarded-For`**（伪造来源不绕过）；若部署在反向代理之后，限流按代理与本服务之间的连接 IP 计数（见 OPERATIONS）。并发上限以在途请求计数实现，请求结束（含出错 / panic）可靠归还、不泄漏。仅应用层（L7）多维限流与并发/连接上限；慢速 / 封禁 / CC / WAF 属 FR-52~56，均未实现。按 TOML 嵌套节 `[protection.rate_limit]` 配置即可（本两层键以 TOML 为准，环境变量前缀仅对单层节名做嵌套映射）。

### [upstream.&lt;name&gt;]（proxy 仓库上游，可配置多个）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| url | 上游地址 | — | JIANARTIFACT_UPSTREAM_&lt;NAME&gt;_URL |
| auth_ref | 上游凭据引用（真值走 env，不入库） | — | JIANARTIFACT_UPSTREAM_&lt;NAME&gt;_TOKEN |

### [vuln]（漏洞库离线镜像，FR-70 / ADR-0012）

> 默认关闭：镜像需主动联网拉取公开漏洞数据集到本机，由运维显式开启。下载公开数据集整体镜像（按生态 `all.zip`），**不把本机制品坐标逐包外发**。本批仅镜像/落库，制品坐标匹配标记（FR-71）尚未实现。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用漏洞库离线镜像 | false | JIANARTIFACT_VULN_ENABLED |
| source_base_url | 数据源基址（按生态取 `{base}/{ecosystem}/all.zip`） | https://osv-vulnerabilities.storage.googleapis.com | JIANARTIFACT_VULN_SOURCE_BASE_URL |
| ecosystems | 镜像的生态列表（如 ["Maven","npm"]） | 空（不镜像任何生态） | JIANARTIFACT_VULN_ECOSYSTEMS |
| refresh_interval_secs | 刷新周期（秒） | 86400 | JIANARTIFACT_VULN_REFRESH_INTERVAL_SECS |
| download_timeout_secs | 单次镜像下载整体超时（秒） | 600 | JIANARTIFACT_VULN_DOWNLOAD_TIMEOUT_SECS |

## 3. 安全

- 真实凭据 / 口令不写入入库的 `config.toml`，走环境变量或不入库的本地配置。
- `config.toml`、`config.local.toml`、`.env`、数据目录、`*.db`、`*.log` 均不入库（见 `.gitignore`）。

> 其余 P2 配置项（如七层防护阈值、WAF 规则、使用分析）在对应能力落地时补入本表，当前不预留占位。
