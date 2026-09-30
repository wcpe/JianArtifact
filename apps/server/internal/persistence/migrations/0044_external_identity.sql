-- 0044：外部身份源接入的数据基座（OIDC / LDAP）
-- 引入版本：0.11.0（开发中）
-- 影响：user 表新增 auth_source 与 external_subject 两列 + 外部标识的 partial unique index
-- 数据处理：无历史回填——既有行缺省即 local（本地口令来源）、external_subject 为空串；
--          ADD COLUMN 带常量默认值不重写全表，预计耗时常量级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：docs/adr/0029-external-identity-providers.md、docs/specs/0.11.0-external-identity-providers.md
--
-- 口径：
--   - auth_source 标记身份来源：local（本地口令）/ oidc / ldap（见 ADR-0029）；
--     外部来源用户不设本地口令（password_hash 为空串），登录只走对应身份源校验。
--   - external_subject 存身份源内稳定标识：OIDC 的 sub / LDAP 的 DN；
--     空串表示"未绑定外部身份"（既有本地用户与尚未绑定的账号）。
--   - 唯一性必须用 **partial unique index**（WHERE external_subject <> ''）：
--     普通唯一索引会把多行空串视作重复，从而禁止第二个「未绑定」用户——索引只约束非空值。
ALTER TABLE user ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local';

ALTER TABLE user ADD COLUMN external_subject TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_user_external_subject ON user (external_subject) WHERE external_subject <> '';
