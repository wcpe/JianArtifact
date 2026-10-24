-- 0051：上游健康历史的分钟级采样表 upstream_health_minute
-- 引入版本：未开窗（0.11.0 之后的下一窗口）
-- 影响：新增表 upstream_health_minute 与索引 idx_upstream_health_repo_bucket；不改动任何既有表
-- 数据处理：无历史回填——新表从本迁移生效时刻起才开始积累，不伪造历史（规格 §1.3「不做历史数据回填」）
-- 回滚：不支持 down migration；需要恢复升级前备份。本表只承载新增的观测数据，
--       直接 DROP TABLE upstream_health_minute 与 DROP INDEX idx_upstream_health_repo_bucket
--       即可回到迁移前的结构，不影响任何既有功能读取
-- 相关：上游健康历史（docs/specs/0.13.0-upstream-health-history.md）、
--       当前连接状态展示、回源失败原因五分类、storage-cleanup 周期作业（30 天保留）
--
-- 背景：既有的「上游当前连接状态」是**内存态**——重启归零，只有一个「此刻」，
-- 没有「过去一小时连通率多少」「昨天挂过几次、每次多久恢复」。断路器侧的
-- jianartifact_upstream_block_events_total 是累计计数器，没有时间维度，无法还原趋势。
-- 本表把「多次请求的可连接性与健康」持久化成时间序列，作为当前状态的**历史补充**
-- （不替换、不改动既有的六态展示）。
--
-- 两路来源写入同一张表，用 source 区分：
--   - source='request'：每次真实回源请求结束时记一次，反映**真实用户可用性**；
--   - source='probe'：后台定时 HEAD 探测，无流量时补空窗并提前发现上游挂掉。
-- source 进主键是硬性要求：两条序列混在一行会让「连通率」被探测流量稀释，
-- 从而无法分别判断「探测 100% 成功但真实请求 60%」这一关键信号（规格 §2.2）。
--
-- 列口径：
--   - samples 是总样本数；ok + timeout + unreachable + slow + aborted 不一定等于 samples，
--     因为 local_write_failure（本地磁盘故障）与 canceled（客户端断连 / 同组落选 / 停机）
--     **不计入任何列**——前者不是上游故障，后者是正常的请求生命周期终点，
--     两者都不是上游健康信号（规格 §2.2）。
--   - 失败列与回源失败的五分类对齐（classifyUpstreamFailure / classifyTransferFailure 的
--     category 取值），采样处不再独立判定一次——两处判定必然漂移。
--   - avg_latency_ms / max_latency_ms 只在**成功样本**上统计；无成功样本时为 NULL，
--     不写 0：0ms 是一个不存在的延迟读数，会在趋势图上伪造「极快」的假象。
--
-- repository_id 声明 REFERENCES repository (id) ON DELETE CASCADE：仓库删除时本表的采样
-- 随之消失，不留「指向已不存在仓库」的孤儿行。连接串已开启 foreign_keys(ON)
-- （见 persistence/db.go 的 _pragma），因此该级联是真实生效的约束，不是装饰性注释。
--
-- 60 分钟桶按 UTC 取整（'YYYY-MM-DD HH:MM:00'），与 host_metric_minute 的既有口径一致，
-- 也使 Go 侧算出的时间窗可与表内文本列直接做字典序比较。
--
-- 索引口径：主键 (bucket_start, repository_id, source) 的最左列是 bucket_start，
-- 适合「按时间窗扫全仓」；而查询 API 的主用法是 `WHERE repository_id=? AND bucket_start
-- BETWEEN ? AND ?`，因此另建 (repository_id, bucket_start) 让等值列在前、范围列在后，
-- 同时覆盖「按仓库取最近 N 个桶」的 ORDER BY bucket_start DESC 倒序扫描。
-- 保留清理（30 天）只按 bucket_start 裁剪，可走主键，无需额外索引。
--
-- 幂等性说明：迁移器把每个文件放在单个事务里执行（见 persistence/migrate.go），
-- 建表 / 建索引与 schema_migrations 记录同事务提交，不存在「表已建但版本未记」的半成品状态，
-- 因此这里按 0043 / 0044 / 0045 / 0050 的既有写法用裸 CREATE，不加 IF NOT EXISTS。
CREATE TABLE upstream_health_minute (
  bucket_start TEXT NOT NULL,
  repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  samples INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  timeout INTEGER NOT NULL,
  unreachable INTEGER NOT NULL,
  slow INTEGER NOT NULL,
  aborted INTEGER NOT NULL,
  avg_latency_ms INTEGER,
  max_latency_ms INTEGER,
  PRIMARY KEY (bucket_start, repository_id, source)
);

CREATE INDEX idx_upstream_health_repo_bucket ON upstream_health_minute (repository_id, bucket_start);
