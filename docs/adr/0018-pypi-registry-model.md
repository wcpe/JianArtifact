# ADR-0018：PyPI Simple 索引与受控上游链接模型

## 状态

已接受

## 背景

PyPI 客户端通过 PEP 503/691 的规范项目名、文件链接和 sha256 选择分发包。直接转发上游 HTML 会泄露上游地址并允许不受控下载；把上传文件当 Raw 文件又不能生成稳定的 Simple 索引。

## 决策

1. PyPI 协议使用 `/pypi/<repo>/simple/`、`/pypi/<repo>/packages/...` 与 hosted `/pypi/<repo>/legacy/`。项目名按 PEP 503 规范化，Simple HTML/JSON 始终由 `pypi_file` 元数据动态生成，文件链接只指向本节点 public URL。
2. `pypi_file` 保存项目、版本、文件名、asset 路径、sha256、requires-python、yanked 和受控来源信息；实际文件继续是内容寻址 asset。hosted legacy 上传以 `MultipartReader` 流式处理并限制元数据字段，重复文件名不可覆盖。
3. proxy 优先解析 PEP 691 JSON，回退到安全 HTML 解析；仅允许来自配置 Simple 根地址的、经统一出站安全策略验证的 http/https 链接。私有上游凭据只经 `credentialRef` 运行时 resolver 提供，所有响应、持久化、日志、审计和报告不得包含凭据值或上游文件 URL。
4. protocol Basic 接受账号口令或 API Token，Bearer 仅接受 `jat_` API Token，Web JWT 被拒绝。不可变 Release 的原生 legacy 上传不能覆盖；格式感知管理员覆盖仅统一管理 API 可带 `overrideReason` 执行。
5. proxy 缓存和 hosted 发布以 FR-105 完成边界复制，接收节点先取得 blob 后才使 `pypi_file` 出现在 Simple 索引。group 根据本地已验证候选记录按成员顺序合并，不拼接上游正文。

## 理由

- 动态索引与本地链接使 pip 永不需直连上游，哈希始终可由本节点验证和发布。
- 严格链接、DNS 与重定向检查阻断 SSRF、云元数据访问和链接注入。
- 分发文件和协议元数据分开保存，既复用 blob 回收又维持 PyPI 候选选择语义。
- 特权覆盖不走 Twine 协议，可避免把审计原因塞入客户端不保证支持的字段。

## 后果

- 首版不实现 Warehouse JSON API、所有者、搜索、TUF、签名、yank 或原生删除。
- 代理上游必须提供规范 Simple 输出；不安全 URL、未受控重定向或不匹配哈希会被拒绝而不缓存。
- 需要覆盖 PEP 503/691、Twine、pip、私有上游、断网缓存和双节点一致性的测试。

## 备选方案

- 直接透传上游 Simple HTML/JSON：被否，泄露上游且无法控制下载链接。
- 用 Raw 文件树手工维护 index：被否，允许绕过项目规范化、哈希与不可变发布。
- 接受任意用户提供的下载链接：被否，会形成 SSRF 与缓存投毒入口。
