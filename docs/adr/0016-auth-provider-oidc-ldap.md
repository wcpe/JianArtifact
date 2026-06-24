# ADR-0016：认证 provider 抽象与 OIDC/LDAP 集成

## 状态

已接受

## 背景

ADR-0003 确立了第一期三条本地认证路径（本地用户名/密码 argon2、Bearer Token、Basic Auth）+ Web 会话/JWT，并**在 `auth` 模块预留了"认证 provider 抽象"作为接口边界**，明确 OIDC/LDAP 留待 P2 落地、第一期不落任何占位字段。本 ADR 就是把那条预留边界落地，对应 PRD FR-34（OIDC 认证集成）、FR-35（LDAP 认证集成），均为 P2，本期同批落地。

企业部署常要求接入既有身份源：通过 OIDC 对接 IdP（如 Keycloak、Azure AD、Okta），或通过 LDAP 对接目录（如 AD、OpenLDAP），让员工用既有账号登录制品库，免去逐个本地建号。约束驱动因素：

- 不得破坏既有四通道（会话/JWT、Bearer、Basic、Docker v2 Bearer）与鉴权矩阵（ADR-0004），外部认证只新增"身份从哪来"，不改"能不能访问哪个仓库"的判定。
- 系统**不开放公开自助注册**（ADR-0010 红线）：外部 IdP/目录里"任何能登录的人"不得因此自助成为本地用户、更不得自助成为管理员。外部身份→本地用户/角色的映射必须显式受控。
- 外部 IdP/目录的凭据（OIDC `client_secret`、LDAP bind 口令等）是密钥，真源在配置/env，绝不入库、不进日志、不进 DB 明文（守 architecture-invariants 真源与脱敏不变量）。
- 保持单一二进制、零外部运行时依赖；TLS 一律走 rustls，不引 openssl/native-tls。

## 决策

在 `auth` 模块把既有预留边界落地为统一的认证 provider 抽象，OIDC 与 LDAP 作为**可选** provider 接入；本地用户名/密码（argon2）仍是**默认且始终启用**的 provider。OIDC 与 LDAP 在本期（P2）同批实现。

### 1. provider 抽象形态

定义 `auth` 内部 trait（示意，最终签名以实现为准）：

```rust
/// 认证 provider：把一次"凭证表单/外部回调"解析为已认证的本地主体（或失败）。
#[async_trait]
trait AuthProvider: Send + Sync {
    /// provider 标识（"local" / "oidc" / "ldap"），用于配置与审计。
    fn kind(&self) -> ProviderKind;
    /// 用口令型凭据认证（本地、LDAP bind 走此路径）；OIDC 不实现此法。
    async fn authenticate_password(&self, username: &str, password: &str)
        -> Result<AuthenticatedSubject, AuthError>;
}
```

- provider 只负责"**证明你是谁**"（产出 `AuthenticatedSubject`：外部唯一标识 + 显示名 + 可选邮箱/组），**不负责授权**。授权仍由 `authz` 按 ADR-0004 的三层判定（角色 × 可见性 × ACL）统一处理，外部 provider 不绕过、不旁路鉴权矩阵。
- OIDC 走浏览器重定向的授权码流，不套进 `authenticate_password`，由独立的 OIDC 回调编排（见第 3 节）调用 provider 的换码/校验能力，最终同样产出 `AuthenticatedSubject`。
- provider 按配置在启动时装配：未配置 OIDC/LDAP 段时不实例化对应 provider（与第一期"未配置即不存在"一致，不留运行期空壳）。

### 2. 既有四通道不变，外部认证只接在"登录入口"

- **会话/JWT、Bearer Token、Basic Auth、Docker v2 Bearer 四通道的解析与鉴权逻辑完全不变**。外部 provider 只参与"用户首次换取本地会话"这一步：
  - LDAP：Web 登录表单与 Basic Auth 的口令校验，可按配置委托给 LDAP provider 做 bind 校验（见第 4 节），成功后照常签发本地会话/JWT。
  - OIDC：仅用于 Web 控制台登录，经授权码流换取本地会话/JWT（见第 3 节）。
