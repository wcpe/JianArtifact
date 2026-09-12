package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AssetMutation 是资产变更的持久化 intent 状态。
type AssetMutation struct {
	ID                      string `db:"id"`
	Status                  string `db:"status"`
	Origin                  string `db:"origin"`
	ReceiptStreamGeneration string `db:"receipt_stream_generation"`
	ReceiptSourceNode       string `db:"receipt_source_node"`
	ReceiptSourceSeq        int64  `db:"receipt_source_seq"`
	ReceiptManifestSHA256   string `db:"receipt_manifest_sha256"`
	ReceiptItemCount        int    `db:"receipt_item_count"`
	CreatedAt               string `db:"created_at"`
	UpdatedAt               string `db:"updated_at"`
	Error                   string `db:"error"`
}

// AssetMutationItem 保存单个资产的前镜像与目标镜像。
type AssetMutationItem struct {
	OperationID  string
	Ordinal      int
	RepositoryID int64
	Path         string
	Before       *Asset
	After        *Asset
}

// MutationCompletionHook 在资产视图已写入、intent 完成前运行，供格式元数据等同库状态
// 与资产变更保持同一短事务。
type MutationCompletionHook func(tx *sqlx.Tx) error

// BlobQuarantine 是隔离区记录，路径仅用于恢复，不对外提供下载。
type BlobQuarantine struct {
	OperationID    string `db:"operation_id"`
	BlobHash       string `db:"blob_hash"`
	QuarantinePath string `db:"quarantine_path"`
	Status         string `db:"status"`
	Error          string `db:"error"`
}

// AssetMutationRepo 持久化资产操作 intent、前镜像与隔离区状态。
type AssetMutationRepo struct{ db *persistence.DB }

// NewAssetMutationRepo 构造资产操作仓储。
func NewAssetMutationRepo(db *persistence.DB) *AssetMutationRepo { return &AssetMutationRepo{db: db} }

// DB 返回底层数据库，供资产完成事务接入复制 outbox。
func (r *AssetMutationRepo) DB() *persistence.DB { return r.db }

// Begin 写入 prepared intent 与全部前镜像；不执行文件系统操作。
func (r *AssetMutationRepo) Begin(id string, items []AssetMutationItem) error {
	return r.begin(id, items, nil)
}

// BeginReceived 在 prepared intent 首次落盘时固化远端 operation 身份，供 standby
// 崩溃恢复后只处理 received intent，并在重试时拒绝同 ID 的不同载荷。
func (r *AssetMutationRepo) BeginReceived(id string, items []AssetMutationItem, receipt OperationEnvelope) error {
	return r.begin(id, items, &receipt)
}

