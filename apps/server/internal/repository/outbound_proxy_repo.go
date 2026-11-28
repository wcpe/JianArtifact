package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// OutboundProxy 是 outbound_proxy 表的行模型：全局命名出站代理节点。
// ConfigCiphertext 保存 AES-256-GCM 密封后的代理 URL 或 Mihomo 节点配置，绝不明文落库或外泄。
type OutboundProxy struct {
	ID               int64  `db:"id"`
	Name             string `db:"name"`
	Kind             string `db:"kind"`
	Description      string `db:"description"`
	EndpointPreview  string `db:"endpoint_preview"`
	ConfigCiphertext []byte `db:"config_ciphertext"`
	Enabled          bool   `db:"enabled"`
	CreatedAt        string `db:"created_at"`
	UpdatedAt        string `db:"updated_at"`
}

// OutboundProxyKind 取值与 outbound_proxy.kind CHECK 约束一一对应。
const (
	OutboundProxyKindHTTP   = "http"
	OutboundProxyKindSOCKS5 = "socks5"
	OutboundProxyKindMihomo = "mihomo"
)

// OutboundProxyRepo 负责 outbound_proxy 表的数据读写。
type OutboundProxyRepo struct {
	db *persistence.DB
}

// NewOutboundProxyRepo 构造 OutboundProxyRepo。
func NewOutboundProxyRepo(db *persistence.DB) *OutboundProxyRepo {
	return &OutboundProxyRepo{db: db}
}

// Create 插入一条出站代理记录并返回自增 ID。
func (r *OutboundProxyRepo) Create(p *OutboundProxy) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO outbound_proxy (name, kind, description, endpoint_preview, config_ciphertext, enabled)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.Name, p.Kind, p.Description, p.EndpointPreview, p.ConfigCiphertext, p.Enabled,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetByID 按 ID 查询出站代理；不存在返回 ErrNotFound。
func (r *OutboundProxyRepo) GetByID(id int64) (*OutboundProxy, error) {
	var p OutboundProxy
	err := r.db.Get(&p,
		`SELECT id, name, kind, description, endpoint_preview, config_ciphertext, enabled, created_at, updated_at
		 FROM outbound_proxy WHERE id = ?`, id,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetByName 按名称查询出站代理；不存在返回 ErrNotFound。
func (r *OutboundProxyRepo) GetByName(name string) (*OutboundProxy, error) {
	var p OutboundProxy
	err := r.db.Get(&p,
		`SELECT id, name, kind, description, endpoint_preview, config_ciphertext, enabled, created_at, updated_at
		 FROM outbound_proxy WHERE name = ?`, name,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// List 返回全部出站代理（按名称升序）。
func (r *OutboundProxyRepo) List() ([]OutboundProxy, error) {
	var items []OutboundProxy
	err := r.db.Select(&items,
		`SELECT id, name, kind, description, endpoint_preview, config_ciphertext, enabled, created_at, updated_at
		 FROM outbound_proxy ORDER BY name ASC, id ASC`,
	)
	if err != nil {
		return nil, err
	}
	return items, nil
}

// Update 更新出站代理全部可变字段并刷新 updated_at；未命中返回 ErrNotFound。
func (r *OutboundProxyRepo) Update(p *OutboundProxy) error {
	res, err := r.db.Exec(
		`UPDATE outbound_proxy
		 SET name = ?, kind = ?, description = ?, endpoint_preview = ?, config_ciphertext = ?, enabled = ?, updated_at = datetime('now')
		 WHERE id = ?`,
		p.Name, p.Kind, p.Description, p.EndpointPreview, p.ConfigCiphertext, p.Enabled, p.ID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 按 ID 删除出站代理；未命中返回 ErrNotFound。
func (r *OutboundProxyRepo) Delete(id int64) error {
	res, err := r.db.Exec(`DELETE FROM outbound_proxy WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