- **认证成功后一律收敛为本地会话/JWT 或本地 API Token**，下游所有受保护请求仍只认这套既有凭据。即：外部身份只在登录边界出现一次，**不在每个 API 请求上反复回源 IdP/目录**（不在请求热路径做外部网络调用）。API Token 仍由本地用户自助签发，CLI/包管理器不直接对 IdP 认证。
- 本地口令登录与外部 provider **默认并存**（混合模式）；是否禁用本地口令登录留作后续可选项，本期不做。

### 3. OIDC：授权码流

- 采用标准 **Authorization Code Flow + PKCE**：
  1. 前端请求 `GET /api/v1/auth/oidc/login` → 服务端生成 `state` + PKCE `code_verifier`（服务端短期持有，绑定一次性），重定向到 IdP 授权端点。
  2. IdP 回调 `GET /api/v1/auth/oidc/callback?code=&state=` → 服务端校验 `state`，用 `code` + `client_secret` + `code_verifier` 向 IdP token 端点换取 ID Token，**校验签名（JWKS）、`iss`/`aud`/`exp`/`nonce`**，解析出外部身份（`sub` + 邮箱/显示名）。
  3. 经"外部身份→本地用户"映射（第 5 节）得到本地用户后，**照常签发既有会话/JWT**（TTL、刷新端点 `POST /api/v1/auth/refresh`、登出/吊销与 ADR-0011 完全一致）。
- 外部 IdP 的会话与本地会话解耦：登出/吊销按 ADR-0011 即时失效本地会话；不实现 OIDC 前端登出（front-channel logout）联动（P2 不需要，避免镀金）。
- OIDC discovery（`/.well-known/openid-configuration`）与 JWKS 在启动/首次使用时拉取并缓存，按标准 TTL 刷新。

### 4. LDAP：bind 校验

- 采用 **bind 校验**模式：用配置的 `bind_dn` + bind 口令连接目录，按 `user_search_base` + 过滤模板查到用户 DN，再用该 DN + 用户提交的口令做一次 bind；bind 成功即认证通过。
- LDAP 仅参与口令型登录（Web 表单 / Basic Auth），产出 `AuthenticatedSubject` 后同样收敛为本地会话/Token。
- 连接走 LDAPS / StartTLS，TLS 由 rustls 提供；不接受明文 LDAP（除非运维显式在可信内网开启，默认 TLS）。

### 5. 外部身份 → 本地用户/角色映射（守 ADR-0010）

- **稳定外部标识**：以 provider kind + 外部稳定标识（OIDC `sub` / LDAP 用户 DN 或 `objectGUID`）作为外部身份键，在本地 `users` 表新增可空的 `external_idp` / `external_subject` 列建立绑定（仅存非敏感的身份标识，**不存任何外部凭据**）。
- **即时开通（JIT）策略，默认关闭**：
  - 默认 `auto_provision = false`——外部认证成功但本地无对应用户时**拒绝登录**，需管理员先建本地账号并绑定外部身份。这是最严格、最契合"不开放自助注册"的默认。
  - 运维可显式开启 `auto_provision = true`：首次外部登录成功时即时创建本地用户，**默认角色固定为最低权限 `User`**（绝不为外部用户自动授予 `Admin`）。
- **默认角色与提权边界**：JIT 开通的用户一律落为全局 `User`；任何到 `Admin` 的提升只能由现有管理员显式操作。可选支持把 IdP/目录的组声明映射到本地角色，但"映射到 Admin"必须是运维在配置中显式、白名单式声明，默认不开启——避免"配错一个组名就批量造管理员"。
- 该映射逻辑承接 ADR-0010 的不变量：外部认证不是"公开自助注册"的旁路；`auto_provision` 关闭时行为等价于"只有管理员预置的账号能登录"。