func (r *AssetMutationRepo) begin(id string, items []AssetMutationItem, receipt *OperationEnvelope) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	origin, generation, sourceNode, manifest := "local", "", "", ""
	var sourceSeq int64
	var itemCount int
	if receipt != nil {
		origin, generation, sourceNode, sourceSeq, manifest, itemCount = "received", receipt.StreamGeneration, receipt.SourceNode, receipt.Seq, receipt.ManifestSHA256, receipt.ItemCount
	}
	if _, err := tx.Exec(`INSERT INTO asset_mutation
		(id, status, origin, receipt_stream_generation, receipt_source_node, receipt_source_seq, receipt_manifest_sha256, receipt_item_count)
		VALUES (?, 'prepared', ?, ?, ?, ?, ?, ?)`, id, origin, generation, sourceNode, sourceSeq, manifest, itemCount); err != nil {
		return err
	}
	for _, item := range items {
		if err := insertMutationItem(tx, id, item); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertMutationItem(tx *sqlx.Tx, id string, item AssetMutationItem) error {
	before := item.Before
	after := item.After
	args := []any{id, item.Ordinal, item.RepositoryID, item.Path}
	if before == nil {
		args = append(args, false, "", 0, "", "", "", "", "")
	} else {
		args = append(args, true, before.BlobHash, before.Size, before.ContentType, before.Sha1, before.Md5, before.CreatedAt, before.UpdatedAt)
	}
	if after == nil {
		args = append(args, false, "", 0, "", "", "")
	} else {
		args = append(args, true, after.BlobHash, after.Size, after.ContentType, after.Sha1, after.Md5)
	}
	_, err := tx.Exec(`INSERT INTO asset_mutation_item (
		operation_id, ordinal, repository_id, path,
		before_exists, before_blob_hash, before_size, before_type, before_sha1, before_md5, before_created_at, before_updated_at,
		after_exists, after_blob_hash, after_size, after_type, after_sha1, after_md5)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
	return err
}

// MarkStaged 在所有文件移动成功后记录隔离路径并推进 staged。
func (r *AssetMutationRepo) MarkStaged(id string, entries []BlobQuarantine) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, entry := range entries {
		if _, err := tx.Exec(`INSERT INTO blob_quarantine (operation_id, blob_hash, quarantine_path, status)
			VALUES (?, ?, ?, 'staged')
			ON CONFLICT(operation_id, blob_hash) DO UPDATE SET quarantine_path=excluded.quarantine_path, status='staged', error=''`,
			entry.OperationID, entry.BlobHash, entry.QuarantinePath); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE asset_mutation SET status='staged', updated_at=datetime('now') WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkRollingBack 将失败 intent 标为恢复中，便于启动恢复继续处理。
func (r *AssetMutationRepo) MarkRollingBack(id, reason string) error {
	_, err := r.db.Exec(`UPDATE asset_mutation SET status='rolling_back', error=?, updated_at=datetime('now') WHERE id=?`, reason, id)
	return err
}

// MarkRolledBack 记录恢复完成。
func (r *AssetMutationRepo) MarkRolledBack(id, reason string) error {
	_, err := r.db.Exec(`UPDATE asset_mutation SET status='rolled_back', error=?, updated_at=datetime('now') WHERE id=?`, reason, id)
	return err
}

// DeleteRolledBack 移除已经完整恢复的失败 intent，允许同一远端 operationId 幂等重试。
// 仅 rolled_back 可删除，级联的 item/quarantine 记录一并清理。
func (r *AssetMutationRepo) DeleteRolledBack(id string) error {
	result, err := r.db.Exec(`DELETE FROM asset_mutation WHERE id=? AND status='rolled_back'`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("远端 operation 尚未完成回滚，不能重试")
	}
	return nil
}

// Complete 在同一个短事务中应用资产视图并发布 completed 状态。
func (r *AssetMutationRepo) Complete(id string, items []AssetMutationItem) error {
	return r.complete(id, items, nil, nil)
}

// CompleteWithHook 在同一事务中应用资产视图并执行附加状态变更。
func (r *AssetMutationRepo) CompleteWithHook(id string, items []AssetMutationItem, hook MutationCompletionHook) error {
	return r.complete(id, items, nil, nil, hook)
}

// CompleteWithOutbox 在同一个短事务中应用资产视图、完成 intent 并写入 operation outbox。
func (r *AssetMutationRepo) CompleteWithOutbox(id string, items []AssetMutationItem, outbox *ReplicationOperationRepo, envelope OperationEnvelope) error {
	return r.complete(id, items, outbox, &envelope)
}

// CompleteWithOutboxAndHook 将资产、outbox 与源端审计作为同一事务提交。
func (r *AssetMutationRepo) CompleteWithOutboxAndHook(id string, items []AssetMutationItem, outbox *ReplicationOperationRepo, envelope OperationEnvelope, hook MutationCompletionHook) error {
	return r.complete(id, items, outbox, &envelope, hook)
}

// RollbackCompleted 恢复已提交但物理回收未完成的资产视图，并将 intent 标为 rolled_back。
// 仅协调器在持有节点写门时调用，避免外部读路径观察回滚中间态。
func (r *AssetMutationRepo) RollbackCompleted(id string, items []AssetMutationItem, reason string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := restoreRepositoryDeleteJournal(tx, id); err != nil {
		return err
	}
	for i := len(items) - 1; i >= 0; i-- {
		if err := restoreMutationItem(tx, items[i]); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE asset_mutation SET status='rolled_back', error=?, updated_at=datetime('now') WHERE id=? AND status='completed'`, reason, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("资产操作未处于可回滚完成状态：%s", id)
	}
	return tx.Commit()
}

func (r *AssetMutationRepo) complete(id string, items []AssetMutationItem, outbox *ReplicationOperationRepo, envelope *OperationEnvelope, hooks ...MutationCompletionHook) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE asset_mutation SET status='committing', updated_at=datetime('now') WHERE id=? AND status='staged'`, id); err != nil {
		return err
	}
	for _, item := range items {
		if err := applyMutationItem(tx, item); err != nil {
			return err
		}
	}
	if outbox != nil && envelope != nil {
		if _, err := outbox.Append(tx, *envelope); err != nil {
			return err
		}
	}
	for _, hook := range hooks {
		if hook != nil {
			if err := hook(tx); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE asset_mutation SET status='completed', updated_at=datetime('now'), error='' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func applyMutationItem(tx *sqlx.Tx, item AssetMutationItem) error {
	var current Asset
	err := tx.Get(&current, `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at FROM asset WHERE repository_id=? AND path=?`, item.RepositoryID, item.Path)
	if errors.Is(err, sql.ErrNoRows) {
		if item.Before != nil {
			return fmt.Errorf("资产前镜像不匹配：%d/%s", item.RepositoryID, item.Path)
		}
	} else if err != nil {
		return err
	} else if item.Before == nil || current.BlobHash != item.Before.BlobHash {
		return fmt.Errorf("资产已被其他操作修改：%d/%s", item.RepositoryID, item.Path)
	}
	if item.After == nil {
		_, err = tx.Exec(`DELETE FROM asset WHERE repository_id=? AND path=?`, item.RepositoryID, item.Path)
		return err
	}
	if item.After.Path != item.Path {
		var target Asset
		err = tx.Get(&target, `SELECT id FROM asset WHERE repository_id=? AND path=?`, item.After.RepositoryID, item.After.Path)
		if err == nil {
			return fmt.Errorf("资产目标路径已存在：%d/%s", item.After.RepositoryID, item.After.Path)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), datetime('now')), COALESCE(NULLIF(?,''), datetime('now')))`,
			item.After.RepositoryID, item.After.Path, item.After.BlobHash, item.After.Size, item.After.ContentType,
			item.After.Sha1, item.After.Md5, item.After.CreatedAt, item.After.UpdatedAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM asset WHERE repository_id=? AND path=?`, item.RepositoryID, item.Path)
		return err
	}
	_, err = tx.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), datetime('now')), COALESCE(NULLIF(?,''), datetime('now')))
		ON CONFLICT(repository_id, path) DO UPDATE SET blob_hash=excluded.blob_hash, size=excluded.size,
		content_type=excluded.content_type, sha1=excluded.sha1, md5=excluded.md5, updated_at=excluded.updated_at`,
		item.After.RepositoryID, item.After.Path, item.After.BlobHash, item.After.Size, item.After.ContentType,
		item.After.Sha1, item.After.Md5, item.After.CreatedAt, item.After.UpdatedAt)
	return err
}

