-- 0008：清理 proxy 误缓存的上游 HTML 目录索引页假制品
-- 引入版本：0.6.0
-- 影响：删除 path 为空或以 / 结尾的历史 asset 记录
-- 数据处理：清理既有脏数据，预计耗时：随 asset 行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：无 / ADR-0002
-- 0008：清理 proxy 误缓存的上游 HTML 目录索引页假制品（path 为空或以 / 结尾）。
-- 根因已在 AssetService.Resolve 修复（目录形路径不再回源缓存），此处清除历史脏数据；
-- 对应 blob 内容按既有约定不即时清理。
DELETE FROM asset WHERE path = '' OR path LIKE '%/';
