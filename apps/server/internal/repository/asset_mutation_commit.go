package repository

import (
	"context"
	"database/sql"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrAssetMutationCommitUnconfirmed 只标记完成事务的 Commit 调用返回错误。
// 提交前的失败不带此标记；提交错误须先查原回执，不能直接按失败补偿。
var ErrAssetMutationCommitUnconfirmed = errors.New("资产操作提交结果未确认")

// ErrAssetMutationTransition 表示原 intent 不存在或不允许阶段转换。
var ErrAssetMutationTransition = errors.New("资产操作不在允许转换的阶段")

func requireMutationTransition(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrAssetMutationTransition
	}
	return nil
}

// ReadCompletionStatus 以一次独立的 SELECT 读取持久回执，不开启写事务。
func (r *AssetMutationRepo) ReadCompletionStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := r.db.GetContext(ctx, &status, `SELECT status FROM asset_mutation WHERE id=?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// IsSQLiteWriteUnavailable 仅识别 SQLite 驱动给出的暂不可写错误，含扩展码。
func IsSQLiteWriteUnavailable(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_FULL:
		return true
	default:
		return false
	}
}
