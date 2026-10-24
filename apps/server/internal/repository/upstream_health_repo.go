package repository

import (
	"fmt"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件实现上游健康历史的持久化层：分钟桶的累加写、按仓库/时间窗读、保留清理。
//
// 分层口径与 OperationsObservabilityRepo 一致：读写原语在 repository 层，
// 「什么时候记一次样本」「采样是否需要异步批量」这类编排留在 domain 层。
// 两者的关键区别只有一点：本表的写发生在**回源主路径**上，因此写入接口接受的是
// 「增量」而非「最终值」——只有增量才能被合并成批量写，也只有增量能避免读-改-写竞态。
//
// 上游健康采样的两路来源。它们是**两条独立序列**，不能混在一行里统计：
// 混在一起会让「连通率」被探测流量稀释，从而看不出「探测 100% 成功但真实请求 60%」
// 这一最关键的分化（规格 §2.2 与 §2.4）。
const (
	// UpstreamHealthSourceRequest 表示样本来自真实回源请求，反映真实用户可用性。
	UpstreamHealthSourceRequest = "request"
	// UpstreamHealthSourceProbe 表示样本来自后台定时 HEAD 探测，用于补齐无流量时的空窗。
	UpstreamHealthSourceProbe = "probe"
)

// 分钟桶里统计的失败类别列名（与回源失败的五分类对齐）。
// 只有这四类计入列；local_write_failure（本地故障）与 canceled（正常取消）
// **不进入任何列**——它们不是上游健康信号（规格 §2.2）。
const (
	// UpstreamHealthFailureTimeout 对应 no_response（未拿到响应头即超时 / 拿到头但零字节）。
	UpstreamHealthFailureTimeout = "timeout"
	// UpstreamHealthFailureUnreachable 对应 unreachable（连接 / DNS / TLS 失败、上游 5xx / 401 / 403）。
	UpstreamHealthFailureUnreachable = "unreachable"
	// UpstreamHealthFailureSlow 对应 slow_rate（有传输进展但未在超时内传完）。
	UpstreamHealthFailureSlow = "slow"
	// UpstreamHealthFailureAborted 对应 transfer_aborted（传输中途被中断）。
	UpstreamHealthFailureAborted = "aborted"
)

// UpstreamHealthDelta 是**一批采样的增量**，不是分钟桶的最终值。
// 单次采样就是「Samples = 1」的退化情形，因此单次与批量共用同一条 SQL 路径，
// 不会出现「单次写与批量写口径不同」的第二套实现。
//
// 为什么是增量而不是绝对值：分钟桶走 upsert 累加（samples = samples + ?），
// 读-改-写在并发回源下必然丢样本（两个请求读到同一个基数各加一次）。
type UpstreamHealthDelta struct {
	// BucketStart 是分钟桶起点，文本格式 'YYYY-MM-DD HH:MM:00'（UTC）。
	BucketStart  string
	RepositoryID int64
	// Source 只允许 UpstreamHealthSourceRequest / UpstreamHealthSourceProbe。
	Source string
	// Samples 是本批计入的采样总数。ok + timeout + unreachable + slow + aborted 之和
	// **应当等于** Samples；调用方负责把每一类都归到某个列（见 domain 侧的唯一映射）。
	Samples     int64
	OK          int64
	Timeout     int64
	Unreachable int64
	Slow        int64
	Aborted     int64
	// LatencySumMs / LatencyCountMs 是**成功样本**的延迟总和与样本数（两者都由 OK 派生的
	// 部分分别维护：CountMs 恒等于 OK，单列是为了让平均值的递推式在 SQL 里可读）。
	LatencySumMs   int64
	LatencyCountMs int64
	// MaxLatencyMs 是本批成功样本中的最大延迟。
	MaxLatencyMs int64
}

// AddSamples 把增量累加进唯一一场（合并批次用）。
func (d *UpstreamHealthDelta) AddSamples(other UpstreamHealthDelta) {
	d.Samples += other.Samples
	d.OK += other.OK
	d.Timeout += other.Timeout
	d.Unreachable += other.Unreachable
	d.Slow += other.Slow
	d.Aborted += other.Aborted
	d.LatencySumMs += other.LatencySumMs
	d.LatencyCountMs += other.LatencyCountMs
	if other.MaxLatencyMs > d.MaxLatencyMs {
		d.MaxLatencyMs = other.MaxLatencyMs
	}
}

// UpstreamHealthBucket 是 upstream_health_minute 的一行（分钟桶）。
type UpstreamHealthBucket struct {
	BucketStart  string `db:"bucket_start"`
	RepositoryID int64  `db:"repository_id"`
	Source       string `db:"source"`
	Samples      int64  `db:"samples"`
	OK           int64  `db:"ok"`
	Timeout      int64  `db:"timeout"`
	Unreachable  int64  `db:"unreachable"`
	Slow         int64  `db:"slow"`
	Aborted      int64  `db:"aborted"`
	// AvgLatencyMs / MaxLatencyMs 只在成功样本上统计；无成功样本时为 nil（NULL），不伪造 0。
	AvgLatencyMs *int64 `db:"avg_latency_ms"`
	MaxLatencyMs *int64 `db:"max_latency_ms"`
}

// Failed 返回该桶里计入失败列的样本数（不含未归类的样本）。
func (b UpstreamHealthBucket) Failed() int64 {
	return b.Timeout + b.Unreachable + b.Slow + b.Aborted
}

// UpstreamHealthRepo 是上游健康历史表的读写入口。
type UpstreamHealthRepo struct{ db *persistence.DB }

// NewUpstreamHealthRepo 构造上游健康历史仓储。
func NewUpstreamHealthRepo(db *persistence.DB) *UpstreamHealthRepo {
	return &UpstreamHealthRepo{db: db}
}

// upstreamHealthBucketLayout 是本表 bucket_start 的文本格式：UTC、分钟取整、秒恒为 00。
// 与 formatMetricTime 同形，使本表与 host_metric_minute 的时间口径一致，
// 也让「Go 侧算出的时间窗」与「表内文本列」可以直接做字典序比较。
const upstreamHealthBucketLayout = "2006-01-02 15:04:05"

// UpstreamHealthBucketStart 把任意时刻对齐到分钟桶起点（UTC，秒恒为 00）。
// 采样写入与查询窗口过滤共用本函数，避免两处各写一套取整逻辑而漂移。
func UpstreamHealthBucketStart(t time.Time) string {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), utc.Minute(), 0, 0, time.UTC).
		Format(upstreamHealthBucketLayout)
}

