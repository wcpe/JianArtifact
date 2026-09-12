package repository

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// PublishPolicyRepo 读写发布账号策略与配额预留。
type PublishPolicyRepo struct{ db *persistence.DB }

// NewPublishPolicyRepo 构造发布策略仓储。
func NewPublishPolicyRepo(db *persistence.DB) *PublishPolicyRepo { return &PublishPolicyRepo{db: db} }

// Get 返回用户×仓库策略；未配置返回 ErrNotFound。
func (r *PublishPolicyRepo) Get(userID, repoID int64) (*PublishPolicy, error) {
	var row struct {
		PublishPolicy
		Prefixes string `db:"path_prefixes_json"`
	}
	err := r.db.Get(&row, `SELECT user_id, repository_id, path_prefixes_json, max_assets_hour, max_bytes_day, max_file_bytes, updated_at
		FROM publish_policy WHERE user_id = ? AND repository_id = ?`, userID, repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.Prefixes), &row.PathPrefixes); err != nil {
		return nil, err
	}
	return &row.PublishPolicy, nil
}

// Upsert 保存发布策略；路径前缀与额度在领域层校验。
func (r *PublishPolicyRepo) Upsert(p PublishPolicy) error {
	prefixes, err := json.Marshal(p.PathPrefixes)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`INSERT INTO publish_policy
		(user_id, repository_id, path_prefixes_json, max_assets_hour, max_bytes_day, max_file_bytes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, repository_id) DO UPDATE SET
		path_prefixes_json = excluded.path_prefixes_json,
		max_assets_hour = excluded.max_assets_hour,
		max_bytes_day = excluded.max_bytes_day,
		max_file_bytes = excluded.max_file_bytes,
		updated_at = datetime('now')`, p.UserID, p.RepositoryID, string(prefixes), p.MaxAssetsHour, p.MaxBytesDay, p.MaxFileBytes)
	return err
}

// Delete 删除发布策略，恢复为仓库默认额度和全路径。
func (r *PublishPolicyRepo) Delete(userID, repoID int64) error {
	res, err := r.db.Exec(`DELETE FROM publish_policy WHERE user_id = ? AND repository_id = ?`, userID, repoID)
	return affected(res, err)
}

// Reserve 原子创建一笔上传预留，并按当前小时/日期统计 used+reserved。
func (r *PublishPolicyRepo) Reserve(p PublishPolicy, assets, bytes int64) (int64, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	hour := "strftime('%Y-%m-%dT%H:00:00Z', 'now')"
	day := "strftime('%Y-%m-%dT00:00:00Z', 'now')"
	if p.MaxAssetsHour > 0 {
		var used, reserved int64
		err = tx.QueryRowx(`SELECT
			COALESCE((SELECT used_assets FROM quota_usage WHERE user_id = ? AND repository_id = ? AND window_kind = 'hour' AND window_start = `+hour+`),0),
			COALESCE((SELECT SUM(assets) FROM quota_reservation WHERE user_id = ? AND repository_id = ? AND settled_at IS NULL),0)`,
			p.UserID, p.RepositoryID, p.UserID, p.RepositoryID).Scan(&used, &reserved)
		if err != nil || used+reserved+assets > p.MaxAssetsHour {
			return 0, quotaExceeded(err)
		}
	}
	if p.MaxBytesDay > 0 {
		var used, reserved int64
		err = tx.QueryRowx(`SELECT
			COALESCE((SELECT used_bytes FROM quota_usage WHERE user_id = ? AND repository_id = ? AND window_kind = 'day' AND window_start = `+day+`),0),
			COALESCE((SELECT SUM(bytes) FROM quota_reservation WHERE user_id = ? AND repository_id = ? AND settled_at IS NULL),0)`,
			p.UserID, p.RepositoryID, p.UserID, p.RepositoryID).Scan(&used, &reserved)
		if err != nil || used+reserved+bytes > p.MaxBytesDay {
			return 0, quotaExceeded(err)
		}
	}
	result, err := tx.Exec(`INSERT INTO quota_reservation (user_id, repository_id, assets, bytes) VALUES (?, ?, ?, ?)`, p.UserID, p.RepositoryID, assets, bytes)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// RemainingBytes 返回当前日期窗口内可再预留的字节数；未配置日额度返回 0。
func (r *PublishPolicyRepo) RemainingBytes(p PublishPolicy) (int64, error) {
	if p.MaxBytesDay <= 0 {
		return 0, nil
	}
	day := "strftime('%Y-%m-%dT00:00:00Z', 'now')"
	var used, reserved int64
	err := r.db.QueryRowx(`SELECT
		COALESCE((SELECT used_bytes FROM quota_usage WHERE user_id = ? AND repository_id = ? AND window_kind = 'day' AND window_start = `+day+`),0),
		COALESCE((SELECT SUM(bytes) FROM quota_reservation WHERE user_id = ? AND repository_id = ? AND settled_at IS NULL),0)`,
		p.UserID, p.RepositoryID, p.UserID, p.RepositoryID).Scan(&used, &reserved)
	if err != nil {
		return 0, err
	}
	remaining := p.MaxBytesDay - used - reserved
	if remaining <= 0 {
		return 0, ErrQuotaExceeded
	}
	return remaining, nil
}

// Settle 将预留结算为当前时间窗的已用量；成功为 true，失败则释放预留。
func (r *PublishPolicyRepo) Settle(id int64, success bool) error {
	return r.SettleBytes(id, success, -1)
}

// SettleBytes 结算预留；bytes >= 0 时先把未知长度上传的预留调整为实际大小。
func (r *PublishPolicyRepo) SettleBytes(id int64, success bool, bytes int64) error {
	if !success {
		_, err := r.db.Exec(`DELETE FROM quota_reservation WHERE id = ? AND settled_at IS NULL`, id)
		return err
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var row struct {
		UserID int64 `db:"user_id"`
		RepoID int64 `db:"repository_id"`
		Assets int64 `db:"assets"`
		Bytes  int64 `db:"bytes"`
	}
	if err := tx.Get(&row, `SELECT user_id, repository_id, assets, bytes FROM quota_reservation WHERE id = ? AND settled_at IS NULL`, id); err != nil {
		return err
	}
	if bytes >= 0 {
		row.Bytes = bytes
		if _, err := tx.Exec(`UPDATE quota_reservation SET bytes = ? WHERE id = ? AND settled_at IS NULL`, bytes, id); err != nil {
			return err
		}
	}
	for _, kind := range []string{"hour", "day"} {
		start := "strftime('%Y-%m-%dT%H:00:00Z', 'now')"
		if kind == "day" {
			start = "strftime('%Y-%m-%dT00:00:00Z', 'now')"
		}
		if _, err := tx.Exec(`INSERT INTO quota_usage (user_id, repository_id, window_kind, window_start, used_assets, used_bytes)
			VALUES (?, ?, ?, `+start+`, ?, ?)
			ON CONFLICT(user_id, repository_id, window_kind, window_start) DO UPDATE SET
			used_assets = used_assets + excluded.used_assets, used_bytes = used_bytes + excluded.used_bytes`,
			row.UserID, row.RepoID, kind, row.Assets, row.Bytes); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE quota_reservation SET settled_at = datetime('now') WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseExpired 在进程启动时释放上次进程遗留的全部未结算预留，返回释放数量。
func (r *PublishPolicyRepo) ReleaseExpired() (int64, error) {
	res, err := r.db.Exec(`DELETE FROM quota_reservation WHERE settled_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func quotaExceeded(err error) error {
	if err != nil {
		return err
	}
	return ErrQuotaExceeded
}
