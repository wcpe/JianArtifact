# ADR-0016：OCI Registry 路由与元数据模型

## 状态

已接受

## 背景

Docker/OCI Distribution API 的镜像 tag、manifest、layer 与断点上传不是普通文件路径。JianArtifact 既要在一个 HTTP 入口中区分内部仓库和镜像名称，也要让内容寻址 blob、FR-104 回收、FR-105 原子复制和 FR-109 不可变 Release 协同，而不引入独立 registry 进程或对象存储。

## 决策

1. OCI 协议使用 `/v2/<jian-repository>/<image>/...` 路由；第一个路径段是 JianArtifact 仓库，其余 manifest/blob 路径中的镜像名保持 OCI 名称语义。`/v2/` 只返回 Registry v2 能力探测。
2. blob 与 manifest 正文仍存现有内容寻址 blobstore，并以保留 asset 路径建立引用；`oci_manifest` 保存不可变 manifest digest 与媒体类型，`oci_tag` 保存可变 tag→digest 指针，`oci_upload` 保存同盘暂存上传会话。只有最终 manifest 写入才原子公布引用与 operationId。
3. hosted 原生 push 允许非不可变仓库更新 tag；不可变 Release 的原生协议始终拒绝既有 tag/digest 覆盖，即使调用者是管理员。格式感知管理员覆盖只能由统一管理 API 以 `overrideReason` 触发并进入审计。
4. proxy 只通过统一出站安全策略访问 `remoteUrl`；私有上游使用 `credentialRef` 的运行时 resolver，公开 Registry Bearer challenge 只申请最小 pull scope，任何令牌值不持久化、不审计。
5. manifest/tag 与引用 asset 以 FR-105 的 operationId 完成边界复制；接收端先取得 blob，后单事务公布 OCI 元数据。协议 Bearer 仅允许 `jat_` API Token，Web JWT 不进入 OCI 协议主体。

## 理由

- 仓库名前缀保留 Docker 客户端的单主机引用模型，不需端口分仓或额外进程。
- asset 承接实际 blob 引用，使 FR-104 的节点全库回收无需第二套物理存储；专用表仅保存协议查询所需索引。
- 分开上传暂存与最终 manifest 可避免客户端看到未完整 layer 的镜像，并让大 layer 不生成大量中间复制/审计噪声。
- 原生 Docker 协议没有通用、可信的管理员覆盖原因载体；把特权覆盖收敛到统一管理 API，能满足不可变与可审计边界。

## 后果

- 需新增 OCI 元数据、上传恢复、manifest 校验、上游 challenge 和双节点测试。
- Docker 的管理删除、跨仓库 blob mount、签名验证和镜像扫描不在该决策范围，必须另立需求。
- OCI proxy 对上游地址和重定向的限制可能拒绝不安全或配置错误的 Registry；管理员需提供合规的公开地址或受控凭据引用。

## 备选方案

- 每个 OCI 仓库单独端口或独立进程：被否，增加部署拓扑和端口治理成本。
- 将 manifest/tag 当 Raw 任意文件：被否，无法保证 digest、tag、layer 引用与原子可见性。
- 在 Docker push 中允许管理员 header 覆盖：被否，客户端兼容性和原因可信度不足，特权路径应统一到管理 API。
