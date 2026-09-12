package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// RepositoryDeleteJournal 保存仓库级级联删除的回滚快照。
type RepositoryDeleteJournal struct {
	Repository     Repository       `json:"repository"`
	ACL            []Acl            `json:"acl"`
	FormatMetadata []FormatMetadata `json:"formatMetadata"`
}

// PutRepositoryDeleteJournal 在仓库删除事务中保存可恢复的级联数据。
func PutRepositoryDeleteJournal(tx *sqlx.Tx, operationID string, snapshot RepositoryDeleteJournal) error {
	repositoryJSON, err := json.Marshal(snapshot.Repository)
	if err != nil {
		return err
	}
	aclJSON, err := json.Marshal(snapshot.ACL)
	if err != nil {
		return err
	}
	metadataJSON, err := json.Marshal(snapshot.FormatMetadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO repository_delete_journal
		(operation_id, repository_json, acl_json, format_metadata_json) VALUES (?, ?, ?, ?)`,
		operationID, repositoryJSON, aclJSON, metadataJSON)
	return err
}

func restoreRepositoryDeleteJournal(tx *sqlx.Tx, operationID string) error {
	var row struct {
		RepositoryJSON     string `db:"repository_json"`
		ACLJSON            string `db:"acl_json"`
		FormatMetadataJSON string `db:"format_metadata_json"`
	}
	err := tx.Get(&row, `SELECT repository_json, acl_json, format_metadata_json
		FROM repository_delete_journal WHERE operation_id=?`, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot RepositoryDeleteJournal
	if err := json.Unmarshal([]byte(row.RepositoryJSON), &snapshot.Repository); err != nil {
		return fmt.Errorf("解析仓库删除回滚快照：%w", err)
	}
	if err := json.Unmarshal([]byte(row.ACLJSON), &snapshot.ACL); err != nil {
		return fmt.Errorf("解析仓库 ACL 回滚快照：%w", err)
	}
	if err := json.Unmarshal([]byte(row.FormatMetadataJSON), &snapshot.FormatMetadata); err != nil {
		return fmt.Errorf("解析格式元数据回滚快照：%w", err)
	}
	if _, err := tx.Exec(`INSERT INTO repository
		(id, name, format, type, visibility, description, config, online, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		snapshot.Repository.ID, snapshot.Repository.Name, snapshot.Repository.Format, snapshot.Repository.Type,
		snapshot.Repository.Visibility, snapshot.Repository.Description, snapshot.Repository.Config,
		snapshot.Repository.Online, snapshot.Repository.CreatedAt); err != nil {
		return err
	}
	for _, entry := range snapshot.ACL {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO acl (repository_id, subject_id, action) VALUES (?, ?, ?)`,
			snapshot.Repository.ID, entry.SubjectID, entry.Action); err != nil {
			return err
		}
	}
	metadata := NewFormatMetadataRepo(nil)
	for _, entry := range snapshot.FormatMetadata {
		if err := metadata.PutWithTimeTx(tx, entry); err != nil {
			return err
		}
	}
	return nil
}
