# ADR-0013：Docker Registry v2 Bearer 令牌认证

## 状态

已接受

## 背景

Docker Registry v2 / OCI Distribution 已支持匿名拉取 public 仓库与预先 Basic 认证（如 `curl -u` 直接带 `Authorization: Basic`）。但真实 OCI 客户端（`docker`、`skopeo`）在**认证推送**时走的是 registry v2 的"挑战-应答"令牌流：客户端先发不带凭据的请求，服务端以 `401 + WWW-Authenticate` 质询应答，客户端据质询到令牌端点换取范围令牌，再以 `Authorization: Bearer <token>` 重试。

此前服务端对受保护 docker 资源仅返回 `401 + WWW-Authenticate: Basic`，且 `GET /v2/` 探活始终返回 200。Basic 质询与这类客户端的令牌流配合不佳，导致 `skopeo copy --dest-creds` / `docker push` 的认证推送不可用。经真机验证（skopeo `--debug`），token-auth 客户端只有在 **`GET /v2/` 探活阶段收到 `401 + WWW-Authenticate: Bearer`** 时才会建立 bearer 认证流程；若 `/v2/` 返回 200，客户端按"无需认证"处理，后续上传遇 401 也不再换取令牌而直接失败。需要在不破坏"匿名拉取 public"与"预先 Basic（curl）"的前提下，补齐标准的 Bearer 令牌流。

## 决策

实现 registry v2 的 **Bearer 令牌认证**，复用既有会话 JWT 的同一 HS256 密钥（`data_dir/.jwt_secret`，见 ADR-0011），不引入新密钥与新依赖：

- **令牌端点** `GET /v2/token`：读 `Authorization: Basic` 解析用户（口令 argon2 或 API Token；无凭据按匿名，提供但无效则 401）；对每个 `scope=repository:{name}:{actions}` 逐项跑既有授权（`pull`→读、`push`→写），只把**授权通过**的动作放进该 scope 的授予集合；用短期（默认 300s）docker 范围令牌承载这些授予返回。
- **401 质询改为 Bearer**：受保护 docker 操作在未认证时返回 `401 + WWW-Authenticate: Bearer realm="{base}/v2/token",service="jianartifact",scope="repository:{name}:{actions}"`（写 = `pull,push`，读 = `pull`）。
- **docker 操作接受 Bearer 令牌**：携带有效 docker 令牌时，**仅按令牌的 `access` 判定**（令牌 `sub` 即已认证身份，授权已在签发时定）；无令牌则回退既有身份解析（预先 Basic / 会话 JWT / API Token / 匿名）+ 既有授权逻辑。
- **`GET /v2/` 发起认证发现**：未带凭据时返回 `401 + WWW-Authenticate: Bearer`（不带 scope），让客户端在探活阶段即发现令牌 realm；带凭据 / 令牌时返回 200。
- **匿名拉取 public 仍成立（不需用户凭据）**：匿名客户端据 `/v2/` 质询到令牌端点换取仅含 public `pull` 的匿名令牌后即可拉取；用户无需提供任何凭据，预先 Basic（curl）亦继续直接生效。

令牌仅在签发时返回，不入库、不进日志；docker 令牌与会话 JWT 结构不同（`access` 集合 vs 用户角色），互不串味。

## 理由

- Bearer 令牌流是 OCI 客户端认证推送的标准路径，补齐后 `skopeo` / `docker` 的真机认证推送可用。
- 复用同一 HS256 密钥与 `JwtSigner`，不新增密钥管理面与依赖，符合"简单优先"。
- 令牌承载的是签发时已判定的授权集合，docker 操作可在不再查库的情况下据令牌快速判定，且授权判定仍集中在既有 `authz` 纯函数，无重复实现。
- "携令牌即按令牌判定、无令牌回退既有身份"两条互斥路径，保证匿名 public 读与预先 Basic 推送照旧可用。

## 后果

- 正面：真实 OCI 客户端（skopeo / docker）的认证推送可用；匿名 public 读与预先 Basic（curl）仍可用；密钥与依赖零新增。
- 负面/约束：`GET /v2/` 不再对匿名直接返回 200，而是先 `401 Bearer` 发起认证发现——匿名 public 拉取因此会先经令牌端点换取一枚匿名令牌（对用户透明，不需凭据），并非严格"零令牌"；新增令牌端点与令牌校验路径，须穷举令牌生命周期（过期 / 伪造 / scope 不匹配）与鉴权矩阵（令牌通道与身份通道分别走一遍）；令牌 TTL 较短，客户端长操作可能需在过期后重新换取（由客户端自身处理）。

## 备选方案

- 维持仅 Basic 质询：与 `docker` / `skopeo` 的令牌流配合不佳，认证推送不可用。落选。
- 为 docker 令牌引入独立签名密钥 / 独立令牌服务：增加密钥管理面与运维复杂度，对单一二进制形态过重。落选（复用既有 HS256 密钥）。
- 令牌内不带 `access`、每次请求再查库判定：令牌退化为纯身份票据，未利用"签发时已判定"的优势，且偏离 registry v2 token 的 `access` 约定。落选。
