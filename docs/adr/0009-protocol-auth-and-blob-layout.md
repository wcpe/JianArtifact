# ADR-0009：协议端点原生客户端鉴权与 blob 内容寻址布局

## 状态

已接受

## 背景

0.3.0 起引入面向原生客户端（curl、Maven、npm 等）的协议端点，需要解决两件事：

1. **鉴权**：原生客户端不走前端登录流程，如何呈递凭据并复用既有的仓库 ACL 授权（`CanAccess`，public read 匿名放行）。
2. **内容存储**：制品字节如何落盘，才能天然去重、并支撑后续 proxy 缓存与 sha256 完整性校验。

约束：复用 0.2.0 的 API Token 体系（前缀 `jat_`，仅存 sha256 摘要）；不引入新的凭据类型；存储层与元数据（SQLite）解耦，见 ADR-0002。

## 决策

1. **协议端点鉴权 = API Token（HTTP Basic 或 Bearer）**：`Authorization: Bearer <jat_...>` 或 `Authorization: Basic`（token 作 password，为空则取 username）。Basic 凭据仅接受 `jat_` 前缀的 API Token，**不启用口令登录**。Bearer 仍兼容 JWT 会话，行为不变。
2. **blob 采用 sha256 分片内容寻址布局**：`<root>/<h[0:2]>/<h[2:4]>/<hash>`；写入经 `<root>/tmp/*` 临时文件 + 边写边算 sha256 + 原子 `rename` 落盘，命中已存在即去重。

## 理由

- API Token 已有摘要存储与主体解析（`PrincipalByTokenDigest`），复用零新增攻击面；Basic 是原生客户端（尤其 curl `-u`、Maven `settings.xml`）最通用的凭据载体。禁用口令登录避免把长期口令暴露到每次协议请求。
- 内容寻址让相同字节天然去重，并使 sha256 既是存储键又是完整性校验值（ETag）；两级分片目录避免单目录文件过多。临时文件 + 原子 rename 保证并发与中断安全，不会读到半写文件。

## 后果

- 协议端点复用同一 `authenticator.Optional()` 中间件（现支持 Basic + Bearer），授权仍由 `CanAccess` 统一判定：无主体且非 public→401、有主体无权限→403。
- blob 去重后 `DELETE` 仅删元数据、不即时回收 blob（内容可能被多路径共享）；即时 GC / 引用计数留待后续增量。
- Windows 上 `rename` 到已存在目标会失败，Put 对该情形回退为「目标已存在即视为去重成功」，保证跨平台并发写同内容的健壮性。

## 备选方案

- **口令登录（Basic user:pass）**：被否，长期口令随每次请求传输、且与会话模型重叠，安全性与一致性均差。
- **按仓库/路径的原样目录存储**：被否，无法去重、难做完整性校验，proxy 缓存与 GC 也更复杂。
