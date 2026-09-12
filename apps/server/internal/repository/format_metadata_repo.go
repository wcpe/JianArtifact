package repository

import (
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// FormatMetadata 是 PyPI/NuGet 生成索引使用的格式元数据。
// sourceMember 只表示本地 group 成员，不保存外部凭据或请求头。
type FormatMetadata struct {
	ID                int64  `db:"id"`
	RepositoryID      int64  `db:"repository_id"`
	Format            string `db:"format"`
	NameNormalized    string `db:"name_normalized"`
	NameDisplay       string `db:"name_display"`
	Version           string `db:"version"`
	VersionNormalized string `db:"version_normalized"`
	Filename          string `db:"filename"`
	AssetPath         string `db:"asset_path"`
	Sha256            string `db:"sha256"`
	Size              int64  `db:"size"`
	RequiresPython    string `db:"requires_python"`
	Yanked            string `db:"yanked"`
	MetadataJSON      string `db:"metadata_json"`
	SourceKind        string `db:"source_kind"`
	SourceMember      string `db:"source_member"`
	CreatedAt         string `db:"created_at"`
	UpdatedAt         string `db:"updated_at"`
}

// FormatMetadataRepo 读写格式专属索引元数据。
type FormatMetadataRepo struct{ db *persistence.DB }

func NewFormatMetadataRepo(db *persistence.DB) *FormatMetadataRepo {
	return &FormatMetadataRepo{db: db}
}

func (r *FormatMetadataRepo) Put(m FormatMetadata) error {
	return putFormatMetadata(r.db, m)
}

// PutAll 将同一索引响应产生的元数据作为一个批次写入，避免部分可见。
func (r *FormatMetadataRepo) PutAll(items []FormatMetadata) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range items {
		if err := putFormatMetadata(tx, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutTx 在调用方提供的事务中写入格式索引，供资产视图与格式元数据同事务提交。
func (r *FormatMetadataRepo) PutTx(tx *sqlx.Tx, m FormatMetadata) error {
	return putFormatMetadata(tx, m)
}

type formatMetadataExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func putFormatMetadata(exec formatMetadataExecutor, m FormatMetadata) error {
	_, err := exec.Exec(`INSERT INTO format_metadata
        (repository_id, format, name_normalized, name_display, version, version_normalized,
         filename, asset_path, sha256, size, requires_python, yanked, metadata_json,
         source_kind, source_member)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(repository_id, format, name_normalized, filename) DO UPDATE SET
         name_display=excluded.name_display, version=excluded.version,
         version_normalized=excluded.version_normalized, asset_path=excluded.asset_path,
         sha256=excluded.sha256, size=excluded.size, requires_python=excluded.requires_python,
         yanked=excluded.yanked, metadata_json=excluded.metadata_json,
         source_kind=excluded.source_kind, source_member=excluded.source_member,
         updated_at=datetime('now')`,
		m.RepositoryID, m.Format, m.NameNormalized, m.NameDisplay, m.Version, m.VersionNormalized,
		m.Filename, m.AssetPath, m.Sha256, m.Size, m.RequiresPython, m.Yanked, m.MetadataJSON,
		m.SourceKind, m.SourceMember)
	return err
}

// PutWithTime 写入跨节点复制的格式元数据，并保留来源的创建与更新时间。
func (r *FormatMetadataRepo) PutWithTime(m FormatMetadata) error {
	return putFormatMetadataWithTime(r.db, m)
}

// PutWithTimeTx 在调用方事务中写入跨节点复制的格式元数据并保留来源时间。
func (r *FormatMetadataRepo) PutWithTimeTx(tx *sqlx.Tx, m FormatMetadata) error {
	return putFormatMetadataWithTime(tx, m)
}

func putFormatMetadataWithTime(exec formatMetadataExecutor, m FormatMetadata) error {
	_, err := exec.Exec(`INSERT INTO format_metadata
        (repository_id, format, name_normalized, name_display, version, version_normalized,
         filename, asset_path, sha256, size, requires_python, yanked, metadata_json,
         source_kind, source_member, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
         COALESCE(NULLIF(?, ''), datetime('now')), COALESCE(NULLIF(?, ''), datetime('now')))
        ON CONFLICT(repository_id, format, name_normalized, filename) DO UPDATE SET
         name_display=excluded.name_display, version=excluded.version,
         version_normalized=excluded.version_normalized, asset_path=excluded.asset_path,
         sha256=excluded.sha256, size=excluded.size, requires_python=excluded.requires_python,
         yanked=excluded.yanked, metadata_json=excluded.metadata_json,
         source_kind=excluded.source_kind, source_member=excluded.source_member,
         created_at=excluded.created_at, updated_at=excluded.updated_at`,
		m.RepositoryID, m.Format, m.NameNormalized, m.NameDisplay, m.Version, m.VersionNormalized,
		m.Filename, m.AssetPath, m.Sha256, m.Size, m.RequiresPython, m.Yanked, m.MetadataJSON,
		m.SourceKind, m.SourceMember, m.CreatedAt, m.UpdatedAt)
	return err
}

// Delete 删除一条格式索引元数据；不存在时保持幂等成功。
func (r *FormatMetadataRepo) Delete(repoID int64, format, name, filename string) error {
	_, err := r.db.Exec(`DELETE FROM format_metadata
        WHERE repository_id=? AND format=? AND name_normalized=? AND filename=?`, repoID, format, name, filename)
	return err
}

func (r *FormatMetadataRepo) Get(repoID int64, format, name, filename string) (*FormatMetadata, error) {
	var m FormatMetadata
	err := r.db.Get(&m, `SELECT id, repository_id, format, name_normalized, name_display,
        version, version_normalized, filename, asset_path, sha256, size, requires_python,
        yanked, metadata_json, source_kind, source_member, created_at, updated_at
        FROM format_metadata WHERE repository_id=? AND format=? AND name_normalized=? AND filename=?`,
		repoID, format, name, filename)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *FormatMetadataRepo) List(repoID int64, format, name string) ([]FormatMetadata, error) {
	var out []FormatMetadata
	err := r.db.Select(&out, `SELECT id, repository_id, format, name_normalized, name_display,
        version, version_normalized, filename, asset_path, sha256, size, requires_python,
        yanked, metadata_json, source_kind, source_member, created_at, updated_at
        FROM format_metadata WHERE repository_id=? AND format=? AND name_normalized=?
        ORDER BY filename`, repoID, format, name)
	return out, err
}

func (r *FormatMetadataRepo) ListProjects(repoID int64, format string) ([]string, error) {
	var out []string
	err := r.db.Select(&out, `SELECT DISTINCT name_normalized FROM format_metadata
        WHERE repository_id=? AND format=? ORDER BY name_normalized`, repoID, format)
	return out, err
}

func (r *FormatMetadataRepo) ListAll(repoID int64, format string) ([]FormatMetadata, error) {
	var out []FormatMetadata
	err := r.db.Select(&out, `SELECT id, repository_id, format, name_normalized, name_display,
        version, version_normalized, filename, asset_path, sha256, size, requires_python,
        yanked, metadata_json, source_kind, source_member, created_at, updated_at
        FROM format_metadata WHERE repository_id=? AND format=? ORDER BY name_normalized, filename`, repoID, format)
	return out, err
}
