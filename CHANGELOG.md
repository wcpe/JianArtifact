# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增
- 项目文档与治理脚手架初始化（PRD、架构、ADR、防漂移规则、工程化配置）
- 运行地基：TOML + 环境变量配置加载、嵌入式 SQLite 元数据库与迁移、文件系统 blob 存储（多校验和）、空库首启管理员引导、健康检查端点
- 认证与身份层：本地口令登录与 JWT 会话（TTL / 刷新 / 当前用户 /me）、API Token 签发/列表/吊销（哈希存储）、Basic Auth 鉴权、全局角色与管理员用户管理、统一身份解析中间件（Bearer-JWT / Bearer-Token / Basic / 匿名 四通道）、登录暴力破解防护（失败锁定 / 限流）

### 变更
- 无

### 修复
- 无

### 移除
- 无

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
