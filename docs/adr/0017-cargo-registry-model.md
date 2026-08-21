# ADR-0017：Cargo sparse registry 索引与发布模型

## 状态

已接受

## 背景

Cargo sparse registry 以 `config.json` 声明下载与 API 根，以每个 crate 的 JSON Lines 索引决定版本、checksum 和 yanked 状态。普通 Raw 缓存无法表达这些语义；错误的 API 基址或认证提示会使标准 Cargo 客户端无法 publish 或访问私有 registry。

## 决策

1. Cargo format 的稳定标识为 `cargo`，支持 hosted/proxy/group。协议基址为 `<public-base>/cargo/<repo>/`；`config.json` 的 hosted `api` 是 `<public-base>/cargo/<repo>`，Cargo 自行追加 `/api/v1/...`，服务端端点保持 `/cargo/<repo>/api/v1/...`。
2. `config.json.auth-required` 由当前仓库匿名 read 判定动态生成：匿名可读为 `false`，否则为 `true`，不按 hosted/proxy/group 硬编码。hosted publish 始终要求 Cargo 标准 registry token，对应 JianArtifact `jat_` API Token。
3. `.crate` 写为保留 asset；`cargo_crate_version` 保存规范化 crate、版本、checksum、完整索引记录、yanked 与受控上游来源。索引按版本记录生成，不允许任意 PUT 覆盖。
4. Cargo 的裸 Authorization 只在 Cargo 路由接受且只能解析 `jat_` token；其它协议 Bearer 也仅允许 `jat_`，Web JWT 在协议端点统一拒绝。Release 仓库的原生 Cargo 请求不可覆盖既有逻辑对象；特权覆盖仅统一管理 API 可带 `overrideReason` 执行。
5. proxy 从受控 sparse `remoteUrl` 读取 config/index，并用其 `dl` 模板下载；私有上游经 `credentialRef` resolver，所有地址、DNS 重解析和重定向走统一出站安全策略，下载 checksum 必须匹配索引后才能缓存。

## 理由

- Cargo 要求 API 根而非完整 API 子路径；固定该层级防止 `/api/v1/api/v1` 兼容错误。
- 匿名 read 是仓库 ACL 的权威，动态输出能同时支持公开 hosted 和私有 proxy/group。
- 把索引记录建模为元数据，能让 yank、group 合并、校验、复制和恢复不依赖可变文件拼接。
- Cargo 原生凭据通道不可靠支持 Basic 用户口令，保留 API Token 例外可维持标准客户端兼容。

## 后果

- 需要真实 Cargo CLI 验证 config 请求、publish URL、私有认证、yank 与双节点恢复。
- Cargo 不支持 Git index、任意下载 URL 或直接 VCS fallback；这些能力另立需求。
- `auth-required` 会随匿名访问开关/ACL 变化而变，客户端需重新读取 config 或更新凭据配置。

## 备选方案

- 把 `api` 写为完整 `/api/v1` 地址：被否，Cargo 会再次追加标准路径。
- 所有仓库固定 `auth-required=true`：被否，破坏匿名 read；固定 false 又不能可靠支持私有仓库。
- 把 crate/index 当 Raw 可写文件：被否，会绕过版本不可变、checksum 与 yanked 语义。
