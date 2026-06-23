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

### [auth]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| session_ttl_secs | Web 会话 / JWT 有效期（秒） | 3600 | JIANARTIFACT_AUTH_SESSION_TTL_SECS |
| login_max_failures | 触发锁定的连续失败次数 | 5 | JIANARTIFACT_AUTH_LOGIN_MAX_FAILURES |
| login_lockout_secs | 锁定时长（秒） | 900 | JIANARTIFACT_AUTH_LOGIN_LOCKOUT_SECS |

> 首启管理员引导（仅空库首次启动）：`JIANARTIFACT_ADMIN_USERNAME` 与 `JIANARTIFACT_ADMIN_PASSWORD`。建议仅用环境变量提供，不写入入库配置；未提供则系统生成随机口令打印到启动日志（见 ADR-0010）。

### [limits]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| max_artifact_size | 单个制品上传大小上限（超限返回 413） | 按需设定 | JIANARTIFACT_LIMITS_MAX_ARTIFACT_SIZE |

### [proxy]

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| upstream_timeout_secs | proxy 仓库回源上游的整体请求超时（秒），避免慢速上游拖垮代理 | 60 | JIANARTIFACT_PROXY_UPSTREAM_TIMEOUT_SECS |

### [upstream.&lt;name&gt;]（proxy 仓库上游，可配置多个）

| 键 | 含义 | 默认（取向） | 环境变量 |
|---|---|---|---|
| url | 上游地址 | — | JIANARTIFACT_UPSTREAM_&lt;NAME&gt;_URL |
| auth_ref | 上游凭据引用（真值走 env，不入库） | — | JIANARTIFACT_UPSTREAM_&lt;NAME&gt;_TOKEN |

## 3. 安全

- 真实凭据 / 口令不写入入库的 `config.toml`，走环境变量或不入库的本地配置。
- `config.toml`、`config.local.toml`、`.env`、数据目录、`*.db`、`*.log` 均不入库（见 `.gitignore`）。

> P2 配置项（如 S3 后端、七层防护阈值、WAF 规则、使用分析）在对应能力落地时补入本表，当前不预留占位。
