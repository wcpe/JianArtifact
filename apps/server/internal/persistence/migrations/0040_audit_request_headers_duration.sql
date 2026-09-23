-- 0040：审计事件补记常用请求头与单段服务端耗时
-- 引入版本：0.10.0（开发中）
-- 影响：audit_log 新增 http_headers（常用白名单请求头紧凑 JSON）、duration_server_ms（业务路由内服务端处理耗时）列
-- 数据处理：已有行使用空串/零值默认值，无历史回填；预计耗时：随 audit_log 行数增长（仅加列）
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：审计中心请求上下文补强（常用头 + 服务端单段耗时）
--
-- 背景：审计中心已记录路由模板/状态码/总耗时（duration_ms，自中间件入口起算），但排查
-- 「客户端慢还是服务端慢」时缺少分段耗时，也无法回看请求携带的常用协商头
-- （Accept-Language / Content-Type / Origin 等）。
-- 安全边界：
--   - http_headers 只存白名单常用头，命中 auditctx.sensitiveKeys 的头名（含 Authorization）
--     一律跳过；单值截断防爆行，不写入任何凭据原文。
--   - duration_server_ms 口径 = 进入业务路由处理（契约路由级中间件链入口）到审计落笔时刻
--     的服务端处理耗时；请求在进入业务路由前被拒绝（如认证拦截）时为 0。
-- 快照签名决定：新字段只做展示，不参与 auditReadSnapshot 指纹 / matchesFilter 筛选维度。
ALTER TABLE audit_log ADD COLUMN http_headers TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN duration_server_ms INTEGER NOT NULL DEFAULT 0;
