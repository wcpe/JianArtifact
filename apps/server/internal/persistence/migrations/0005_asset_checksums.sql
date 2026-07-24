-- 0005_asset_checksums：asset 表补登 SHA-1 与 MD5 校验和列。
-- 前向追加迁移，不修改已有迁移文件。
-- 写入制品时一次性计算并落库，读取时直接取列值，不在读路径现算。

ALTER TABLE asset ADD COLUMN sha1 TEXT NOT NULL DEFAULT '';
ALTER TABLE asset ADD COLUMN md5 TEXT NOT NULL DEFAULT '';