// AddSamples 把一批增量累加进它所属的分钟桶。
//
// 累加（而不是覆盖）是**需求**而非优化：同一分钟内多次请求必须累加成一行
// （规格 §5.1「分钟桶累加」），跨分钟自动分行由主键 bucket_start 决定。
//
// 平均延迟的递推式：新平均 = (旧平均×旧成功数 + 本批延迟和) ÷ (旧成功数 + 本批成功数)。
// 旧成功数为 0 时 COALESCE 让旧平均按 0 参与，结果正是「本批延迟和 ÷ 本批成功数」。
// 所有运算都以**旧行值**为输入并在单条 SQL 内完成，因此并发 upsert 之间由 SQLite
// 单写者语义串行化，不存在「读到旧值再算」的丢更新窗口。
//
// 本批无成功样本时两列**完全不参与更新**：因此一整分钟只有失败时 avg/max 保持 NULL，
// 这正是「无成功样本时为 NULL，不伪造 0」的落点。
func (r *UpstreamHealthRepo) AddSamples(delta UpstreamHealthDelta) error {
	if delta.RepositoryID <= 0 || delta.Source == "" {
		return fmt.Errorf("上游健康增量缺少仓库或来源")
	}
	if delta.Samples <= 0 {
		return fmt.Errorf("上游健康增量样本数必须为正")
	}
	counted := delta.OK + delta.Timeout + delta.Unreachable + delta.Slow + delta.Aborted
	if counted != delta.Samples {
		// 这条不变式一旦破坏，连通率的分母与分子就不再自洽（会算出 >100% 或空洞），
		// 因此在写入前直接拒绝，而不是把坏数据落库。
		return fmt.Errorf("上游健康增量分类不自洽：samples=%d 分类合计=%d", delta.Samples, counted)
	}
	// avg 递推所需的两项只在本批有成功样本时有意义；无成功样本时下发 NULL 让 CASE 走「保持原值」。
	var latencySum, latencyCount, maxLatency any
	if delta.LatencyCountMs > 0 {
		latencySum = delta.LatencySumMs
		latencyCount = delta.LatencyCountMs
		maxLatency = delta.MaxLatencyMs
	}
	_, err := r.db.Exec(`INSERT INTO upstream_health_minute
		(bucket_start, repository_id, source, samples, ok, timeout, unreachable, slow, aborted, avg_latency_ms, max_latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?,
			CASE WHEN ? IS NULL THEN NULL ELSE CAST(ROUND(? * 1.0 / ?) AS INTEGER) END,
			?)
		ON CONFLICT(bucket_start, repository_id, source) DO UPDATE SET
		samples = samples + excluded.samples,
		ok = ok + excluded.ok,
		timeout = timeout + excluded.timeout,
		unreachable = unreachable + excluded.unreachable,
		slow = slow + excluded.slow,
		aborted = aborted + excluded.aborted,
		avg_latency_ms = CASE WHEN excluded.ok > 0
			THEN CAST(ROUND((COALESCE(avg_latency_ms, 0) * ok + ? * 1.0) / (ok + ?)) AS INTEGER)
			ELSE avg_latency_ms END,
		max_latency_ms = CASE WHEN excluded.ok > 0
			THEN MAX(COALESCE(max_latency_ms, 0), ?)
			ELSE max_latency_ms END`,
		delta.BucketStart, delta.RepositoryID, delta.Source,
		delta.Samples, delta.OK, delta.Timeout, delta.Unreachable, delta.Slow, delta.Aborted,
		latencyCount, latencySum, latencyCount,
		maxLatency,
		latencySum, latencyCount, maxLatency)
	return err
}

