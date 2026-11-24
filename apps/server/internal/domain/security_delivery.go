package domain

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// emailAddrPattern 匹配错误串中残留的 user@host 形态地址用于脱敏。
var emailAddrPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// 安全治理站外投递派发：逐条处理 security_notification_delivery 的持久行。
// 发送在事务外执行，仅结果在下一事务内结算
// （DB 事务内不等待网络）。
// SMTP 未配置、QQ 未就绪的渠道以 failed+原因记录可视化，不伪造成功。

const (
	// deliveryMaxAttempts 为持久重试上限；超过即 failed 终态。
	deliveryMaxAttempts = 8
	// deliveryAttemptTimeout 为单次发送尝试上限（与 30 秒首次预算对齐）。
	deliveryAttemptTimeout = 30 * time.Second
	// deliveryRetryBase 为重试间隔基数（秒），随 attempts 线性增长。
	deliveryRetryBase = 60
)

// maskEmail 返回脱敏表示（例: h***@example.com）；真实收件地址不持久化。
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

// emitDeliveriesForNotificationTx 在同一写事务内登记与通知 seq 关联的站外投递行。
// SMTP：为当前全部有效管理员的注册邮箱各登记 1 行（收件地址仅脱敏保存）。
// QQ：为已绑定 qq 的管理员各登记 1 行（收件地址仅保存 qq:u<uid> 不透明标识，
// 真实 openid 在发送时从绑定表还原）。
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

// qqMaskedTarget 是 QQ 投递行的脱敏收件表示（仅 user_id 标识，不含 openid）。
func qqMaskedTarget(userID int64) string {
	return "qq:u" + strconv.FormatInt(userID, 10)
}

// qqTargetUserID 从脱敏收件地址还原 user_id；格式非法返回 false。
func qqTargetUserID(masked string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(masked, "qq:u"), 10, 64)
	if err != nil || id <= 0 || !strings.HasPrefix(masked, "qq:u") {
		return 0, false
	}
	return id, true
}

// NotificationMailer 是发送单封邮件的注入函数（SMTP 实现在装配层）；
// ctx 携带单次尝试超时。
type NotificationMailer func(ctx context.Context, to, subject, text string) error

// NotificationQQSender 是发送单条 QQ 消息的注入函数
// （官方 API 适配器在装配层；未就绪则传 nil，以永久失败可视化）。
type NotificationQQSender func(ctx context.Context, openid, title, text string) error

// DispatchPendingNotifications 将到期待投递行认领→发送→结算，返回处理件数。
// mailer/qq 为 nil（渠道未配置）也会被调用——待投递行以 failed 可视化。
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
	// 收件地址仅脱敏保存，发送时从当前管理员邮箱列表还原。
	// 脱敏形式与当前邮箱列表不一致（注销/变更）以 target_gone 失败可视化。
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
	// QQ：收件地址在每次发送时从绑定表还原（已解绑的地址为 target_gone）。
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
		permanent := false // 配置导致的永久失败不重试，直接 failed（可视化后停止）
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
					sendErr = qq(actx, openid, title, title+"\n\n请在安全控制台（/security）查看。")
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
			body := subject + "\n\n请在安全控制台（/security）查看。"
			sendErr = mailer(actx, target, subject, body)
			cancel()
		}
		if permanent {
			attempts = deliveryMaxAttempts
		}
		errStrVal := ""
		if sendErr != nil {
			// 中继错误串可能携带完整收件地址；与投递行同口径脱敏后落库。
			errStrVal = sanitizeDeliveryErr(sendErr.Error(), target)
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

// sanitizeDeliveryErr 对落库的中继错误串脱敏：以收件目标为中心，
// 先整体替换精确目标，再对残留的 user@host 形态地址逐项脱敏。
func sanitizeDeliveryErr(msg, target string) string {
	if target != "" {
		masked := target
		if strings.Contains(target, "@") {
			masked = maskEmail(target)
		}
		msg = strings.ReplaceAll(msg, target, masked)
	}
	return emailAddrPattern.ReplaceAllStringFunc(msg, func(m string) string { return maskEmail(m) })
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errStr(s string) error    { return strErr(s) }

// notificationTitleOf 取通知主题（无主题回退 kind）。
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
