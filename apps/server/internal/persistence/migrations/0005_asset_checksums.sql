-- 0005：asset 表补登 SHA-1 与 MD5 校验和列
-- 引入版本：0.4.0
-- 影响：新增 asset.sha1、asset.md5 字段
-- 数据处理：已有记录使用空串默认值，不回填历史内容；预计耗时：随 asset 行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-25b / ADR-0002
-- 前向追加迁移，不修改已有迁移文件。
-- 写入制品时一次性计算并落库，读取时直接取列值，不在读路径现算。

ALTER TABLE asset ADD COLUMN sha1 TEXT NOT NULL DEFAULT '';
ALTER TABLE asset ADD COLUMN md5 TEXT NOT NULL DEFAULT '';
