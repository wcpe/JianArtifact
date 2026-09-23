package repository

import (
	"fmt"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AssetDownloadMinute 是「UTC 分钟 × 仓库 × 制品 × 来源 IP × 客户端归类」的下载计数。
//
// 设计见 docs/specs/download-metrics.md：按分钟预聚合以控制行数膨胀；ua_family 是
// 归类结果（原始 UA 串不落库）；client_ip 明文仅供管理员查询；只统计完整传输
// （GET + 200，Range 分段不计），采集与归类的判断在 protocol 层完成。
type AssetDownloadMinute struct {
	BucketStart   string `db:"bucket_start"`
	Repo          string `db:"repo"`
	AssetPath     string `db:"asset_path"`
	ClientIP      string `db:"client_ip"`
	UAFamily      string `db:"ua_family"`
	DownloadCount int64  `db:"download_count"`
}

// AssetDownloadRepo 读写制品下载计量明细（asset_download_minutes）。
type AssetDownloadRepo struct {
	db *persistence.DB
}

func NewAssetDownloadRepo(db *persistence.DB) *AssetDownloadRepo {
	return &AssetDownloadRepo{db: db}
}

// AddMinutes 批量幂等累加：同（分钟, 仓库, 制品, IP, UA 族）在主键上 ON CONFLICT 累加，
// 因此跨 flush 周期的重复写入天然合并；单次调用走一个事务 + 预编译语句，摊薄采集端开销。
func (r *AssetDownloadRepo) AddMinutes(items []AssetDownloadMinute) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO asset_download_minutes
		(bucket_start, repo, asset_path, client_ip, ua_family, download_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_start, repo, asset_path, client_ip, ua_family) DO UPDATE SET
		download_count = download_count + excluded.download_count`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, item := range items {
		if _, err := stmt.Exec(item.BucketStart, item.Repo, item.AssetPath, item.ClientIP, item.UAFamily, item.DownloadCount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SumByAsset 返回该仓库每个制品的累计下载次数（**原始口径**，不做去重——去重是查询视角，
// 见 spec §3.3；原始数据不丢）。
func (r *AssetDownloadRepo) SumByAsset(repo string) (map[string]int64, error) {
	rows, err := r.db.Query(
		`SELECT asset_path, SUM(download_count) FROM asset_download_minutes WHERE repo = ? GROUP BY asset_path`,
		repo,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int64)
	for rows.Next() {
		var path string
		var count int64
		if err := rows.Scan(&path, &count); err != nil {
			return nil, err
		}
		out[path] = count
	}
	return out, rows.Err()
}

// PurgeBefore 清理早于 cutoff 的分钟桶（30 天保留，接入既有观测清理周期）。
func (r *AssetDownloadRepo) PurgeBefore(cutoff time.Time) (int64, error) {
	result, err := r.db.Exec(
		`DELETE FROM asset_download_minutes WHERE bucket_start < ?`,
		formatMetricTime(cutoff),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DownloadTrendBucket 是下载累计趋势的一个时间桶（Bucket 为 RFC3339 起点字符串）。
type DownloadTrendBucket struct {
	Bucket string
	Count  int64
}

// DownloadClientIPCount / DownloadClientFamilyCount 是来源排名的行。
type DownloadClientIPCount struct {
	IP    string
	Count int64
}

type DownloadClientFamilyCount struct {
	Family string
	Count  int64
}

// metricBucketFormat 返回分桶截断用的 strftime 格式。桶粒度与 api 层 operationsBucket
// 产出的 minute/hour/day 对齐，截断结果统一落在 UTC 桶起点（minute 为默认——明细表
// 本身就是分钟预聚合，保底不粗于原始粒度）。
func metricBucketFormat(bucket string) string {
	switch bucket {
	case "hour":
		return "%Y-%m-%dT%H:00:00Z"
	case "day":
		return "%Y-%m-%dT00:00:00Z"
	default:
		return "%Y-%m-%dT%H:%M:00Z"
	}
}

// assetDownloadMinuteRangeBounds 将精确时间窗 [from,to) 对齐到下载明细的分钟桶起点。
// 明细每行已是整分钟聚合，因此只纳入分钟起点落在请求窗内的行；上界按分钟向上取整，
// 再由 SQL 严格 `<` 排除 to 边界，避免 RFC3339 小数秒的文本比较和额外终点桶。
func assetDownloadMinuteRangeBounds(from, to time.Time) (time.Time, time.Time) {
	fromUTC, toUTC := from.UTC(), to.UTC()
	start := fromUTC.Truncate(time.Minute)
	if start.Before(fromUTC) {
		start = start.Add(time.Minute)
	}
	end := toUTC.Truncate(time.Minute)
	if end.Before(toUTC) {
		end = end.Add(time.Minute)
	}
	return start, end
}

// DownloadTrend 按粒度（minute/hour/day）聚合下载累计（原始口径，不去重）。明细表是
// 「组合级」行（分钟 × 仓库 × 制品 × IP × UA）——故聚合下推到
// SQL（strftime 截断 + GROUP BY），不读全量进内存；只纳入分钟桶起点落在 [from,to) 的样本。
func (r *AssetDownloadRepo) DownloadTrend(from, to time.Time, bucket string) ([]DownloadTrendBucket, error) {
	rangeFrom, rangeTo := assetDownloadMinuteRangeBounds(from, to)
	rows, err := r.db.Query(
		`SELECT strftime(?, bucket_start) AS bucket, SUM(download_count)
		 FROM asset_download_minutes WHERE bucket_start >= ? AND bucket_start < ?
		 GROUP BY bucket ORDER BY bucket`,
		metricBucketFormat(bucket), formatMetricTime(rangeFrom), formatMetricTime(rangeTo),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]DownloadTrendBucket, 0)
	for rows.Next() {
		var item DownloadTrendBucket
		if err := rows.Scan(&item.Bucket, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// DownloadClientRanking 返回「独立来源」口径的 IP Top N 与客户端族分布：同 IP + 同制品
// 在同一小时窗口内只计一次贡献（对 小时桶 × 仓库 × 制品 × 维度 做 DISTINCT 再计数）。
// 原始累计口径见 DownloadTrend；此处刻意不返回原始 UA 串（ua_family 为归类结果）。
func (r *AssetDownloadRepo) DownloadClientRanking(from, to time.Time, topIPs int) ([]DownloadClientIPCount, []DownloadClientFamilyCount, error) {
	// 排名只纳入分钟桶起点位于 [from,to) 的样本，再按小时去重；to 上界严格不含。
	rangeFrom, rangeTo := assetDownloadMinuteRangeBounds(from, to)
	fromStr, toStr := formatMetricTime(rangeFrom), formatMetricTime(rangeTo)
	ipRows, err := r.db.Query(
		`SELECT client_ip, COUNT(*) FROM (
		   SELECT DISTINCT client_ip, repo, asset_path, strftime('%Y-%m-%dT%H', bucket_start) AS hour
		   FROM asset_download_minutes WHERE bucket_start >= ? AND bucket_start < ?
		 ) GROUP BY client_ip ORDER BY COUNT(*) DESC, client_ip LIMIT ?`,
		fromStr, toStr, topIPs,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = ipRows.Close() }()
	ips := make([]DownloadClientIPCount, 0)
	for ipRows.Next() {
		var item DownloadClientIPCount
		if err := ipRows.Scan(&item.IP, &item.Count); err != nil {
			return nil, nil, err
		}
		ips = append(ips, item)
	}
	if err := ipRows.Err(); err != nil {
		return nil, nil, err
	}

	familyRows, err := r.db.Query(
		`SELECT ua_family, COUNT(*) FROM (
		   SELECT DISTINCT ua_family, repo, asset_path, client_ip, strftime('%Y-%m-%dT%H', bucket_start) AS hour
		   FROM asset_download_minutes WHERE bucket_start >= ? AND bucket_start < ?
		 ) GROUP BY ua_family ORDER BY COUNT(*) DESC, ua_family`,
		fromStr, toStr,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = familyRows.Close() }()
	families := make([]DownloadClientFamilyCount, 0)
	for familyRows.Next() {
		var item DownloadClientFamilyCount
		if err := familyRows.Scan(&item.Family, &item.Count); err != nil {
			return nil, nil, err
		}
		families = append(families, item)
	}
	return ips, families, familyRows.Err()
}

// SumPaths 返回给定制品路径集合的累计次数（仓库详情树层批量展示用；单次 IN 查询，
// 命中 (repo, asset_path, bucket_start) 索引）。空集合返回空 map，不发起查询。
func (r *AssetDownloadRepo) SumPaths(repo string, paths []string) (map[string]int64, error) {
	out := make(map[string]int64)
	if len(paths) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, 0, len(paths)+1)
	args = append(args, repo)
	for _, path := range paths {
		args = append(args, path)
	}
	rows, err := r.db.Query(
		`SELECT asset_path, SUM(download_count) FROM asset_download_minutes
		 WHERE repo = ? AND asset_path IN (`+placeholders+`) GROUP BY asset_path`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path string
		var count int64
		if err := rows.Scan(&path, &count); err != nil {
			return nil, err
		}
		out[path] = count
	}
	return out, rows.Err()
}

// DownloadGroupKey 是分组下载时序里的分组维度取值：ip 为来源 IP 明文（仅管理员可见），
// family 为 UA 归类结果（不含原始 UA 串）。
type DownloadGroupKey string

const (
	DownloadGroupKeyIP     DownloadGroupKey = "ip"
	DownloadGroupKeyFamily DownloadGroupKey = "family"
)

// DownloadGroupedBucket 是分组下载时序的一行：Bucket 为截断后的桶起点 RFC3339 字符串
// （即桶标签），Group 为该行的分组取值，Count 为该桶该组内的下载次数合计。
// 返回行按 (Bucket, Group) 升序；空区间返回空切片。
type DownloadGroupedBucket struct {
	Bucket string
	Group  string
	Count  int64
}

// DownloadTrendGrouped 按「截断后的桶 × 分组列」聚合下载时序（原始累计口径，不去重），
// 供仪表盘分组趋势图与饼图使用。groupBy ∈ {ip, family}，非法值直接报错（调用方在
// handler 层做参数校验）。repo 为空串表示全局口径；非空时按仓库过滤——过滤形态为
// `repo = ? AND bucket_start >= ? AND bucket_start < ?`，可走 idx_asset_download_asset 的 repo 前缀。
// 桶粒度复用 api 层 operationsBucket 的 minute/hour/day；聚合仍下推 SQL，不读全量进内存。
func (r *AssetDownloadRepo) DownloadTrendGrouped(from, to time.Time, bucket string, groupBy DownloadGroupKey, repo string) ([]DownloadGroupedBucket, error) {
	var groupColumn string
	switch groupBy {
	case DownloadGroupKeyIP:
		groupColumn = "client_ip"
	case DownloadGroupKeyFamily:
		groupColumn = "ua_family"
	default:
		return nil, fmt.Errorf("不支持的分组维度：%s", groupBy)
	}
	// 分组列名来自白名单 switch（非用户输入拼接），值仍以绑定参数传入。
	// 查询只纳入分钟桶起点位于 [from,to) 的样本，再按目标粒度分组；首尾目标桶可以是部分桶。
	rangeFrom, rangeTo := assetDownloadMinuteRangeBounds(from, to)
	query := `SELECT strftime(?, bucket_start) AS bucket, ` + groupColumn + ` AS grp, SUM(download_count)
		 FROM asset_download_minutes WHERE bucket_start >= ? AND bucket_start < ?`
	args := []any{metricBucketFormat(bucket), formatMetricTime(rangeFrom), formatMetricTime(rangeTo)}
	if repo != "" {
		query += ` AND repo = ?`
		args = append(args, repo)
	}
	query += ` GROUP BY bucket, grp ORDER BY bucket, grp`
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]DownloadGroupedBucket, 0)
	for rows.Next() {
		var item DownloadGroupedBucket
		if err := rows.Scan(&item.Bucket, &item.Group, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// DownloadTrendForRepo 返回单仓库的下载累计趋势（仓库详情页下载趋势图）。
// 过滤形态为 `repo = ? AND bucket_start >= ? AND bucket_start < ?`：纯桶范围 + repo 等值过滤
// 可走 idx_asset_download_asset (repo, asset_path, bucket_start) 的 repo 前缀缩小扫描集。
// 查询只纳入分钟桶起点位于 [from,to) 的样本，再按目标粒度聚合；空区间返回空切片。
func (r *AssetDownloadRepo) DownloadTrendForRepo(repo string, from, to time.Time, bucket string) ([]DownloadTrendBucket, error) {
	rangeFrom, rangeTo := assetDownloadMinuteRangeBounds(from, to)
	rows, err := r.db.Query(
		`SELECT strftime(?, bucket_start) AS bucket, SUM(download_count)
		 FROM asset_download_minutes WHERE repo = ? AND bucket_start >= ? AND bucket_start < ?
		 GROUP BY bucket ORDER BY bucket`,
		metricBucketFormat(bucket), repo, formatMetricTime(rangeFrom), formatMetricTime(rangeTo),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]DownloadTrendBucket, 0)
	for rows.Next() {
		var item DownloadTrendBucket
		if err := rows.Scan(&item.Bucket, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SumByRepo 返回单仓库全时段累计下载次数（**原始口径**，与 SumByAsset 同口径求和），
// 供仓库详情页展示仓库总下载。命中 idx_asset_download_asset 的 repo 前缀；无数据返回 0。
func (r *AssetDownloadRepo) SumByRepo(repo string) (int64, error) {
	var total int64
	if err := r.db.Get(&total,
		`SELECT COALESCE(SUM(download_count), 0) FROM asset_download_minutes WHERE repo = ?`,
		repo,
	); err != nil {
		return 0, err
	}
	return total, nil
}
