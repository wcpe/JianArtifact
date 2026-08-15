package repository

import (
	"encoding/json"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// SyncLogEntry 是 repl_sync_log 表的行模型：一次同步的记录（含进行中/成功/失败与统计）。
// Success 为 nil 表示进行中（started 后、finished 前）；FinishedAt 同理为 nil。
// EntityCounts 为变更实体构成 JSON 文本（如 {"asset":5,"repository":2,"user":1}）。
type SyncLogEntry struct {
	ID           int64   `db:"id" json:"id"`
	PeerURL      string  `db:"peer_url" json:"peerUrl"`
	StartedAt    string  `db:"started_at" json:"startedAt"`
	FinishedAt   *string `db:"finished_at" json:"finishedAt,omitempty"`
	Success      *bool   `db:"success" json:"success"` // nil=进行中 / true=成功 / false=失败
	FromSeq      int64   `db:"from_seq" json:"fromSeq"`
	ToSeq        int64   `db:"to_seq" json:"toSeq"`
	Changes      int     `db:"changes" json:"changes"`
	Applied      int     `db:"applied" json:"applied"`
	Failed       int     `db:"failed" json:"failed"`
	Blobs        int     `db:"blobs" json:"blobs"`
	EntityCounts string  `db:"entity_counts" json:"entityCounts"`
	ErrorText    string  `db:"error_text" json:"errorText,omitempty"`
}

// SyncLogRepo 读写 repl_sync_log 表（复制同步历史，FR-88 可视化）。
type SyncLogRepo struct{ db *persistence.DB }

// NewSyncLogRepo 构造 SyncLogRepo。
func NewSyncLogRepo(db *persistence.DB) *SyncLogRepo { return &SyncLogRepo{db: db} }

// Start 记录一轮同步的开始（进行中），返回日志 ID。
func (r *SyncLogRepo) Start(peerURL string, fromSeq int64) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO repl_sync_log (peer_url, started_at, from_seq, to_seq, changes, applied, failed, blobs, entity_counts, error_text)
		 VALUES (?, ?, ?, ?, 0, 0, 0, 0, '{}', '')`,
		peerURL, time.Now().UTC().Format(time.RFC3339Nano), fromSeq, fromSeq,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Finish 结束一轮同步：更新完成状态与统计（entityCounts 序列化为 JSON 文本）。
func (r *SyncLogRepo) Finish(id int64, success bool, toSeq int64, changes, applied, failed, blobs int, entityCounts map[string]int, errText string) error {
	raw, err := json.Marshal(entityCounts)
	if err != nil {
		return err
	}
	s := 0
	if success {
		s = 1
	}
	_, err = r.db.Exec(
		`UPDATE repl_sync_log SET success=?, finished_at=?, to_seq=?, changes=?, applied=?, failed=?, blobs=?, entity_counts=?, error_text=?
		 WHERE id = ?`,
		s, time.Now().UTC().Format(time.RFC3339Nano), toSeq, changes, applied, failed, blobs, string(raw), errText, id,
	)
	return err
}

// List 按开始时间倒序分页返回同步历史。
func (r *SyncLogRepo) List(limit, offset int) ([]SyncLogEntry, error) {
	var entries []SyncLogEntry
	err := r.db.Select(&entries,
		`SELECT id, peer_url, started_at, finished_at, success, from_seq, to_seq, changes, applied, failed, blobs, entity_counts, error_text
		 FROM repl_sync_log ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	return entries, err
}

// Delete 删除一条同步记录（用于空同步不留痕：无变更且成功时移除进行中记录）。
func (r *SyncLogRepo) Delete(id int64) error {
	_, err := r.db.Exec(`DELETE FROM repl_sync_log WHERE id = ?`, id)
	return err
}

// Count 返回同步历史总条数。
func (r *SyncLogRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM repl_sync_log`)
	return n, err
}
