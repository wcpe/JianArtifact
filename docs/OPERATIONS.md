# 运维手册：JianArtifact

> 本文只记录当前可执行的部署、升级、备份、恢复、回滚和排障流程。版本真源是根目录 `VERSION`；当前发布版本为 `0.7.1`，`0.8.0` 尚未发布。级联复制与邻接控制台已在工作区实现，但未完成整期真实验收前不得宣称已发布。

## 1. 运行与配置

### 1.1 当前支持的基础配置

配置通过环境变量、部署 Secret 或 systemd 环境文件注入；真实环境文件不入库。

| 变量                             | 作用                                       | 默认/约束                                                            |
| -------------------------------- | ------------------------------------------ | -------------------------------------------------------------------- |
| `JIAN_HTTP_ADDR`                 | HTTP 监听地址                              | `:8080`                                                              |
| `JIAN_DATA_DIR`                  | SQLite 与 blob 数据根目录                  | `./data`；生产必须使用持久化绝对路径                                 |
| `JIAN_JWT_SECRET`                | JWT(HS256) 签名密钥                        | 生产必填、强随机、不得打印                                           |
| `JIAN_MIGRATION_CREDENTIAL_KEY`  | 在线迁移凭据 AES-256-GCM 密钥              | Base64 编码 32 字节；生产应显式固定                                  |
| `JIAN_UPSTREAM_TIMEOUT`          | proxy 回源整体超时（秒）                   | `30`                                                                 |
| `JIAN_ENABLED_FORMATS`           | 启用的协议格式                             | 缺省 `raw,maven,npm`；显式空值关闭全部                               |
| `JIAN_PUBLIC_URL`                | 对外基础 URL                               | 节点本地配置，不参与复制                                             |
| `JIAN_REPLICATION_ROLE`          | 复制角色                                   | `disabled`、`primary` 或 `standby`；缺省 `disabled`                  |
| `JIAN_REPLICATION_PRIMARY_URL`   | standby 的直接父节点地址（历史变量名）     | 当前代码中仅 standby 必填，必须是不含路径/查询/凭据的 HTTP(S) 根地址 |
| `JIAN_REPLICATION_RELAY_ENABLED` | standby 是否向多个直接 child 提供 relay    | `true` 开启；缺省/非法值为 `false`                                   |
| `JIAN_SYNC_INTERVAL`             | standby 轮询间隔（秒）                     | `5`                                                                  |
| `JIAN_BLOB_GC_INTERVAL`          | primary 清理失败写入遗留 blob 的间隔（秒） | `86400`；`0` 禁用                                                    |
| `JIAN_TLS_ADDR`                  | 内置 HTTPS 监听地址                        | 为空表示不启用                                                       |
| `JIAN_TLS_CERT` / `JIAN_TLS_KEY` | TLS 证书和私钥路径                         | 配置 `JIAN_TLS_ADDR` 时必填                                          |

派生路径固定为 `${JIAN_DATA_DIR}/jianartifact.db` 和 `${JIAN_DATA_DIR}/blobs`。启动会创建数据目录并执行 schema 迁移；不会在启动时扫描活动 blob。只有 primary 运行孤立 blob 定时清理，standby 和 disabled 不运行该任务。

### 1.2 当前已支持的主备级联复制

当前代码是静态一主多级联树模型：

- primary 是唯一业务写节点，只提供节点专属凭据保护的 v2 capabilities、pull 和 blob GET；不发起出站同步。
- standby 可读、可登录、可健康检查；业务、管理和协议写入口返回 `503 standby_read_only`。内部复制应用、水位、同步历史和接收审计仍可写入本地。
- standby 配置 `JIAN_REPLICATION_PRIMARY_URL` 后，导入直接父节点复制凭据，启动轮询并从该父节点按根源 stream watermark 续拉；默认间隔为 5 秒。
- relay standby 开启 `JIAN_REPLICATION_RELAY_ENABLED=true` 后，可通过逐跳凭据为任意多个直接 child 提供已完整应用的 v2 record/blob；record 与 blob 都受整轮确认的 `forwardable_seq` 限制，relay 不生成本地业务 outbox。
- 复制固定使用 v2、GET-only 和 HTTP/1.1；完整 operation、缺失 blob 和校验成功后才推进 watermark。
- 角色、来源和复制凭据不是 Web 运行时配置。`JIAN_SYNC_PEER_URL` 是旧配置，当前主备模式不读取、不参与调度。

### 1.3 级联树运行边界

当前模型是有向级联树：根 primary 唯一可写；每个非根节点只有一个直接上级；每个节点可挂多个直接下级；下级主动向直接上级 GET 拉取。

具备 relay 能力的 standby 才能向直接下级提供已经完整接收、校验并原子应用的根源记录。中继不生成本地业务 `repl_change`、不改变根源 seq/operationId、不允许业务写入。

