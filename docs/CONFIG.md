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
| port | 监听端口 | 9999 | JIANARTIFACT_SERVER_PORT |
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

> 默认关闭，启用与调阈值由运维显式承担。默认阈值**保守宽放**，**FR-51 新增的仓库维度与三档并发上限默认 0（不启用）**，不误杀正常包管理器批量并发拉取（如 CI 高频拉取）；调小阈值前请评估正常峰值。来源 IP 取**连接级地址**、**不采信 `X-Forwarded-For`**（伪造来源不绕过）；若部署在反向代理之后，限流按代理与本服务之间的连接 IP 计数（见 OPERATIONS）。并发上限以在途请求计数实现，请求结束（含出错 / panic）可靠归还、不泄漏。仅应用层（L7）多维限流与并发/连接上限；CC / WAF 属 FR-54~55，均未实现。按 TOML 嵌套节 `[protection.rate_limit]` 配置即可（本两层键以 TOML 为准，环境变量前缀仅对单层节名做嵌套映射）。

### [protection.ip_list]（IP 黑/白名单，P2 / FR-53 / ADR-0008）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| allow | 白名单（IP 或 CIDR 数组，IPv4 / IPv6 均可）；命中即豁免一切应用层防护（限流 / 封禁 / 异常统计），**优先级高于黑名单** | []（空 = 不启用） | （经 TOML 配置） |
| deny | 黑名单（IP 或 CIDR 数组）；命中即在进入业务前直接拒绝（403） | []（空 = 不启用） | （经 TOML 配置） |

> 名单项支持单 IP（如 `203.0.113.7`）与 CIDR 网段（如 `10.0.0.0/8`、`2001:db8::/32`）两种写法；非法项启动时记 WARN 并跳过、不阻断启动。按**连接级来源 IP** 判定，**不采信 `X-Forwarded-For`**（伪造来源不绕过）；反代部署时名单按代理与本服务之间的连接 IP 匹配（见 OPERATIONS）。白名单优先级最高（同一 IP 同时在黑白名单时按白名单放行）。按 TOML 嵌套节 `[protection.ip_list]` 配置即可（数组形态以 TOML 为准）。

### [protection.ban]（访问异常检测与自动封禁，P2 / FR-53 / ADR-0008）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用异常检测与自动封禁；关闭时不统计、不封禁、零额外开销 | false | （经 TOML 配置） |
| window_secs | 异常检测固定时间窗时长（秒）；每窗内独立统计异常信号、跨窗清零 | 60 | （经 TOML 配置） |
| threshold | 触发封禁的窗内异常信号阈值；单 IP 一窗内异常信号数达此值即自动封禁 | 100 | （经 TOML 配置） |
| duration_secs | 自动封禁时长（秒）；封禁期内该 IP 一律拒绝（403），到期自动解封 | 900 | （经 TOML 配置） |

> **异常信号**指响应为 4xx 客户端错误（含 401/403 鉴权失败与被限流拒绝的 429）；2xx/3xx 正常响应与 5xx 服务端错误**不计**（5xx 是本服务问题，不据此封禁来源）。默认关闭且阈值**保守宽放**，正常包管理器批量拉取偶发 404（探测制品是否存在）或鉴权重试不应触顶；调小阈值前请评估正常峰值。封禁状态**进程内内存维护**（时间窗，**重启即清**、不落 DB），白名单来源豁免封禁。来源 IP 取**连接级地址**、**不采信 `X-Forwarded-For`**（伪造来源不绕过）。仅应用层（L7）异常检测与封禁；体积型攻击交前置反向代理 / CDN / WAF（见 OPERATIONS）。按 TOML 嵌套节 `[protection.ban]` 配置即可。

