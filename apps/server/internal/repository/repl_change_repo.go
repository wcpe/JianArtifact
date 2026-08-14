package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// Change 是 repl_change 表的行模型：一条本地写操作的变更日志（FR-83）。
// seq 本地单调递增；entity_key 为跨节点自然键（不依赖 SQLite 数值 ID）。
type Change struct {
	Seq        int64  `db:"seq"`
	NodeID     string `db:"node_id"`
	Op         string `db:"op"` // put | delete
	EntityType string `db:"entity_type"`
	EntityKey  string `db:"entity_key"`
	Data       string `db:"data"` // 变更后数据 JSON；delete 时为 tombstone
	TS         string `db:"ts"`   // 写入节点本地时钟（RFC3339Nano）
}

// ReplChangeRepo 读写 repl_change 表。
type ReplChangeRepo struct{ db *persistence.DB }

// NewReplChangeRepo 构造 ReplChangeRepo。
func NewReplChangeRepo(db *persistence.DB) *ReplChangeRepo { return &ReplChangeRepo{db: db} }

// Append 追加一条变更日志，返回新 seq。
func (r *ReplChangeRepo) Append(nodeID, op, entityType, entityKey, data, ts string) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO repl_change (node_id, op, entity_type, entity_key, data, ts)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		nodeID, op, entityType, entityKey, data, ts,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
