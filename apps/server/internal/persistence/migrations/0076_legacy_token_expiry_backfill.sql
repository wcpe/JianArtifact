-- 0076：历史遗留 API Token 回填过期时间与账号代次
-- 引入版本：未发布
-- 影响：UPDATE api_token 中 expires_at 与 account_epoch 双 NULL 的迁移前旧行：
--       expires_at 回填为迁移执行时刻 +30 天宽限期，account_epoch 回填为所属用户当前代次
-- 数据处理：一次性回填；30 天与机器凭据默认期限一致，到期后旧令牌自然失效促使用户换发受限凭据
-- 回滚：不支持 down migration；回填仅收紧（旧令牌从「永久有效」变为「有截止」），
--       回滚代码不会恢复 NULL 值，行为一致
--
-- 背景：0057 引入 expires_at/account_epoch 时刻意不回填（「不代表库外恢复时代」），
-- tokenCurrentSQL 因此对双 NULL 旧行网开一面——只要账号代次为 1 这些旧令牌永久有效，
-- 过期制度对存量完全不生效。安全审计（jat_ 令牌审计）定此为中危缺口。
--
-- 为什么两个字段一起回填：只补 expires_at 不补 account_epoch 的话，旧行在主条款
-- （t.account_epoch=u.account_epoch）下永不匹配，「宽限 30 天」会变成「立即全部失效」，
-- 对仍在生产使用的 CI/部署脚本是无预警中断。回填为当前代次精确保持迁移前有效性
-- （0057 之前本就无代次追踪，令牌不随改密失效），只是加了时间盒。
-- 为什么给 30 天宽限而非立即吊销：给运维一个完整换发周期，与新建凭据默认期限对齐。
-- 混合行（单字段 NULL）不回填：0057 后所有签发路径同时写两字段，单 NULL 属异常态，按失效处理。
UPDATE api_token SET
    expires_at = strftime('%s','now') + 2592000,
    account_epoch = (SELECT account_epoch FROM user WHERE user.id = api_token.user_id)
WHERE expires_at IS NULL AND account_epoch IS NULL;
