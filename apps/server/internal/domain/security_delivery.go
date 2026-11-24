package domain

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-172 站外投递派发：security_notification_delivery の持久行を逐次処理する。
// 送信はトランザクション外で行い、結果だけを次のトランザクションで確定する
// （DB トランザクション内でネットワーク待ちしない）。
// SMTP 未設定・QQ 未整備の渠道は failed+原因記録で見える化し、成功を偽装しない。

const (
	// deliveryMaxAttempts は永続再試行の上限。超過で failed 終態。
	deliveryMaxAttempts = 8
	// deliveryAttemptTimeout は 1 回の送信試行の上限（30 秒初回予算に合わせる）。
	deliveryAttemptTimeout = 30 * time.Second
	// deliveryRetryBase は再試行間隔の基数（秒）。attempts 回に比例して線形に伸ばす。
	deliveryRetryBase = 60
)

// maskEmail は保存用の脱敏表現を返す（例: h***@example.com）。配送先の実値は永続化しない。
func maskEmail(email string) string {
	local, domainPart, ok := strings.Cut(email, "@")
	if !ok {
		return "***"
	}
	if len(local) <= 1 {
		return local + "***@" + domainPart
	}
	return local[:1] + "***@" + domainPart
}

// emitDeliveriesForNotificationTx は通知 seq に紐づく站外投递行を同じ写トランザクションで登録する。
// SMTP：現時点の有効管理者の登録メール宛に 1 人 1 行（宛先は脱敏のみ保存）。
// QQ：BotGo 等の平台前提が未整備のため行を作らない（阻塞項として终验に記載）。
func emitDeliveriesForNotificationTx(ctx context.Context, tx *sqlx.Tx, seq int64, now time.Time) error {
	emails, err := repository.AdminEmailsTx(ctx, tx)
	if err != nil {
		return err
	}
	unix := now.Unix()
	for _, e := range emails {
		d := repository.SecurityNotificationDeliveryRow{
			DeliveryID: "dlv-" + uuid.NewString(), Seq: seq, Channel: "smtp",
			TargetMasked: maskEmail(e), State: "pending", Attempts: 0,
			NextRetryUnix: unix, CreatedUnix: unix, UpdatedUnix: unix,
		}
		if err := repository.InsertDeliveryTx(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}

// NotificationMailer は 1 通の送信を行う差し込み関数（SMTP 実装は装配層）。
// ctx に試行タイムアウトが載る。
type NotificationMailer func(ctx context.Context, to, subject, text string) error

// DispatchPendingNotifications は到期待投递行を认领→送信→结算する。处理件数を返す。
// mailer が nil（SMTP 未設定）でも待機行を failed で可視化するため呼び出される。
func (s *SecurityIncidentService) DispatchPendingNotifications(ctx context.Context, mailer NotificationMailer) (int, error) {
	now := s.now().Unix()
	var claimed []repository.SecurityNotificationDeliveryRow
	err := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
		var e error
		claimed, e = repository.ClaimDueDeliveriesTx(ctx, tx, now, 50)
		return e
	})
	if err != nil || len(claimed) == 0 {
		return 0, err
	}
	// 宛先は脱敏のみ保存しているため、送信時に管理者メール一覧から復元する。
	// 脱敏形式と現在のメール一覧が一致しない（退会・変更）は target_gone で失敗可視化。
	var emails []string
	err = s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var e error
		emails, e = repository.AdminEmailsTx(ctx, tx)
		return e
	})
	if err != nil {
		return 0, err
	}
	realTarget := map[string]string{}
	for _, e := range emails {
		realTarget[maskEmail(e)] = e
	}
	done := 0
	for _, d := range claimed {
		attempts := d.Attempts + 1
		var sendErr error
		permanent := false // 設定起因の恒久的失敗は再試行せず即 failed（可視化して止める）
		target := realTarget[d.TargetMasked]
		switch {
		case d.Channel == "qq":
			sendErr, permanent = errStr("qq_channel_not_configured"), true
		case mailer == nil:
			sendErr, permanent = errStr("smtp_not_configured"), true
		case target == "":
			sendErr, permanent = errStr("target_gone"), true
		default:
			actx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
			subject := "[JianArtifact 安全通知] " + notificationTitleOf(ctx, s, d.Seq)
			body := subject + "\n\n安全コンソール（/security）で確認してください。"
			sendErr = mailer(actx, target, subject, body)
			cancel()
		}
		if permanent {
			attempts = deliveryMaxAttempts
		}
		errStrVal := ""
		if sendErr != nil {
			errStrVal = sendErr.Error()
			if len(errStrVal) > 200 {
				errStrVal = errStrVal[:200]
			}
		}
		next := now + int64(deliveryRetryBase)*attempts
		if werr := s.repo.Write(ctx, func(tx *sqlx.Tx) error {
			return repository.SettleDeliveryTx(ctx, tx, d.DeliveryID, sendErr == nil, errStrVal, s.now().Unix(), next, attempts)
		}); werr != nil {
			return done, werr
		}
		done++
	}
	return done, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errStr(s string) error    { return strErr(s) }

// notificationTitleOf は通知件名を取得する（件名不在なら kind を返す）。
func notificationTitleOf(ctx context.Context, s *SecurityIncidentService, seq int64) string {
	var title string
	if err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
		var e error
		title, e = repository.NotificationTitleTx(ctx, tx, seq)
		return e
	}); err != nil || title == "" {
		return "security notification"
	}
	return title
}
