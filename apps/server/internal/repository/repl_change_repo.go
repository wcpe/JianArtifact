package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// Change 是 repl_change 表的行模型：一条本地写操作的变更日志（FR-83）。
// seq 本地单调递增；entity_key 为跨节点自然键（不依赖 SQLite 数值 ID）。
// JSON tag 供复制协议传输（FR-84 pull 端点）。
type Change struct {
	Seq        int64  `db:"seq" json:"seq"`
	NodeID     string `db:"node_id" json:"nodeId"`
	Op         string `db:"op" json:"op"` // put | delete
	EntityType string `db:"entity_type" json:"entityType"`
	EntityKey  string `db:"entity_key" json:"entityKey"`
	Data       string `db:"data" json:"data"` // 变更后数据 JSON；delete 时为 tombstone
	TS         string `db:"ts" json:"ts"`     // 写入节点本地时钟（RFC3339Nano）
}

// ReplChangeRepo 读写 repl_change 表。
type ReplChangeRepo struct{ db *persistence.DB }

// EntityVersion 是实体已接受的最新 LWW 版本。
type EntityVersion struct {
	NodeID string `db:"node_id"`
	TS     string `db:"ts"`
}

const upsertEntityVersionSQL = `INSERT INTO repl_entity_version (entity_type, entity_key, node_id, ts)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(entity_type, entity_key) DO UPDATE SET node_id=excluded.node_id, ts=excluded.ts
	WHERE excluded.ts > repl_entity_version.ts
	   OR (excluded.ts = repl_entity_version.ts AND excluded.node_id > repl_entity_version.node_id)`

// NewReplChangeRepo 构造 ReplChangeRepo。
func NewReplChangeRepo(db *persistence.DB) *ReplChangeRepo { return &ReplChangeRepo{db: db} }

// Append 追加一条变更日志，并在同一事务更新实体版本，返回新 seq。
func (r *ReplChangeRepo) Append(nodeID, op, entityType, entityKey, data, ts string) (int64, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`INSERT INTO repl_change (node_id, op, entity_type, entity_key, data, ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		nodeID, op, entityType, entityKey, data, ts,
	)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(upsertEntityVersionSQL, entityType, entityKey, nodeID, ts); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// LatestSeq 返回本地最新变更序号；空表返回 0（供复制协议 since 水位）。
func (r *ReplChangeRepo) LatestSeq() (int64, error) {
	var seq int64
	err := r.db.Get(&seq, `SELECT COALESCE(MAX(seq), 0) FROM repl_change`)
	return seq, err
}

// LastChangeOf 返回指定实体最近一条变更（按 ts,node_id 降序）；无记录返回 ErrNotFound。
// 用于 last-writer-wins 冲突裁决：本地已对该实体有过更晚写入则跳过对端变更。
func (r *ReplChangeRepo) LastChangeOf(entityType, entityKey string) (*Change, error) {
	var c Change
	err := r.db.Get(&c,
		`SELECT seq, node_id, op, entity_type, entity_key, data, ts FROM repl_change
		 WHERE entity_type = ? AND entity_key = ?
		 ORDER BY ts DESC, node_id DESC, seq DESC LIMIT 1`,
		entityType, entityKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// LastVersionOf 返回本地日志或已应用远端状态中的最新实体版本。
func (r *ReplChangeRepo) LastVersionOf(entityType, entityKey string) (*EntityVersion, error) {
	var v EntityVersion
	err := r.db.Get(&v, `SELECT node_id, ts FROM (
		SELECT node_id, ts FROM repl_entity_version WHERE entity_type = ? AND entity_key = ?
		UNION ALL
		SELECT node_id, ts FROM repl_change WHERE entity_type = ? AND entity_key = ?
	) ORDER BY ts DESC, node_id DESC LIMIT 1`, entityType, entityKey, entityType, entityKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// UpsertVersion 仅在传入版本更新时推进实体的 LWW 状态。
func (r *ReplChangeRepo) UpsertVersion(entityType, entityKey, nodeID, ts string) error {
	_, err := r.db.Exec(upsertEntityVersionSQL, entityType, entityKey, nodeID, ts)
	return err
}

// ListSince 返回 seq 大于 since 的变更（按 seq 升序）；limit ≤ 0 表示不限制。
// 供复制协议按 watermark 增量拉取（FR-84）。
func (r *ReplChangeRepo) ListSince(since int64, limit int) ([]Change, error) {
	var changes []Change
	var err error
	if limit > 0 {
		err = r.db.Select(&changes,
			`SELECT seq, node_id, op, entity_type, entity_key, data, ts FROM repl_change
			 WHERE seq > ? ORDER BY seq LIMIT ?`,
			since, limit)
	} else {
		err = r.db.Select(&changes,
			`SELECT seq, node_id, op, entity_type, entity_key, data, ts FROM repl_change
			 WHERE seq > ? ORDER BY seq`,
			since)
	}
	return changes, err
}

// ListRange 分页返回 seq 区间 (from, to] 的变更（FR-98 同步历史详情：从 repl_change 反推某次同步拉取的具体变更）。
// entityType 非空时按实体类型过滤（分类展示）。
func (r *ReplChangeRepo) ListRange(from, to int64, limit, offset int, entityType string) ([]Change, error) {
	var changes []Change
	if entityType != "" {
		err := r.db.Select(&changes,
			`SELECT seq, node_id, op, entity_type, entity_key, data, ts FROM repl_change
			 WHERE seq > ? AND seq <= ? AND entity_type = ? ORDER BY seq LIMIT ? OFFSET ?`,
			from, to, entityType, limit, offset)
		return changes, err
	}
	err := r.db.Select(&changes,
		`SELECT seq, node_id, op, entity_type, entity_key, data, ts FROM repl_change
		 WHERE seq > ? AND seq <= ? ORDER BY seq LIMIT ? OFFSET ?`,
		from, to, limit, offset)
	return changes, err
}

// CountRange 返回 seq 区间 (from, to] 的变更总数（FR-98 分页用）；entityType 非空时按实体类型过滤。
func (r *ReplChangeRepo) CountRange(from, to int64, entityType string) (int, error) {
	var n int
	if entityType != "" {
		err := r.db.Get(&n,
			`SELECT COUNT(*) FROM repl_change WHERE seq > ? AND seq <= ? AND entity_type = ?`,
			from, to, entityType)
		return n, err
	}
	err := r.db.Get(&n,
		`SELECT COUNT(*) FROM repl_change WHERE seq > ? AND seq <= ?`,
		from, to)
	return n, err
}
