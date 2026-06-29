# ADR-0037：Maven 服务端权威 maven-metadata.xml + pom 三级兜底（client-priority）

## 状态

已接受

## 背景

第一期 Maven 格式（FR-14，见 `docs/specs/p1-maven.md`）落地时采纳了「**服务端不重写 `maven-metadata.xml`，由客户端自管**」的决策：`mvn deploy` 自带上传 `maven-metadata.xml`，服务端按文件逐个存取并允许覆盖即可。该决策在「纯 `mvn` 客户端」场景下成立——`mvn` 会先 GET 现有 metadata、合并本次版本、再 PUT 回完整聚合。

但随 Web 控制台通用上传（FR-73）与 jar 内嵌 pom 解析（FR-120）引入，旧决策出现缺口：

- **Web 上传不产生 metadata**：经网页上传的 Maven 版本没有 `mvn` 在客户端计算并 PUT `maven-metadata.xml` 的环节，故这些版本在 `{groupId}/{artifactId}/maven-metadata.xml` 的版本列表里**不可见**，`mvn dependency:get` 等按 metadata 解析的客户端拉不到。
- **Web 上传可能缺 pom**：`mvn dependency:get` / 传递依赖解析需要 `{a}-{v}.pom`；网页只传 jar 时没有 pom，制品不完整。

旧决策把「metadata 与 pom 的完整性」全部押在客户端，无法覆盖「非 `mvn` 来源」的写入。FR-121 需要服务端成为 Maven 元数据的权威维护者。

## 决策

**取代旧决策**（`docs/specs/p1-maven.md` §2/§6「服务端不重写 maven-metadata.xml、由客户端自管」作废）：服务端**权威生成并维护** Maven 派生元数据，遵循 **client-priority**（客户端已提供的内容为主，服务端只补缺并维护聚合）：

1. **artifact 级 `maven-metadata.xml` 服务端权威生成**：任一 Maven 主版本文件（jar/war/pom 等，能反解 GAV、非 sidecar、非 metadata）写入 hosted 仓库后，服务端按 SQLite 索引（唯一真源）**聚合该 `{groupId}/{artifactId}` 下全部版本**，重新生成 `{groupId路径}/{artifactId}/maven-metadata.xml`（`versioning/versions/latest/release/lastUpdated`）并落盘，附四校验和 sidecar。SQLite 仍是版本真源，metadata 文件是其按写入派生的物化视图（可反复幂等重写，`maven-metadata.xml` 本就可覆盖、无 409）。
2. **pom 三级兜底**：保证每个 Maven 主构件 GAV 下存在 `{a}-{v}.pom`，来源优先级——① **客户端已上传的 pom**（`mvn deploy` 自带，client-priority，不覆盖）→ ② **jar 内嵌 pom**（复用 FR-120 从 `META-INF/maven/.../pom.xml` 原样提取）→ ③ **按 GAV 生成最小 pom**（`modelVersion=4.0.0` + GAV + 据扩展名推断 `packaging`）。仅在 pom **缺失**时兜底生成，附四校验和 sidecar。
3. **client-priority 边界**：服务端只补缺与维护聚合，绝不改写客户端已上传的 pom / 制品本体；`maven-metadata.xml` 因服务端聚合即权威（且与 `mvn` 客户端计算结果内容等价），由服务端在写入后重生成。

`lastUpdated` 取该 artifact 下全部版本文件 `created_at`（SQLite `CURRENT_TIMESTAMP`，UTC）的最大值、去非数字字符截 14 位为 `yyyyMMddHHmmss`，**不新增时间库依赖**。

完整 SNAPSHOT 快照规则（时间戳唯一版本 + snapshot 级 metadata）见后续 FR-122，本 ADR 聚焦 release 级 artifact metadata 与 pom 兜底。

## 理由

- **守「数据不外发」与「简单优先」**：聚合源是本地 SQLite 索引（唯一真源），metadata 为派生物化视图，不引入外部组件、不新增依赖（`lastUpdated` 复用 `created_at`）。
- **client-priority 兼容 `mvn` 现状**：`mvn deploy` 仍按原协议工作——其自带的 pom 不被覆盖；其 PUT 的 metadata 与服务端聚合内容等价（服务端在 jar PUT 后已含该版本）。既不破坏既有真机互通，又补齐 Web 上传缺口。
- **生成在写时**：按 Maven 仓库布局把 metadata / pom 物化为真实文件，复用既有 `put_hosted` + 校验和 sidecar 机理与统一下载 / 浏览路径，`mvn` 客户端按原生协议直接消费，无需为 Maven 另设动态读端点。

## 后果

- 正面：Web 上传与 `mvn deploy` 产出的 Maven 制品都具备完整 pom + 权威 metadata，`mvn dependency:get` 可解析；版本列表跨「Web / mvn」两种来源一致。
- 负面 / 约束：每次主版本写入触发一次 metadata 重生成（列举该 artifact 前缀下制品 → 纯函数聚合 → 落盘 + sidecar），是写路径上的额外 IO；按 P1 单机 SQLite 规模可接受，且锁外执行。并发写同一 artifact 时 metadata 以「最后一次重生成」收敛（每次都按当前已提交版本全量聚合，最终一致）。
- 后续：SNAPSHOT 时间戳唯一版本与 snapshot 级 metadata 由 FR-122 在本决策基础上扩展；Web 上传页多文件（可附 pom）由 FR-123 接入「用户上传 pom」这一兜底层级。

## 备选方案

- **维持旧决策（客户端自管）**：被否。无法覆盖 Web 上传来源，metadata / pom 缺失，`mvn` 解析失败。
- **读时动态合成 metadata（仿 NuGet 版本列表）**：被否。Maven `maven-metadata.xml` 及其 sidecar 是布局内的真实文件，读时合成还需同时合成 sidecar 并拦截多种读路径（下载 / 浏览），复杂度高于写时物化；写时物化复用既有 `put_hosted` + sidecar 机理更简单一致。
- **引入 chrono/time 取 `lastUpdated`**：被否（YAGNI）。`created_at` 已是 UTC ISO8601，去非数字即得 `yyyyMMddHHmmss`，无需新依赖。