func restoreMutationItem(tx *sqlx.Tx, item AssetMutationItem) error {
	if item.After != nil {
		var current Asset
		err := tx.Get(&current, `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at FROM asset WHERE repository_id=? AND path=?`, item.After.RepositoryID, item.After.Path)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && current.BlobHash != item.After.BlobHash {
			return fmt.Errorf("资产回滚前镜像不匹配：%d/%s", item.After.RepositoryID, item.After.Path)
		}
		if err == nil {
			if _, err := tx.Exec(`DELETE FROM asset WHERE repository_id=? AND path=?`, item.After.RepositoryID, item.After.Path); err != nil {
				return err
			}
		}
	}
	if item.Before == nil {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), datetime('now')), COALESCE(NULLIF(?,''), datetime('now')))
		ON CONFLICT(repository_id, path) DO UPDATE SET blob_hash=excluded.blob_hash, size=excluded.size,
		content_type=excluded.content_type, sha1=excluded.sha1, md5=excluded.md5,
		created_at=excluded.created_at, updated_at=excluded.updated_at`,
		item.Before.RepositoryID, item.Before.Path, item.Before.BlobHash, item.Before.Size, item.Before.ContentType,
		item.Before.Sha1, item.Before.Md5, item.Before.CreatedAt, item.Before.UpdatedAt)
	return err
}

// GetIncomplete 返回尚未完成的 intent，供进程启动恢复。
func (r *AssetMutationRepo) GetIncomplete() ([]AssetMutation, error) {
	var items []AssetMutation
	err := r.db.Select(&items, `SELECT id, status, origin, receipt_stream_generation, receipt_source_node, receipt_source_seq,
		receipt_manifest_sha256, receipt_item_count, created_at, updated_at, error
		FROM asset_mutation WHERE status NOT IN ('completed','rolled_back') ORDER BY created_at, id`)
	return items, err
}

// GetIncompleteReceived 只返回复制接收产生的未完成 intent。
func (r *AssetMutationRepo) GetIncompleteReceived() ([]AssetMutation, error) {
	var items []AssetMutation
	err := r.db.Select(&items, `SELECT id, status, origin, receipt_stream_generation, receipt_source_node, receipt_source_seq,
		receipt_manifest_sha256, receipt_item_count, created_at, updated_at, error
		FROM asset_mutation WHERE origin='received' AND status NOT IN ('completed','rolled_back') ORDER BY created_at, id`)
	return items, err
}

// Get 按 operationId 读取 intent。
func (r *AssetMutationRepo) Get(id string) (*AssetMutation, error) {
	var item AssetMutation
	err := r.db.Get(&item, `SELECT id, status, origin, receipt_stream_generation, receipt_source_node, receipt_source_seq,
		receipt_manifest_sha256, receipt_item_count, created_at, updated_at, error FROM asset_mutation WHERE id=?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListItems 返回指定操作的前镜像清单。
