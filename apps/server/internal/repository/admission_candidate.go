package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ErrAdmissionCommitUnknown 只表示 COMMIT 回报不明，调用方须重新查询原操作。
var ErrAdmissionCommitUnknown = errors.New("候选提交结果未知")

// AdmissionCandidateRow 是库内私有状态，不能作为 HTTP 响应序列化。
type AdmissionCandidateRow struct {
	OperationID        string  `db:"operation_id" json:"-"`
	ObjectKey          string  `db:"object_key" json:"-"`
	BootID             string  `db:"boot_id" json:"-"`
	RepositoryID       int64   `db:"repository_id" json:"-"`
	ActorUserID        int64   `db:"actor_user_id" json:"-"`
	SessionID          string  `db:"session_id" json:"-"`
	AuthMethod         string  `db:"auth_method" json:"-"`
	AccountEpoch       int64   `db:"account_epoch" json:"-"`
	OriginRequestID    string  `db:"origin_request_id" json:"-"`
	ClientIP           string  `db:"client_ip" json:"-"`
	ProxyTrust         string  `db:"proxy_trust" json:"-"`
	SourceTrustVersion string  `db:"source_trust_version" json:"-"`
	CanonicalPath      string  `db:"canonical_path" json:"-"`
	ContentType        string  `db:"content_type" json:"-"`
	State              string  `db:"state" json:"-"`
	TargetVersion      int64   `db:"target_version" json:"-"`
	ManifestVersion    *int64  `db:"manifest_version" json:"-"`
	ManifestDigest     *string `db:"manifest_digest" json:"-"`
	BlobHash           *string `db:"blob_hash" json:"-"`
	SizeBytes          *int64  `db:"size_bytes" json:"-"`
	StagedEventID      *string `db:"staged_event_id" json:"-"`
	ErrorCode          string  `db:"error_code" json:"-"`
	ExpiresUnix        int64   `db:"expires_unix" json:"-"`
}

type AdmissionCandidateRepo struct{ db *persistence.DB }

func NewAdmissionCandidateRepo(db *persistence.DB) *AdmissionCandidateRepo {
	return &AdmissionCandidateRepo{db: db}
}

// Read 让资格和对象的读取使用同一 SQLite 快照。
func (r *AdmissionCandidateRepo) Read(ctx context.Context, fn func(*sqlx.Tx) error) error {
	tx, err := r.db.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// Write 只运行短事务；候选文件读写必须在此方法外。
func (r *AdmissionCandidateRepo) Write(ctx context.Context, fn func(*sqlx.Tx) error, gate func(func() error) error) error {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return gate(func() error {
		if err := tx.Commit(); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return fmt.Errorf("%w：%v", ErrAdmissionCommitUnknown, err)
		}
		return nil
	})
}

func (r *AdmissionCandidateRepo) GetTx(ctx context.Context, tx *sqlx.Tx, operationID string, owner int64) (AdmissionCandidateRow, error) {
	var row AdmissionCandidateRow
	err := tx.GetContext(ctx, &row, `SELECT * FROM private_admission_candidate WHERE operation_id=? AND actor_user_id=?`, operationID, owner)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return row, err
}

func (r *AdmissionCandidateRepo) InsertTx(ctx context.Context, tx *sqlx.Tx, row AdmissionCandidateRow) error {
	_, err := tx.NamedExecContext(ctx, `INSERT INTO private_admission_candidate
 (operation_id,object_key,boot_id,repository_id,actor_user_id,session_id,auth_method,account_epoch,origin_request_id,client_ip,proxy_trust,source_trust_version,canonical_path,content_type,state,expires_unix)
 VALUES (:operation_id,:object_key,:boot_id,:repository_id,:actor_user_id,:session_id,:auth_method,:account_epoch,:origin_request_id,:client_ip,:proxy_trust,:source_trust_version,:canonical_path,:content_type,'receiving',:expires_unix)`, row)
	return err
}

func (r *AdmissionCandidateRepo) SealTx(ctx context.Context, tx *sqlx.Tx, row AdmissionCandidateRow) error {
	result, err := tx.NamedExecContext(ctx, `UPDATE private_admission_candidate SET state='staged',target_version=target_version+1,
 manifest_version=1,manifest_digest=:manifest_digest,blob_hash=:blob_hash,size_bytes=:size_bytes,staged_event_id=:staged_event_id
 WHERE operation_id=:operation_id AND object_key=:object_key AND boot_id=:boot_id AND target_version=:target_version AND state='receiving'`, row)
	return admissionCandidateUpdated(result, err)
}

func (r *AdmissionCandidateRepo) TransitionTx(ctx context.Context, tx *sqlx.Tx, row AdmissionCandidateRow, state, code string) error {
	result, err := tx.ExecContext(ctx, `UPDATE private_admission_candidate SET state=?,error_code=?,target_version=target_version+1
 WHERE operation_id=? AND target_version=? AND state=?`, state, code, row.OperationID, row.TargetVersion, row.State)
	return admissionCandidateUpdated(result, err)
}

func admissionCandidateUpdated(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrNotFound
	}
	return err
}

// AdmissionPublishGrantTx 按既有动作蕴含及当前用户/组授权读取，不按参数伪造组织权限。
func AdmissionPublishGrantTx(ctx context.Context, tx *sqlx.Tx, repoID, userID int64) (bool, error) {
	query, args, err := sqlx.In(`SELECT EXISTS (SELECT 1 FROM acl
 WHERE repository_id=? AND action IN (?) AND ((subject_type='user' AND subject_id=?) OR
 (subject_type='group' AND subject_group_id IN (SELECT group_id FROM user_group_member WHERE user_id=?))))`, repoID, satisfyingGrants(ActionPublish), userID, userID)
	if err != nil {
		return false, err
	}
	var allowed bool
	err = tx.GetContext(ctx, &allowed, query, args...)
	return allowed, err
}
