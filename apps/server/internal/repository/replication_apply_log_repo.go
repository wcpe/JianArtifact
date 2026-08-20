package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ReplicationApplyLog 是 replication_apply_log 表的行模型。
// 它记录本节点接收远端变更的应用结果，不参与 repl_change 回声复制。
type ReplicationApplyLog struct {
	SourceNode   string `db:"source_node" json:"sourceNode"`
	SourceSeq    int64  `db:"source_seq" json:"sourceSeq"`
	PeerURL      string `db:"peer_url" json:"peerUrl"`
	EntityType   string `db:"entity_type" json:"entityType"`
	EntityKey    string `db:"entity_key" json:"entityKey"`
	Op           string `db:"op" json:"op"`
	Result       string `db:"result" json:"result"`
	Detail       string `db:"detail" json:"detail"`
	LastError    string `db:"last_error" json:"lastError"`
	LastErrorAt  string `db:"last_error_at" json:"lastErrorAt"`
	FirstSeenAt  string `db:"first_seen_at" json:"firstSeenAt"`
	LastSeenAt   string `db:"last_seen_at" json:"lastSeenAt"`
	AttemptCount int    `db:"attempt_count" json:"attemptCount"`
}

// ReplicationApplyLogFilter 是复制接收审计查询筛选。
type ReplicationApplyLogFilter struct {
	SourceNode string
	SourceSeq  *int64
	PeerURL    string
	EntityType string
	EntityKey  string
	Op         string
	Result     string
	Limit      int
	Offset     int
}

// ReplicationApplyLogRepo 读写复制接收审计表。
type ReplicationApplyLogRepo struct{ db *persistence.DB }

// NewReplicationApplyLogRepo 构造复制接收审计仓储。
func NewReplicationApplyLogRepo(db *persistence.DB) *ReplicationApplyLogRepo {
	return &ReplicationApplyLogRepo{db: db}
}

// Observe 记录一次变更尝试；同一 source_node/source_seq 幂等合并并递增尝试次数。
// 非空错误会覆盖 last_error，空错误不会清除既有错误证据。
func (r *ReplicationApplyLogRepo) Observe(e ReplicationApplyLog) error {
	_, err := r.db.Exec(`
		INSERT INTO replication_apply_log
			(source_node, source_seq, peer_url, entity_type, entity_key, op, result, detail,
			 last_error, last_error_at, first_seen_at, last_seen_at, attempt_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(source_node, source_seq) DO UPDATE SET
			peer_url = excluded.peer_url,
			entity_type = excluded.entity_type,
			entity_key = excluded.entity_key,
			op = excluded.op,
			result = excluded.result,
			detail = excluded.detail,
			last_seen_at = excluded.last_seen_at,
			attempt_count = replication_apply_log.attempt_count + 1,
			last_error = CASE WHEN excluded.last_error <> '' THEN excluded.last_error ELSE replication_apply_log.last_error END,
			last_error_at = CASE WHEN excluded.last_error_at <> '' THEN excluded.last_error_at ELSE replication_apply_log.last_error_at END`,
		e.SourceNode, e.SourceSeq, e.PeerURL, e.EntityType, e.EntityKey, e.Op, e.Result, e.Detail,
		e.LastError, e.LastErrorAt, e.FirstSeenAt, e.LastSeenAt,
	)
	return err
}

// UpdateResult 更新已有变更的最终结果；不改变尝试次数，并保留既有错误证据。
func (r *ReplicationApplyLogRepo) UpdateResult(sourceNode string, sourceSeq int64, result, detail, lastError, lastErrorAt, seenAt string) error {
	var query string
	var args []any
	if lastError != "" || lastErrorAt != "" {
		query = `UPDATE replication_apply_log
			SET result = ?, detail = ?, last_seen_at = ?, last_error = CASE WHEN ? <> '' THEN ? ELSE last_error END,
				last_error_at = CASE WHEN ? <> '' THEN ? ELSE last_error_at END
			WHERE source_node = ? AND source_seq = ?`
		args = []any{result, detail, seenAt, lastError, lastError, lastErrorAt, lastErrorAt, sourceNode, sourceSeq}
	} else {
		query = `UPDATE replication_apply_log SET result = ?, detail = ?, last_seen_at = ?
			WHERE source_node = ? AND source_seq = ?`
		args = []any{result, detail, seenAt, sourceNode, sourceSeq}
	}
	res, err := r.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get 返回指定来源变更的审计记录。
func (r *ReplicationApplyLogRepo) Get(sourceNode string, sourceSeq int64) (*ReplicationApplyLog, error) {
	var e ReplicationApplyLog
	err := r.db.Get(&e, `SELECT source_node, source_seq, peer_url, entity_type, entity_key, op, result, detail,
		last_error, last_error_at, first_seen_at, last_seen_at, attempt_count
		FROM replication_apply_log WHERE source_node = ? AND source_seq = ?`, sourceNode, sourceSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// List 按最近看到时间倒序分页查询接收审计记录。
func (r *ReplicationApplyLogRepo) List(f ReplicationApplyLogFilter) ([]ReplicationApplyLog, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	where, args := applyLogWhere(f)
	args = append(args, limit, offset)
	var entries []ReplicationApplyLog
	if err := r.db.Select(&entries, `SELECT source_node, source_seq, peer_url, entity_type, entity_key, op, result, detail,
		last_error, last_error_at, first_seen_at, last_seen_at, attempt_count
		FROM replication_apply_log WHERE 1=1`+where+` ORDER BY last_seen_at DESC, source_node DESC, source_seq DESC LIMIT ? OFFSET ?`, args...); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []ReplicationApplyLog{}
	}
	return entries, nil
}

// Count 返回筛选条件下的记录总数。
func (r *ReplicationApplyLogRepo) Count(f ReplicationApplyLogFilter) (int, error) {
	where, args := applyLogWhere(f)
	var n int
	if err := r.db.Get(&n, `SELECT COUNT(*) FROM replication_apply_log WHERE 1=1`+where, args...); err != nil {
		return 0, err
	}
	return n, nil
}

func applyLogWhere(f ReplicationApplyLogFilter) (string, []any) {
	where := ""
	args := []any{}
	if f.SourceNode != "" {
		where += " AND source_node = ?"
		args = append(args, f.SourceNode)
	}
	if f.SourceSeq != nil {
		where += " AND source_seq = ?"
		args = append(args, *f.SourceSeq)
	}
	if f.PeerURL != "" {
		where += " AND peer_url = ?"
		args = append(args, f.PeerURL)
	}
	if f.EntityType != "" {
		where += " AND entity_type = ?"
		args = append(args, f.EntityType)
	}
	if f.EntityKey != "" {
		where += " AND entity_key = ?"
		args = append(args, f.EntityKey)
	}
	if f.Op != "" {
		where += " AND op = ?"
		args = append(args, f.Op)
	}
	if f.Result != "" {
		where += " AND result = ?"
		args = append(args, f.Result)
	}
	return where, args
}