func (r *AssetMutationRepo) ListItems(id string) ([]AssetMutationItem, error) {
	var rows []mutationItemRow
	if err := r.db.Select(&rows, `SELECT operation_id, ordinal, repository_id, path,
		before_exists, before_blob_hash, before_size, before_type, before_sha1, before_md5, before_created_at, before_updated_at,
		after_exists, after_blob_hash, after_size, after_type, after_sha1, after_md5
		FROM asset_mutation_item WHERE operation_id=? ORDER BY ordinal`, id); err != nil {
		return nil, err
	}
	items := make([]AssetMutationItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.item())
	}
	return items, nil
}

type mutationItemRow struct {
	OperationID     string `db:"operation_id"`
	Ordinal         int    `db:"ordinal"`
	RepositoryID    int64  `db:"repository_id"`
	Path            string `db:"path"`
	BeforeExists    bool   `db:"before_exists"`
	BeforeBlobHash  string `db:"before_blob_hash"`
	BeforeSize      int64  `db:"before_size"`
	BeforeType      string `db:"before_type"`
	BeforeSha1      string `db:"before_sha1"`
	BeforeMd5       string `db:"before_md5"`
	BeforeCreatedAt string `db:"before_created_at"`
	BeforeUpdatedAt string `db:"before_updated_at"`
	AfterExists     bool   `db:"after_exists"`
	AfterBlobHash   string `db:"after_blob_hash"`
	AfterSize       int64  `db:"after_size"`
	AfterType       string `db:"after_type"`
	AfterSha1       string `db:"after_sha1"`
	AfterMd5        string `db:"after_md5"`
}

func (r mutationItemRow) item() AssetMutationItem {
	item := AssetMutationItem{OperationID: r.OperationID, Ordinal: r.Ordinal, RepositoryID: r.RepositoryID, Path: r.Path}
	if r.BeforeExists {
		item.Before = &Asset{RepositoryID: r.RepositoryID, Path: r.Path, BlobHash: r.BeforeBlobHash, Size: r.BeforeSize, ContentType: r.BeforeType, Sha1: r.BeforeSha1, Md5: r.BeforeMd5, CreatedAt: r.BeforeCreatedAt, UpdatedAt: r.BeforeUpdatedAt}
	}
	if r.AfterExists {
		item.After = &Asset{RepositoryID: r.RepositoryID, Path: r.Path, BlobHash: r.AfterBlobHash, Size: r.AfterSize, ContentType: r.AfterType, Sha1: r.AfterSha1, Md5: r.AfterMd5}
	}
	return item
}

// ListQuarantine 返回指定操作的隔离条目。
func (r *AssetMutationRepo) ListQuarantine(id string) ([]BlobQuarantine, error) {
	var entries []BlobQuarantine
	err := r.db.Select(&entries, `SELECT operation_id, blob_hash, quarantine_path, status, error FROM blob_quarantine WHERE operation_id=?`, id)
	return entries, err
}

// ListPendingQuarantine 返回仍需恢复或 GC 的隔离条目。
func (r *AssetMutationRepo) ListPendingQuarantine() ([]BlobQuarantine, error) {
	var entries []BlobQuarantine
	err := r.db.Select(&entries, `SELECT operation_id, blob_hash, quarantine_path, status, error FROM blob_quarantine WHERE status IN ('staged','pending_gc') ORDER BY updated_at, operation_id, blob_hash`)
	return entries, err
}

// MarkQuarantine 更新隔离条目的 GC/恢复状态。
func (r *AssetMutationRepo) MarkQuarantine(id, hash, status, reason string) error {
	_, err := r.db.Exec(`UPDATE blob_quarantine SET status=?, error=?, updated_at=datetime('now') WHERE operation_id=? AND blob_hash=?`, status, reason, id, hash)
	return err
}

// CountBlobReferences 返回全库活动资产对 hash 的引用数。
func (r *AssetMutationRepo) CountBlobReferences(hash string) (int, error) {
	var count int
	err := r.db.Get(&count, `SELECT COUNT(*) FROM asset WHERE blob_hash=?`, hash)
	return count, err
}

// GetAsset 读取协调器进行并发快照时所需的当前资产；不存在返回 nil。
func (r *AssetMutationRepo) GetAsset(repositoryID int64, path string) (*Asset, error) {
	var item Asset
	err := r.db.Get(&item, `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at FROM asset WHERE repository_id=? AND path=?`, repositoryID, path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}