// upstreamHealthWindowEnd 把窗口右边界对齐成「可比较的排他上界」。
//
// 为什么不能像 host_metric_minute 那样直接 formatMetricTime：本表的 bucket_start 是
// 'YYYY-MM-DD HH:MM:00' 而不是 RFC3339Nano，两种文本的字典序不可比（`2026-08-27T10:00:00Z`
// 与 `2026-08-27 10:00:00` 在第一个不同字符处就分出大小）。因此查询边界的格式化
// 必须用本表布局，而不是复用别的表的。
//
// 向上取整的理由：`bucket_start < to` 是排他比较，而 to 带亚分钟精度时（如 10:00:30）
// 直接截断成 10:00:00 会把 10:00 这一分钟整桶排掉——那正是用户点「最近 1 小时」最想看到的
// 当前分钟。因此只要 to 不是整分钟，就补一分钟再截断，让 10:00 桶被包含进来。
func upstreamHealthWindowEnd(t time.Time) string {
	utc := t.UTC()
	if utc.Second() != 0 || utc.Nanosecond() != 0 {
		utc = utc.Add(time.Minute)
	}
	return UpstreamHealthBucketStart(utc)
}

// UpstreamHealthBuckets 返回 [from, to) 内某仓库的分钟桶，按 (source, bucket_start) 升序。
// source 为空表示两路来源都取（调用方据此分别展示，或自行合并）。
//
// 边界口径为左闭右开，与 ProtocolMinutes 等既有查询一致：连续窗拼接不会重复计同一分钟。
func (r *UpstreamHealthRepo) UpstreamHealthBuckets(repositoryID int64, from, to time.Time, source string) ([]UpstreamHealthBucket, error) {
	items := []UpstreamHealthBucket{}
	fromValue, toValue := UpstreamHealthBucketStart(from), upstreamHealthWindowEnd(to)
	query := `SELECT bucket_start, repository_id, source, samples, ok, timeout, unreachable, slow, aborted,
		avg_latency_ms, max_latency_ms FROM upstream_health_minute
		WHERE repository_id = ? AND bucket_start >= ? AND bucket_start < ?`
	args := []any{repositoryID, fromValue, toValue}
	if source != "" {
		query += ` AND source = ?`
		args = append(args, source)
	}
	// source 参与排序：同一分钟两路来源各占一行，按来源分组后前端可各自成序列。
	err := r.db.Select(&items, query+` ORDER BY source, bucket_start`, args...)
	return items, err
}

// UpstreamHealthRecent 返回某仓库最近 limit 个分钟桶（按 bucket_start 倒序，两路来源都含）。
//
// 「最近 N 次明细」在规格 §2.3 / §6 R3 里明确采用**分钟桶展开为明细摘要**的保守口径，
// 不是逐请求日志：同分钟内多次请求只能看到一行聚合摘要。本查询就是该口径的取数入口。
func (r *UpstreamHealthRepo) UpstreamHealthRecent(repositoryID int64, limit int) ([]UpstreamHealthBucket, error) {
	items := []UpstreamHealthBucket{}
	if limit <= 0 {
		return items, nil
	}
	err := r.db.Select(&items, `SELECT bucket_start, repository_id, source, samples, ok, timeout, unreachable, slow, aborted,
		avg_latency_ms, max_latency_ms FROM upstream_health_minute
		WHERE repository_id = ? ORDER BY bucket_start DESC, source LIMIT ?`, repositoryID, limit)
	return items, err
}

// UpstreamHealthRetention 是上游健康历史的保留期：固定 30 天，与 host_metric_minute 的既有口径对齐
// （规格 §2.2：「保留 30 天，由 storage-cleanup 顺带清理，不新增作业」）。
// 这里刻意**不**做成可配项：spec 未要求可配，多一个开关就多一处「配成 0 等于永不清理」的磁盘风险。
const UpstreamHealthRetention = 30 * 24 * time.Hour

// PruneUpstreamHealth 删除 bucket_start 早于 cutoff 的采样行，返回删除行数。
//
// **只删除本表**：清理范围必须精确到表名，否则 storage-cleanup 的「顺带清理」
// 会波及 host_metric_minute 等同类分钟表（规格 §5.1 有专门的用例钉住这一点）。
func (r *UpstreamHealthRepo) PruneUpstreamHealth(cutoff string) (int, error) {
	if r == nil {
		return 0, nil
	}
	result, err := r.db.Exec(`DELETE FROM upstream_health_minute WHERE bucket_start < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理上游健康历史：%w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}
