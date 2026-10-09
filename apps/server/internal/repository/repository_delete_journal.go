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
	// 恢复 ACL 时必须带上主体类型与组 ID：组主体行的 subject_id 为 NULL、
	// 授权信息全在 subject_group_id 上，只写三列会让组授权在回滚后静默丢失
	// （用户主体只靠 subject_type 的列默认值兜底、看不出问题，故极易漏）。
	for _, entry := range snapshot.ACL {
		if entry.SubjectType == SubjectTypeGroup {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO acl (repository_id, subject_id, subject_type, subject_group_id, action)
				VALUES (?, NULL, ?, ?, ?)`,
				snapshot.Repository.ID, SubjectTypeGroup, entry.SubjectGroupID, entry.Action,
			); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO acl (repository_id, subject_id, subject_type, subject_group_id, action)
			VALUES (?, ?, ?, NULL, ?)`,
			snapshot.Repository.ID, entry.SubjectID, SubjectTypeUser, entry.Action,
		); err != nil {
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
