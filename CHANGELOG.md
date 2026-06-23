# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增
- 项目文档与治理脚手架初始化（PRD、架构、ADR、防漂移规则、工程化配置）
- 运行地基：TOML + 环境变量配置加载、嵌入式 SQLite 元数据库与迁移、文件系统 blob 存储（多校验和）、空库首启管理员引导、健康检查端点
- 认证与身份层：本地口令登录与 JWT 会话（TTL / 刷新 / 当前用户 /me）、API Token 签发/列表/吊销（哈希存储）、Basic Auth 鉴权、全局角色与管理员用户管理、统一身份解析中间件（Bearer-JWT / Bearer-Token / Basic / 匿名 四通道）、登录暴力破解防护（失败锁定 / 限流）
- 仓库模型与授权层：仓库创建/配置/删除（格式、hosted/proxy 类型、public/private 可见性）、每仓库读写 ACL 管理、按全局角色×可见性×ACL 综合判定的授权纯函数、仓库列表（按身份过滤）/详情/制品浏览端点；私有仓库对匿名与无权用户一律返回 404 隐藏存在性
- 制品通用机理与统一格式 trait + Raw 参考格式：hosted 制品流式直传/下载、proxy 代理上游并缓存（cache-miss 回源→校验→落盘→写索引、命中不回源、并发单飞合并、上游失败回退不写坏缓存）、blob 先落盘再写索引（失败回滚不留孤儿）、上传大小限制（超限 413）、四校验和计算与暴露、制品删除与按格式覆盖策略、Raw 格式端点（PUT/GET/DELETE 路径直存直取）、制品详情（四校验和 + 使用方式片段）、跨仓库搜索（结果按读权限过滤、不泄露无权私有制品）
- 三种高频格式（hosted+proxy）经统一 Format trait 注册接入通用机理：Maven（仓库布局、maven-metadata.xml、.sha1/.md5/.sha256 sidecar、release 不可覆盖 409 / snapshot 可覆盖）、npm（packument/tarball、publish 解析 _attachments、已发布版本不可覆盖、dist shasum/integrity 摘要、scoped 包）、Docker/OCI（Registry v2：blob 上传状态机与 digest 校验、manifest 存取、同 tag 可覆盖、未认证带 WWW-Authenticate）

### 变更
- 无

### 修复
- 无

### 移除
- 无

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
