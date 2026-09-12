-- 0012：管理审计日志
-- 引入版本：0.7.0
-- 影响：新增 audit_log 表
-- 数据处理：创建审计结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-38 / ADR-0002
-- 0012（审计日志，FR-38）：记录全部管理写操作（制品上传/删除/修改 + 仓库/ACL/用户/令牌/设置），
-- 供管理端「审计日志」页查看"谁在何时做了什么"（操作者/时间/操作类型/对象/仓库/结果/IP）。
-- 与 repl_change（复制同步用）相互独立：repl_change 服务复制，audit_log 服务审计。
CREATE TABLE audit_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            TEXT    NOT NULL,             -- 操作时间（RFC3339Nano）
  actor         TEXT    NOT NULL DEFAULT '',  -- 操作者（用户名；匿名/系统操作为空或 system）
  action        TEXT    NOT NULL,             -- 操作类型：asset.put/asset.delete/repo.create/repo.update/repo.delete/acl.set/user.create/user.update/user.delete/user.password/token.create/token.revoke/setting.set
  entity_type   TEXT    NOT NULL DEFAULT '',  -- 对象类型：asset/repository/acl/user/token/setting
  entity_key    TEXT    NOT NULL DEFAULT '',  -- 对象标识（如 asset 的 repo/path、repository 的名称）
  repo          TEXT    NOT NULL DEFAULT '',  -- 关联仓库名（asset 操作必填，其他可为空）
  detail        TEXT    NOT NULL DEFAULT '',  -- 补充说明（如制品大小、变更内容摘要），JSON 或纯文本
  result        TEXT    NOT NULL DEFAULT 'ok', -- 结果：ok / error（失败时 detail 含错误摘要）
  ip            TEXT    NOT NULL DEFAULT ''   -- 操作来源 IP（经 X-Forwarded-For 取，可能为空）
);
CREATE INDEX idx_audit_log_ts ON audit_log(ts DESC);
CREATE INDEX idx_audit_log_actor ON audit_log(actor);
CREATE INDEX idx_audit_log_action ON audit_log(action);
CREATE INDEX idx_audit_log_repo ON audit_log(repo);