### [protection.slowloris]（慢速攻击超时与通用请求体大小限制，P2 / FR-52 / ADR-0008）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用慢速攻击防护与通用请求体大小限制；关闭时中间件直接放行、零额外开销 | false | （经 TOML 配置） |
| body_read_timeout_secs | 请求体相邻数据块的**空闲超时**（秒）：两次到达数据块的最大间隔，超过即判为慢速 drip 并断开连接 | 30 | （经 TOML 配置） |
| header_timeout_secs | 等待请求体**首个数据块**的超时（秒）：发完头后迟迟不发体即判为慢速起始攻击并断开 | 30 | （经 TOML 配置） |
| max_body_bytes | 单个请求体**通用**大小上限（字节）；超过即返回 413。0 表示不启用该通用上限 | 0 | （经 TOML 配置） |

> 默认关闭，启用由运维显式承担。超时按「**块间空闲**」而非「整体时长」判定——只要客户端持续有数据到达就不触发，故对正常大文件流式上传（mvn deploy 大 jar、docker push 大层）友好，只切断长时间不发数据的慢速连接；档位默认保守（30 秒），调小前请评估正常网络抖动。`max_body_bytes` 区别于 `limits.max_artifact_size`（仅约束制品上传体）：本项是对**所有请求**请求体的兜底上限，带 `Content-Length` 时在进入业务前即拒 413（不读体），分块传输则边读边计、超限即断开；默认 0（不启用），启用时应设得**高于预期最大制品体**，仅作异常超大体的兜底拦截，避免误杀正常大制品上传。仅应用层（L7）：L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担（见 OPERATIONS）。按 TOML 嵌套节 `[protection.slowloris]` 配置即可。

### [protection.cc_challenge]（CC 挑战 / 工作量证明 PoW，P2 / FR-54 / ADR-0008）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用 CC 挑战；关闭时中间件直接放行、零开销 | false | （经 TOML 配置） |
| difficulty | PoW 难度（要求 `sha256(token + ":" + nonce)` 的二进制前导零比特数）；越高客户端求解开销越大 | 20 | （经 TOML 配置） |
| ttl_secs | 挑战令牌有效期（秒）；签发后超此时长的证明视为过期、须重新获取挑战 | 300 | （经 TOML 配置） |
| exempt_authenticated | 是否豁免已认证（Bearer / Basic / 会话）请求；避免误伤带凭据的包管理器 CLI | true | （经 TOML 配置） |

> ⚠️ **默认关闭，且默认仅在确有 CC 攻击时由运维显式开启**——正常包管理器 CLI（mvn / npm / docker / curl）**不会解工作量证明（PoW）**，启用后对匿名拉取无差别下发挑战会**直接打断正常匿名拉取**。故默认 `exempt_authenticated = true`，让带凭据的 CLI 豁免，挑战只面向**匿名可疑流量**；若你的部署允许匿名拉取公开仓库，开启 CC 挑战会影响这些匿名客户端，请谨慎评估。机制：对匿名请求下发挑战令牌（HMAC 无状态签名、绑定**连接级来源 IP** + 难度 + 签发时刻，不采信 `X-Forwarded-For`，换 IP 的证明不可复用），客户端须找到 `nonce` 使摘要前导零位数达 `difficulty`，再以请求头 `X-CC-Solution: <challenge_token>:<nonce>` 重发原请求；无 / 错误证明返回 `429`（错误码 `cc_challenge_required`，响应体含挑战参数）。难度越高刷流成本越高、正常单请求成本仍可忽略；调高 `difficulty` 前请评估目标客户端算力。仅应用层（L7）：L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担（见 OPERATIONS）。按 TOML 嵌套节 `[protection.cc_challenge]` 配置即可。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用 WAF 规则引擎；关闭或空规则集时中间件直接放行、零额外开销 | false | （经 TOML 配置） |
| rules | 有序规则数组（`[[protection.waf.rules]]`）；按声明顺序匹配、**首个命中生效** | 空 | （经 TOML 配置） |

每条规则（`[[protection.waf.rules]]`）字段：

| 键 | 含义 | 取值 |
|---|---|---|
| field | 匹配的请求属性字段 | `method` / `path` / `query` / `header` |
| header_name | 当 `field = "header"` 时指定的请求头名（大小写不敏感）；其余字段忽略 | 字符串（`header` 字段必填） |
| pattern | 匹配模式串，按 `match_type` 解释 | 字符串 |
| match_type | 匹配类型 | `literal`（子串包含）/ `wildcard`（`*` 任意多字符、`?` 任意单字符，整体匹配）/ `regex`（正则子串搜索） |
| action | 命中后的动作 | `block`（拒 403）/ `allow`（放行并短路后续规则） |

