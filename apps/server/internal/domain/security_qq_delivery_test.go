package domain

import (
	"context"
	"time"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-172 QQ 渠道：绑定后通知产生 qq 投递行，mock 发送器验证 openid 解析与结果结算。
func TestSecurityQQDeliveryChannel(t *testing.T) {
	db := mutationTestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin-qq", "hash", "admin"); err != nil {
		t.Fatalf("建管理员：%v", err)
	}
	var uid int64
	if err := db.Get(&uid, `SELECT id FROM user WHERE username='admin-qq'`); err != nil {
		t.Fatal(err)
	}
	factors := repository.NewSecurityAuthFactorRepo(db)
	svc := NewSecurityIncidentService(factors, users)
	p := &auth.Principal{UserID: uid, Role: "admin"}

	// 绑定 → 通知 → qq 投递行。
	if err := svc.SetQQBinding(context.Background(), p, uid, "openid-abc-123"); err != nil {
		t.Fatalf("绑定：%v", err)
	}
	if err := factors.Write(context.Background(), func(tx *sqlx.Tx) error {
		_, e := EmitSecurityNotificationTx(context.Background(), tx, "security.test", "テスト通知", "{}", time.Now())
		return e
	}); err != nil {
		t.Fatalf("发通知：%v", err)
	}
	var qqRows int
	if err := db.Get(&qqRows, `SELECT count(*) FROM security_notification_delivery WHERE channel='qq'`); err != nil {
		t.Fatal(err)
	}
	if qqRows != 1 {
		t.Fatalf("qq 投递行 = %d，应为 1", qqRows)
	}

	// mock 发送器：验证解析出真实 openid，accepted 结算。
	var mu sync.Mutex
	var got []string
	sender := func(ctx context.Context, openid, title, text string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, openid)
		return nil
	}
	n, err := svc.DispatchPendingNotifications(context.Background(), nil, sender)
	if err != nil || n != 1 || len(got) != 1 || got[0] != "openid-abc-123" {
		t.Fatalf("派发 n=%d got=%v err=%v", n, got, err)
	}
	var st string
	if err := db.Get(&st, `SELECT state FROM security_notification_delivery WHERE channel='qq'`); err != nil || st != "accepted" {
		t.Fatalf("state=%q err=%v", st, err)
	}

	// 解绑后再发：新投递行仍建（绑定期快照），但发送解析不到 → target_gone 永久失败。
	if err := svc.RemoveQQBinding(context.Background(), p, uid); err != nil {
		t.Fatalf("解绑：%v", err)
	}
	// 旧行已 accepted；手动插一条 pending qq 行验证 target_gone。
	if err := factors.Write(context.Background(), func(tx *sqlx.Tx) error {
		var seq int64
		if e := tx.GetContext(context.Background(), &seq, `SELECT seq FROM security_notification ORDER BY seq DESC LIMIT 1`); e != nil {
			return e
		}
		return repository.InsertDeliveryTx(context.Background(), tx, repository.SecurityNotificationDeliveryRow{
			DeliveryID: "dlv-qq-gone", Seq: seq, Channel: "qq", TargetMasked: "qq:u1",
			State: "pending", Attempts: 0, NextRetryUnix: time.Now().Unix(),
			CreatedUnix: time.Now().Unix(), UpdatedUnix: time.Now().Unix()})
	}); err != nil {
		t.Fatal(err)
	}
	n, err = svc.DispatchPendingNotifications(context.Background(), nil, sender)
	if err != nil || n != 1 {
		t.Fatalf("派发2 n=%d err=%v", n, err)
	}
	if err := db.Get(&st, `SELECT state FROM security_notification_delivery WHERE delivery_id='dlv-qq-gone'`); err != nil || st != "failed" {
		t.Fatalf("target_gone 应 failed，state=%q err=%v", st, err)
	}
}
