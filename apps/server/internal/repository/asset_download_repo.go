package repository

import (
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