> 默认**空规则集 + 关闭**（不影响现有行为、不误杀正常包管理器请求），启用与规则由运维显式承担。规则在**启动期编译一次**（正则经 `regex-lite` 预编译、通配转译为锚定正则）；**字段 / 匹配类型 / 动作非法或正则无法编译的规则在启动时记 WARN 跳过、不阻断启动**，其余规则照常生效——配置后请检查启动日志确认无规则被跳过。匹配**按声明顺序、首个命中生效**：可把对合法模式的 `allow` 规则**排在前面**给其开豁免口子，再用 `block` 规则兜底拦截。**误杀提示**：`literal` 走子串包含、`regex` 走子串搜索——过宽的 `pattern`（如对 `path` 写 `/` 或对 `query` 写常见参数名）会误伤正常请求；`block` 规则上线前建议先以 `allow` 或在测试环境验证其只命中目标请求。WAF 按请求属性（method/path/query/header）匹配，**与来源 IP 无关、不采信 `X-Forwarded-For`**。仅应用层（L7）：L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担（见 OPERATIONS）。按 TOML 嵌套节 `[protection.waf]` 与 `[[protection.waf.rules]]` 配置即可。

### [protection.alerts]（防护监控与阈值告警，P2 / FR-56 / ADR-0017）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用阈值告警；关闭时不评估、不落库、零额外开销 | false | （经 TOML 配置） |
| window_secs | 告警评估固定时间窗时长（秒）；每窗内独立统计各维度计数、跨窗清零 | 300 | （经 TOML 配置） |
| rate_limit_warn_threshold | 限流被拒窗内告警阈值（一窗内限流被拒次数达此值即告警） | 1000 | （经 TOML 配置） |
| ban_warn_threshold | 自动封禁触发窗内告警阈值 | 50 | （经 TOML 配置） |
| cc_challenge_fail_warn_threshold | CC 挑战证明校验失败窗内告警阈值 | 1000 | （经 TOML 配置） |
| waf_block_warn_threshold | WAF 阻断窗内告警阈值 | 500 | （经 TOML 配置） |
| slowloris_warn_threshold | 慢速攻击超时 / 截断拒绝窗内告警阈值 | 200 | （经 TOML 配置） |
| max_rows | 告警明细行数硬上限（超限删最旧行，兜底防撑爆 SQLite） | 100000 | （经 TOML 配置） |

> 默认**关闭**（避免无人值守时刷告警），阈值默认**保守宽放**（避免正常高频访问 / 合法批量拉取误报），启用与阈值由运维按自身流量基线显式调优。机制：在固定时间窗内按维度（限流 / 自动封禁 / CC 挑战失败 / WAF 阻断 / 慢速超时）累加防护事件计数，单维度窗内计数达对应阈值即按严重度记**中文分级日志**（窗内观测值达阈值 5 倍升级为 ERROR、否则 WARN）并**异步落 SQLite**（`protection_alerts` 表）；同一维度**窗内去抖**（一窗只告警一次、不刷屏），跨窗计数清零后可再告警。各维度的连续计数同时经 `GET /metrics`（见 `[observability.metrics]`）暴露为低基数指标供外部 Prometheus 抓取并自定义告警规则。**告警是本机内部数据：只落本地、不外发、不内置外发型通知（Webhook / 邮件等若未来要做须另写 ADR）**；运维经 `GET /api/v1/protection/status`（仅 Admin）查看防护健康快照、经 `GET /api/v1/protection/alerts`（仅 Admin）分页查询告警历史。仅应用层（L7）。按 TOML 嵌套节 `[protection.alerts]` 配置即可。

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

### [network.proxy]（出站网络代理，P2 / FR-84 / ADR-0020，运行时可编辑见 FR-88 / ADR-0022）

