package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// SecurityNotificationRow 站内通知持久投影（seq 为可重放游标，FR-172）。
type SecurityNotificationRow struct {
	Seq         int64  `db:"seq"`
	NoticeID    string `db:"notice_id"`
	Kind        string `db:"kind"`
	Title       string `db:"title"`
	Payload     string `db:"payload"`
	CreatedUnix int64  `db:"created_unix"`
}

type SecurityNotificationDeliveryRow struct {
	DeliveryID    string `db:"delivery_id"`
	Seq           int64  `db:"seq"`
	Channel       string `db:"channel"`
	TargetMasked  string `db:"target_masked"`
	State         string `db:"state"`
	Attempts      int64  `db:"attempts"`
	LastError     string `db:"last_error"`
	NextRetryUnix int64  `db:"next_retry_unix"`
	CreatedUnix   int64  `db:"created_unix"`
	UpdatedUnix   int64  `db:"updated_unix"`
}

// EmitNotificationTx 同事务写入站内条目并注册站外投递（按渠道各一行）。
func EmitNotificationTx(ctx context.Context, tx *sqlx.Tx, n SecurityNotificationRow, deliveries []SecurityNotificationDeliveryRow) (int64, error) {
	res, err := tx.NamedExecContext(ctx, `INSERT INTO security_notification (notice_id,kind,title,payload,created_unix)
 VALUES (:notice_id,:kind,:title,:payload,:created_unix)`, n)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, d := range deliveries {
		d.Seq = seq
		if _, err := tx.NamedExecContext(ctx, `INSERT INTO security_notification_delivery
 (delivery_id,seq,channel,target_masked,state,attempts,last_error,next_retry_unix,created_unix,updated_unix)
 VALUES (:delivery_id,:seq,:channel,:target_masked,:state,:attempts,:last_error,:next_retry_unix,:created_unix,:updated_unix)`, d); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

// NotificationsAfterTx 游标补查：after 之后的有界页（含 ack 状态按 user 联查）。
func NotificationsAfterTx(ctx context.Context, tx *sqlx.Tx, userID, after int64, limit int) ([]SecurityNotificationRow, error) {
	var out []SecurityNotificationRow
	return out, tx.SelectContext(ctx, &out, `SELECT seq,notice_id,kind,title,payload,created_unix FROM security_notification
 WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
}

// NotificationWatermarkTx 当前序列水位（无条目为 0）。
func NotificationWatermarkTx(ctx context.Context, tx *sqlx.Tx) (int64, error) {
	var w int64
	return w, tx.GetContext(ctx, &w, `SELECT COALESCE(MAX(seq),0) FROM security_notification`)
}

// AckNotificationTx 幂等记录本人确认；notice 不可见返回 ErrNotFound。
func AckNotificationTx(ctx context.Context, tx *sqlx.Tx, noticeID string, userID, ackUnix int64) (bool, error) {
	var seq int64
	if err := tx.GetContext(ctx, &seq, `SELECT seq FROM security_notification WHERE notice_id=?`, noticeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO security_notification_ack (seq,user_id,ack_unix) VALUES (?,?,?)`, seq, userID, ackUnix)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// NotificationAckedTx 本人是否已确认。
func NotificationAckedTx(ctx context.Context, tx *sqlx.Tx, seq, userID int64) (bool, error) {
	var n int
	if err := tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM security_notification_ack WHERE seq=? AND user_id=?`, seq, userID); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListDeliveriesTx 脱敏投递分页（seq 倒序界）。
func ListDeliveriesTx(ctx context.Context, tx *sqlx.Tx, limit int) ([]SecurityNotificationDeliveryRow, error) {
	var out []SecurityNotificationDeliveryRow
	return out, tx.SelectContext(ctx, &out, `SELECT * FROM security_notification_delivery ORDER BY created_unix DESC LIMIT ?`, limit)
}
