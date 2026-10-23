-- 0051：清理任务队列与回收站条目
-- 引入版本：未开窗（0.13.0 窗口）
-- 影响：新增两张表（clean_task / trash_entry）与四个索引；不改动任何既有表
-- 数据处理：无历史回填，纯建表
-- 回滚：不支持 down migration；需要恢复升级前备份（本迁移只新增表，旧版本代码不认识它们，回滚代码即可继续运行）
-- 相关：维护与清理控制台（docs/specs/0.13.0-maintenance-cleanup-console.md）
--
-- 为什么要落库而不是只在内存里记：
--   清理是**破坏性**操作，运维必须能回答「上次清的是什么、删了多少、有没有失败」，而进程重启
--   会把内存状态清空。clean_task 同时还是「重启收口」的载体——启动时把残留的 running 标成
--   interrupted，才能如实说明「上次没跑完」，而不是让一条记录永远停在 running。
--
-- 为什么回收站条目要单独一张表（而不是只在 clean_task 里存 JSON）：
--   恢复与彻底删除都是**逐条**动作（某一项恢复失败不该连累其余），逐条独立行才能精确表达
--   staged / restored / purged 的迁移；塞进 JSON 会让"只恢复其中 3 项"变成整行重写。
--
-- 字段口径与 migration_task 对齐（status / 进度 / 错误 / 时间戳），不另造抽象。

CREATE TABLE clean_task (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  -- 清理类别：empty_dirs | stale_uploads | pre_restore | quarantine
  kind             TEXT NOT NULL,
  -- queued 排队中 / running 执行中 / succeeded 成功 / failed 失败 / interrupted 进程重启中断
  status           TEXT NOT NULL,
  total_items      INTEGER NOT NULL DEFAULT 0,
  processed_items  INTEGER NOT NULL DEFAULT 0,
  -- 释放字节只统计**真正腾出磁盘**的部分：移入回收站时仍占盘，故 succeeded 前该值为 0，
  -- 由回收站的 purged 事件累加。保留字段是为让历史能说明"这次清理最终省了多少"。
  freed_bytes      INTEGER NOT NULL DEFAULT 0,
  error_message    TEXT,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  started_at       TEXT,
  finished_at      TEXT
);

CREATE INDEX idx_clean_task_status ON clean_task(status);
CREATE INDEX idx_clean_task_created ON clean_task(created_at);

CREATE TABLE trash_entry (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  -- 归属任务；任务删除时级联清理（回收站条目没有独立存在的意义）
  task_id          INTEGER NOT NULL REFERENCES clean_task(id) ON DELETE CASCADE,
  kind             TEXT NOT NULL,
  -- 移入前的绝对路径，恢复时原样放回
  original_path    TEXT NOT NULL,
  -- 回收站内的落盘位置（data/trash/<id>/...）
  trash_path       TEXT NOT NULL,
  is_dir           INTEGER NOT NULL DEFAULT 0,
  bytes            INTEGER NOT NULL DEFAULT 0,
  -- staged 在回收站 / restored 已恢复 / purged 已彻底删除
  status           TEXT NOT NULL,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  resolved_at      TEXT
);

CREATE INDEX idx_trash_entry_status ON trash_entry(status);
CREATE INDEX idx_trash_entry_created ON trash_entry(created_at);