### 6. 凭据来源（绝不入库）

- OIDC `client_secret`、LDAP bind 口令等密钥**真源在配置文件/env**（前缀 `JIANARTIFACT_`，与既有配置一致）；DB 不存这些密钥明文。
- 沿用既有"DB 仅存引用"约定：provider 配置中引用的密钥按 `upstream_auth_ref` 同类模式以引用形式落地，真值走 env/配置（参见 ARCHITECTURE 凭据真源约定）。
- 这些密钥不得进日志、不得进错误响应；OIDC ID Token、LDAP 口令等在日志中一律脱敏。

## 理由

- **落地既有预留边界、不另起炉灶**：ADR-0003 已把"provider 抽象"定为扩展点，本 ADR 顺着该边界实现，新增"身份从哪来"而不改"能访问什么"，鉴权矩阵零改动、可继续穷举。
- **收敛到本地会话/Token**：外部身份只在登录边界出现一次，下游复用既有四通道，既不在请求热路径回源外部系统（性能与可用性都更稳），也避免四通道各自重写一套外部认证。
- **默认最严**：`auto_provision` 默认关闭、JIT 默认角色 `User`、Admin 映射需显式白名单——三道闸守住 ADR-0010"不自助注册、不任意人成管理员"红线，配置失误不至于批量造管理员。
- **凭据不入库**：外部密钥真源在 env/配置、DB 仅存非敏感身份绑定，延续既有脱敏与真源不变量。
- **栈不漂移**：OIDC/LDAP 库选型须支持 rustls、纯 Rust、不拉 openssl/native-tls，保持单一二进制与零外部运行时依赖。

## 后果

- 正面：企业可经既有 IdP/目录登录；本地认证与四通道、鉴权矩阵不受影响；外部凭据不入库、不进日志；演进路径清晰（provider 可逐个接入）。
- 负面/约束：
  - 新增 OIDC 回调编排与 LDAP bind 路径，**凭据与会话高风险验证区（testing-and-quality §2.6）须扩测**：外部认证失败/超时回退、`state`/`nonce`/PKCE 防 CSRF 与重放、ID Token 签名与声明校验、LDAP bind 失败与目录不可用、外部凭据脱敏、JIT 默认角色与"不自动成管理员"边界、`auto_provision` 关/开两条路径、外部身份键与本地用户绑定的一致性。
  - 新增配置项（OIDC issuer/client/secret-ref、LDAP url/bind/search 等）与可空的 `external_idp`/`external_subject` 列；这些仅在本 ADR 进入实现时落地。
  - 引入外部 IdP/目录后，制品库的登录可用性部分依赖外部系统可用性（仅登录边界，受保护请求因收敛为本地会话/Token 而不受影响）。
  - 新增第三方依赖 `openidconnect`、`ldap3`（均裁到 rustls，已按 §15 经用户确认）。

## 备选方案

- **每个受保护请求都回源 IdP/目录校验**（不收敛为本地会话）：把外部网络调用放到请求热路径，性能差、外部抖动即全站登录态受损，且要在四通道各自接外部认证。落选——只在登录边界认证、收敛为本地会话。
- **外部登录默认即时开通并自动建号**（`auto_provision` 默认开启）：等价于对 IdP/目录里所有人开放自助注册，触碰 ADR-0010 红线。落选——默认关闭，开通须运维显式开启且默认角色 `User`。
- **OIDC 走 Implicit Flow 或纯前端换 token**：Implicit 已被 OAuth 2.1 弃用、`client_secret` 暴露风险高。落选——用服务端授权码流 + PKCE。
- **LDAP 用"读出口令哈希自行比对"而非 bind**：多数目录不暴露口令哈希、且自行比对削弱目录侧策略。落选——用标准 bind 校验。
- **第三方密钥写入 DB 便于热更**：违反"凭据绝不入库"红线。落选——真源在 env/配置，DB 仅存引用。
