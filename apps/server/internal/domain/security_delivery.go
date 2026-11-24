package domain

import (
	"context"
	"strconv"
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
// QQ：qq 绑定済みの管理者宛に 1 人 1 行（宛先は qq:u<uid> の不透明識別子のみ保存、
// 実 openid は送信時に绑定表から復元する）。
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
	bindings, err := repository.AdminQQBindingsTx(ctx, tx)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		d := repository.SecurityNotificationDeliveryRow{
			DeliveryID: "dlv-" + uuid.NewString(), Seq: seq, Channel: "qq",
			TargetMasked: qqMaskedTarget(b.UserID), State: "pending", Attempts: 0,
			NextRetryUnix: unix, CreatedUnix: unix, UpdatedUnix: unix,
		}
		if err := repository.InsertDeliveryTx(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}

// qqMaskedTarget は QQ 投递行の脱敏宛先表現（user_id 識別子のみ、openid を含まない）。
func qqMaskedTarget(userID int64) string {
	return "qq:u" + strconv.FormatInt(userID, 10)
}

// qqTargetUserID は脱敏宛先から user_id を復元する。形式不正は false。
func qqTargetUserID(masked string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(masked, "qq:u"), 10, 64)
	if err != nil || id <= 0 || !strings.HasPrefix(masked, "qq:u") {
		return 0, false
	}
	return id, true
}

// NotificationMailer は 1 通の送信を行う差し込み関数（SMTP 実装は装配層）。
// ctx に試行タイムアウトが載る。
type NotificationMailer func(ctx context.Context, to, subject, text string) error

// NotificationQQSender は QQ チャンネル 1 件の送信を行う差し込み関数
// （公式 API アダプタは装配層；未整備なら nil で恒久的失敗可視化）。
type NotificationQQSender func(ctx context.Context, openid, title, text string) error

// DispatchPendingNotifications は到期待投递行を认领→送信→结算する。处理件数を返す。
// mailer/qq が nil（渠道未設定）でも待機行を failed で可視化するため呼び出される。
func (s *SecurityIncidentService) DispatchPendingNotifications(ctx context.Context, mailer NotificationMailer, qq NotificationQQSender) (int, error) {
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
	// QQ：宛先復元は送信ごとに绑定表から行う（解绑された宛先は target_gone）。
	qqOpenid := func(uid int64) string {
		var openid string
		if err := s.repo.Read(ctx, func(tx *sqlx.Tx) error {
			var e error
			openid, e = repository.QQOpenidByUserTx(ctx, tx, uid)
			return e
		}); err != nil {
			return ""
		}
		return openid
	}
	done := 0
	for _, d := range claimed {
		attempts := d.Attempts + 1
		var sendErr error
		permanent := false // 設定起因の恒久的失敗は再試行せず即 failed（可視化して止める）
		target := realTarget[d.TargetMasked]
		switch {
		case d.Channel == "qq":
			uid, ok := qqTargetUserID(d.TargetMasked)
			switch {
			case qq == nil:
				sendErr, permanent = errStr("qq_channel_not_configured"), true
			case !ok:
				sendErr, permanent = errStr("target_gone"), true
			default:
				openid := qqOpenid(uid)
				if openid == "" {
					sendErr, permanent = errStr("target_gone"), true
				} else {
					actx, cancel := context.WithTimeout(ctx, deliveryAttemptTimeout)
					title := "[JianArtifact 安全通知] " + notificationTitleOf(ctx, s, d.Seq)
					sendErr = qq(actx, openid, title, title+"\n\n安全コンソール（/security）で確認してください。")
					cancel()
				}
			}
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
