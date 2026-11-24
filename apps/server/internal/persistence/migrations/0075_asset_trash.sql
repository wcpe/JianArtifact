-- 0075：资产回收站（Maven 版本级软删除）
-- 引入版本：未开窗（本分支提交时尚未开窗）
-- 影响：新增一张表 asset_trash 与三个索引；不改动任何既有表
-- 数据处理：无历史回填，纯建表
-- 回滚：不支持 down migration；本迁移只新增表，旧版本代码不认识它，回滚代码即可继续运行
--       （回滚期间 asset_trash 中的行会被旧代码视为不存在，其 blob 会被 blob-gc 判为孤儿回收，
--        因此**回滚前必须先恢复或彻底清理回收站**，见规格文档）
--
-- 为什么要与 trash_entry 分开而不是复用：
--   trash_entry 是**存储清理隔离区**的账本：条目必须归属一条 clean_task、恢复靠
--   os.Rename(trash_path, original_path) 把磁盘文件搬回去。它记录的是「文件系统事实」。
--
--   资产回收站记录的是「元数据事实」：Maven 制品是 SQLite 里的 asset 行 + 内容寻址 blob，
--   同一 blob 可能被多个路径共享，没有「一个可 rename 回去的原文件」。恢复的语义是
--   「按保存下来的字段重建 asset 行」，而不是搬文件。两者混在一张表会让 task_id 与
--   trash_path 对资产条目变成恒空的无意义列，并迫使恢复逻辑按类型分叉——那是语义污染，
--   后续每次改动都要重新判断「这条记录属于哪种」。
--
-- 为什么必须在库里保存完整资产快照（而不是只存 path + blob_hash）：
--   恢复要还原到**与删除前完全一致**的状态，包括 created_at / updated_at / sha1 / md5 /
--   content_type。只存 path 与 hash 会让恢复后的制品在列表排序、时间显示与校验和字段上
--   与原件不同——用户点的是「恢复」，不是一个「碰巧内容相同的重新上传」。
--
-- 为什么 repository_id 用 ON DELETE CASCADE 而 blob 不受影响：
--   删仓库是管理员对仓库本身的动作，仓库没了其回收站条目也不该继续占着可见位置；
--   条目消失后 blob 引用一并消失，blob-gc 自然回收。这与 asset 表的既有级联口径一致。

CREATE TABLE asset_trash (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  -- 原仓库。仓删除时级联清理本表（与 asset 表同口径）。
  repository_id    INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  -- 原 asset 路径（仓库内相对路径），恢复时按它写回
  path             TEXT NOT NULL,
  -- 内容寻址 blob 哈希：删除只动元数据，**blob 必须保留**，否则恢复不可能
  blob_hash        TEXT NOT NULL,
  size             INTEGER NOT NULL,
  content_type     TEXT NOT NULL DEFAULT 'application/octet-stream',
  -- 校验和与时间戳是「恢复后与原件一致」的一部分，必须随条目保存
  sha1             TEXT NOT NULL DEFAULT '',
  md5              TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  -- 删除动作：version（整版本）/ snapshot_build（单个时间戳快照构建）/ artifact（整个 artifact）
  -- 仅用于展示与审计追溯，不参与恢复判定
  scope            TEXT NOT NULL DEFAULT 'version',
  -- 触发删除的主体，便于审计回答「谁删的」
  actor            TEXT NOT NULL DEFAULT '',
  operation_id     TEXT NOT NULL DEFAULT '',
  -- staged 在回收站（blob 保留）/ restored 已恢复 / purged 已彻底清理（blob 已释放）
  status           TEXT NOT NULL,
  -- 入回收站时间，保留期回收按它计算
  trashed_at       TEXT NOT NULL DEFAULT (datetime('now')),
  resolved_at      TEXT
);

-- 列表与统计只查 staged：把状态放在最前，让「回收站里还有多少」恒定走索引。
CREATE INDEX idx_asset_trash_status_time ON asset_trash (status, trashed_at DESC);
-- 恢复时需要判断「同一路径是否已有一条在回收站的记录」（配合 UNIQUE 近似唯一约束）。
CREATE UNIQUE INDEX idx_asset_trash_active_path
  ON asset_trash (repository_id, path) WHERE status = 'staged';
-- blob 引用判定：每次孤立回收都要问「这个 hash 是否仍被回收站引用」，
-- 这是热路径（blob-gc 会对每个活动 blob 问一次），必须有索引。
CREATE INDEX idx_asset_trash_blob ON asset_trash (blob_hash);
