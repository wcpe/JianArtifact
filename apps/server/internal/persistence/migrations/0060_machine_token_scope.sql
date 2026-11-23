-- 机器 Token 用途与作用域（受限签发凭据）：
--   purpose 取值：''（过渡期的未限用途旧凭据）、artifact_read、artifact_publish；
--   scopes_json 为 [{repository_id,path_prefix,actions[]}] 的 JSON 数组，作用域
--   在每次鉴权时与实时 ACL 取交集，角色不放大 Token 权限。
ALTER TABLE api_token ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
ALTER TABLE api_token ADD COLUMN scopes_json TEXT NOT NULL DEFAULT '';
