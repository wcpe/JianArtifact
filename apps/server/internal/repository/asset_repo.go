package repository

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AssetRepo 读写 asset 表。
type AssetRepo struct{ db *persistence.DB }

// NewAssetRepo 构造 AssetRepo。
func NewAssetRepo(db *persistence.DB) *AssetRepo { return &AssetRepo{db: db} }

// Upsert 覆盖写入仓库内某路径的资产：路径已存在则更新 blob/大小/类型与 updated_at，
// 否则插入新行。以 (repository_id, path) 唯一约束实现 last-writer-wins。
func (r *AssetRepo) Upsert(repoID int64, path, blobHash string, size int64, contentType string) error {
	_, err := r.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size, content_type)
			VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, path) DO UPDATE SET
			blob_hash    = excluded.blob_hash,
			size         = excluded.size,
			content_type = excluded.content_type,
			updated_at   = datetime('now')`,
		repoID, path, blobHash, size, contentType,
	)
	return err
}

// GetByPath 按仓库与路径取资产；不存在返回 ErrNotFound。
func (r *AssetRepo) GetByPath(repoID int64, path string) (*Asset, error) {
	var a Asset
	err := r.db.Get(&a,
		`SELECT id, repository_id, path, blob_hash, size, content_type, created_at, updated_at
			FROM asset WHERE repository_id = ? AND path = ?`,
		repoID, path,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteByPath 删除仓库内某路径的资产（blob 内容不即时清理）；不存在返回 ErrNotFound。
func (r *AssetRepo) DeleteByPath(repoID int64, path string) error {
	res, err := r.db.Exec(`DELETE FROM asset WHERE repository_id = ? AND path = ?`, repoID, path)
	return affected(res, err)
}

// ListByRepo 返回仓库内资产（按 path 升序分页）；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) ListByRepo(repoID int64, prefix string, limit, offset int) ([]Asset, error) {
	var assets []Asset
	err := r.db.Select(&assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, created_at, updated_at
			FROM asset WHERE repository_id = ? AND path LIKE ? ESCAPE '\' ORDER BY path LIMIT ? OFFSET ?`,
		repoID, likePrefix(prefix), limit, offset,
	)
	return assets, err
}

// CountByRepo 返回仓库内资产总数；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) CountByRepo(repoID int64, prefix string) (int, error) {
	var n int
	err := r.db.Get(&n,
		`SELECT COUNT(*) FROM asset WHERE repository_id = ? AND path LIKE ? ESCAPE '\'`,
		repoID, likePrefix(prefix),
	)
	return n, err
}

// likePrefix 把路径前缀转为 LIKE 模式：转义 %/_/\ 后追加通配 %（配合 ESCAPE '\'），
// 空前缀匹配全部。
func likePrefix(prefix string) string {
	if prefix == "" {
		return "%"
	}
	var b strings.Builder
	for _, ch := range prefix {
		switch ch {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	b.WriteByte('%')
	return b.String()
}