当前实现以 `replication_relay_record` 保存已经完整应用的上游原始 record，以 `replication_relay_frontier` 限制下游可见连续前缀，并以 `streamGeneration/sourceNode/sourceSeq` 作为下游续拉身份；同 stream/seq 的载荷不可改写。父流 generation、watermark 和 valid 围栏原子持久化，发现代次变化或“非零 watermark 无 generation”时关闭 relay 出口并要求清理旧水位后重新配对。relay blob 采用保守保留，防止慢 child 在父节点先处理删除后无法回补。每个节点只保存一个直接父配置，父节点只通过各自的逐跳凭据认识直接 child。

0.8.0 开发数据库首次应用迁移 0033 时会丢弃旧版未经不可变 provenance 验证的 relay inbox/frontier 和父边 watermark，并自动从直接父节点水位 0 重建；这是未发布开发版的一次性安全迁移，不适用于已发布版本的常规数据保留承诺。standby 崩溃恢复只处理标记为 `received` 且带原始 receipt 身份的 intent，旧角色本地 intent 不会被自动执行。

当前仍不支持自动选主、自动换父、运行时拓扑编辑、跨节点主机监控和双主合并；人工提升/重新挂接必须按规格先围栏、核验并重新建立 stream。真实多进程级联、浏览器和发布门仍是 0.8.0 整期验收项。详细边界见 [`specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md)。

### 1.4 当前主备凭据配对

当前已支持的节点专属凭据流程如下；级联按每条直接父子边重复执行。

1. 在 standby 执行 `jianartifact replication credential node-id`，初始化稳定 node ID。
2. 在直接父节点执行 `jianartifact replication credential create --node-id <standby-node-id>`，令牌只在本地终端显示一次；relay standby 也可执行该命令。
3. 通过受保护通道交付令牌；在 standby 执行 `jianartifact replication credential import --credential-id <credential-id>`，令牌只能从标准输入提供。
4. 在 standby 执行 `jianartifact replication credential verify`，确认能力协商成功后再启动服务同步。
5. 轮换时由直接父节点执行 `rotate`，standby 导入并 `verify` 新凭据，确认成功后由该直接父节点执行 `revoke` 撤销旧凭据。

primary 只保存加盐不可逆摘要；standby 使用数据目录独立的 `replication-credential.key` 密封保存。令牌、密文、密钥、Authorization 头和命令参数不得写入日志、审计、工单、截图或备份明文。

### 1.5 对外访问安全

#### Host 白名单

管理端设置中的允许访问域名是节点本地配置。配置后所有管理 API、协议和静态页面的 Host 必须命中白名单；本机回环放行。回环只按服务端看到的 TCP 对端地址判定，`Host: localhost`、`Host: 127.0.0.1` 或转发头都不能把外部连接声明为回环。命中显式白名单的非 TLS 协议请求是 CDN HTTP 回源兼容路径，不构成来源认证，生产必须同时启用回源 Token 或改用 HTTPS 回源。同机反向代理或隧道若以回环连接转发外部流量，应用看到的对端将是该代理；此时不得把回环例外当作来源认证，应让代理连接服务的非回环监听地址，并配合防火墙限制来源。白名单不能替代防火墙，生产仍应限制源站只接受 CDN/反向代理回源流量。

#### 内置 TLS

配置 `JIAN_TLS_ADDR`、`JIAN_TLS_CERT` 和 `JIAN_TLS_KEY` 后，服务在 HTTP 之外启动 HTTPS 监听，共用同一 handler。证书缺失或格式错误必须使进程整体启动失败，不得静默退回明文。CDN/反向代理回源应使用 HTTPS。

#### 回源 Token

安全防护设置中的回源 Token 是节点本地配置，默认关闭。启用后所有非回环请求必须携带配置的请求头和值；直连源站即使伪造 Host 也应被拒绝。重新生成后旧 Token 立即失效。

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

`deploy/deploy.sh` 或 `deploy/remote-ssh.sh` 会把二进制放入版本目录，原子切换 `current`，探活失败时回滚。systemd 环境文件必须使用服务实际数据目录的绝对路径；不要直接在错误工作目录执行裸二进制。

```bash
bash deploy/remote-ssh.sh setup-key
bash deploy/remote-ssh.sh deploy
bash deploy/remote-ssh.sh health
```

重启、查进程和回滚必须限定部署用户与服务实例，禁止使用无范围的 `pkill` 或清理命令影响同机其他服务。

### 2.3 Helm / Kubernetes

`deploy/helm/` 和 `deploy/k8s/` 当前是单实例、RWO 持久卷和 `Recreate` 策略；它们不是多副本 HA 模板。生产 Secret 通过 Secret 管理系统注入，不在清单中提交真实值。探针使用 `/healthz` 和 `/readyz`。

### 2.4 多节点测试隔离

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

包内**仅含数据，不含任何密钥或节点本地配置**：排除复制凭据密钥、JWT 密钥、迁移凭据密钥、环境文件、`*.db-wal`、`*.db-shm`、`blobs/tmp/`、`blobs/quarantine/`。因此新机器导入后必须自行配置密钥与对外地址。

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
- `jianartifact admin emit-asset-times`：为存量资产重新登记带时间的复制变更。
- `jianartifact replication status/start/stop`：查看或控制当前支持的 standby 同步调度。
- `jianartifact replication backfill`：仅受控提升后的 primary 可执行，重建新的根源复制历史。
- `jianartifact replication credential node-id/create/import/verify/revoke/rotate`：管理当前支持的主备凭据；级联实现后按直接父子边使用。
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
5. 检查 `/readyz`、管理员登录、关键仓库读取和制品抽样。

SQLite + blob 是一个数据边界，默认不做滚动多副本升级。

### 5.2 备份

**首选做法：生成节点备份包**（FR-132 起）。它把"数据边界"打成一个可校验、可离线搬运的自包含归档：

```bash
jianartifact backup create --mode hot --label "每周例行"
jianartifact backup verify <包标识> --deep     # 可选的深度校验
```

包落在 `${JIAN_DATA_DIR}/backups/`，也可从管理台「迁移与搬迁 → 备份与搬迁」生成与下载。热备份不停服，适合例行备份；要求严格一致性时改用冻结窗口。

**包内不含密钥**，因此除包之外仍必须另行受保护地保存：

- 生产显式配置的 JWT 密钥、迁移凭据密钥与服务环境文件；
- 复制凭据密钥（若该节点仍参与复制通道）与其密封凭据必须处于同一受保护恢复边界，不能复制给另一节点，也不得跨节点搬运。

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

### 5.4 当前主备人工提升

> **即将退役**：搬迁与备份已改为以节点备份包为单位（见 §3.2/§3.3 与 `docs/adr/0027`）。下列复制通道的提升流程仅在复制通道尚未退役的过渡期适用，新部署请优先使用备份包搬迁。
> 二者不可混用：备份包内含 SQLite 快照与 blob，**不含复制凭据密钥**；用备份包搬到新机后不需要、也不应再参与原复制通道。

仍在使用复制通道时，提升流程仍是人工单主：

1. 停止或网络围栏旧 primary。
2. 在 standby 核验 watermark、接收审计、制品计数和抽样 SHA-256。
3. 将 standby 改为 primary，移除 `JIAN_REPLICATION_PRIMARY_URL`，执行受控 `replication backfill`。
4. 重启并验证健康、读写和审计后再开放流量。
5. 旧 primary 回接时使用全新 standby 数据目录重新同步，禁止直接复用旧 SQLite/blob。

中间 relay 的提升/重新挂接仍必须人工围栏和生成新的 stream generation；本期不做自动换父。

## 6. 回滚

- 二进制/镜像回滚到上一个已知良好版本；systemd 回切 `current`，Compose 回切镜像 tag。
- 若新版本执行了不可逆 schema 迁移，必须同时恢复对应数据备份，不能只回滚代码。
- 复制拓扑或凭据变更回滚前先停止相关同步边，避免旧 watermark 或旧凭据继续写入新流。

## 7. CI、发布与排障

### 7.1 质量门与发布

本地质量入口是 Windows 原生 `make check` / `scripts/check.ps1`；应覆盖前端格式、类型、lint、测试、构建、Go vet/lint/race/vuln、契约和静态构建。`0.8.0` 在版本提交、tag、远程 CI 和 Release 完成前都不得标正式交付。

开发预览和正式发布使用不同版本标识；发布资产必须有对应校验和。交叉编译成功不能代替原生运行或真实服务验收。

### 7.2 常见问题

- **启动失败**：检查数据目录权限、SQLite 路径、blob 目录、TLS 证书和必要密钥；不要打印密钥值。
- **`/readyz` 失败**：检查 SQLite 是否可打开、blob 目录是否可写、磁盘空间和服务用户权限。
- **standby 写入返回 503**：这是预期只读栅栏；业务写入必须回到 primary，复制应用不经过业务写入口。
- **复制未启动**：确认角色为 standby、直接父地址有效、本地凭据已导入并通过 `credential verify`；父节点必须是 primary 或已开启 relay 的 standby。
- **复制失败**：查看集群同步事件的脱敏阶段和错误码；认证失败检查凭据状态，网络失败检查 HTTPS/Host/防火墙，blob 失败检查来源存储和哈希。
- **级联 relay 未生效**：确认直接父节点设置 `JIAN_REPLICATION_RELAY_ENABLED=true`、已完成上游同步并已为 child 创建逐跳凭据；不要让 child 越级连接祖父节点。
- **迁移卡住**：检查任务状态、报告和密钥是否保持不变，使用显式恢复，不要把来源凭据或内部地址复制到工单。

排障输出只保留稳定错误码、阶段、脱敏原因、恢复提示和必要时间/序号；禁止粘贴令牌、Authorization、环境变量值、内部地址或系统路径。