> 统一注入全部出站 reqwest 客户端（proxy 回源 / Nexus 迁移 / 漏洞库镜像 / OIDC / 在线更新）。三键默认全空：不显式注入代理，保持 reqwest 既有行为不变（含其默认 honor 系统 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境变量）。**任一键给值即以本配置为真源**（注入即关闭 reqwest 的自动系统代理探测，配置压过系统环境）。代理 URL 可含 `user:pass@` 凭据，凭据**不入库、不进日志 / 错误信息**——建议含凭据的代理 URL 仅经环境变量提供，不写入入库 TOML。
>
> **运行时可编辑（FR-88）**：本节为启动期初值；Admin 可经控制台「设置」页或 `PATCH /api/v1/settings` 在线改代理，**即时生效、无须重启**。运行时改动只入内存热替换槽、**不写回本文件**，重启回落本节 + env——需持久代理仍应写本节 / env。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| http | HTTP 出站代理 URL（如 `http://proxy.internal:8080`） | 空（不注入） | JIANARTIFACT_NETWORK_PROXY_HTTP |
| https | HTTPS 出站代理 URL | 空（不注入） | JIANARTIFACT_NETWORK_PROXY_HTTPS |
| no_proxy | 直连绕过列表（逗号分隔的主机 / 域 / CIDR） | 空 | JIANARTIFACT_NETWORK_PROXY_NO_PROXY |

### [update]（在线更新，P2 / FR-85 / ADR-0021，运行时可编辑见 FR-88 / ADR-0022）

> 管理员手动触发的完整自更新：查 GitHub 最新稳定 Release、与当前版本比对，下载对应资产、校验 sha256、原子替换二进制并自动重启。**出站默认关闭**（`enabled=false` 时检查 / 应用端点一律拒绝、不联网），须运维显式开启。出站经 `[network.proxy]`（FR-84）注入的代理。
>
> **运行时可编辑（FR-88）**：`enabled` / `repo` / `api_base_url` / `restart_mode` / `channel` / `token` 可经控制台「设置」页或 `PATCH /api/v1/settings` 在线改、**即时生效、无须重启**；运行时改动只入内存槽、**不写回本文件**（token 同样只入内存、不回显），重启回落本节 + env。

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| enabled | 是否启用在线更新（出站开关）；关闭时检查 / 应用端点一律拒绝、不联网 | false | JIANARTIFACT_UPDATE_ENABLED |
| repo | 仓库源（`owner/repo` 形式），自更新从此仓库取 Release | wcpe/JianArtifact | JIANARTIFACT_UPDATE_REPO |
| api_base_url | GitHub API 基址（可配，便于测试 / 镜像） | https://api.github.com | JIANARTIFACT_UPDATE_API_BASE_URL |
| restart_mode | 重启模式：`self`（自拉起新进程）/ `exit`（仅退出交外部进程管理器 systemd / docker 重启） | self | JIANARTIFACT_UPDATE_RESTART_MODE |
| channel | 更新通道（FR-89）：`stable`（仅最新稳定版）/ `prerelease`（含预发布，取最新一条非草稿 release） | stable | JIANARTIFACT_UPDATE_CHANNEL |
| download_timeout_secs | 资产下载整体超时（秒） | 300 | JIANARTIFACT_UPDATE_DOWNLOAD_TIMEOUT_SECS |

> `token` 是密钥（私有仓库可选）：真源为环境变量 `JIANARTIFACT_UPDATE_TOKEN`，**绝不入库、不进日志、序列化不回显**；公开仓库免凭据。仅做 sha256 完整性校验、校验通过才替换（不做签名验签）；校验失败即拒绝替换、删临时文件、保留旧二进制，进程续以旧版运行。

## 3. 安全

- 真实凭据 / 口令不写入入库的 `config.toml`，走环境变量或不入库的本地配置。
- `config.toml`、`config.local.toml`、`.env`、数据目录、`*.db`、`*.log` 均不入库（见 `.gitignore`）。

> 其余 P2 配置项（如七层防护阈值、WAF 规则、使用分析）在对应能力落地时补入本表，当前不预留占位。
