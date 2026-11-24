package domain

import (
	"context"
	"net/netip"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-171：拒否バーストが規則命中→自動建案→通知まで通ること、再投影で増えないこと。
func TestSecurityFindingBurstOpensCase(t *testing.T) {
	db := mutationTestDB(t)
	audit := NewSecurityAuditService(db)
	if err := audit.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("192.0.2.77")
	ctx := WithSecurityAuditIdentity(securityAuditDirect(ip), SecurityAuditIdentity{
		ActorUserID: 7, AuthMethod: "bearer_token", AuthStrength: "machine", CredentialID: "550e8400-e29b-41d4-a716-446655440000", SessionID: "550e8400-e29b-41d4-a716-446655440001"})
	for i := 0; i < 12; i++ {
		audit.RecordDenied(ctx, SecurityAuditDenied{
			EventType: "protocol.write_denied", ErrorCode: "forbidden", AuthMethod: "bearer_token",
			ActorUserID: 7, HTTPStatus: 403})
	}
	if err := audit.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	// 采集层对同源有限流抑制，可能少于 12；阈值 10 以上即可。
	if err := db.Get(&eventCount, `SELECT count(*) FROM security_event WHERE outcome='denied'`); err != nil || eventCount < 10 {
		t.Fatalf("前提イベント = %d err=%v，期望 >=10", eventCount, err)
	}

	fs := NewSecurityFindingService(repository.NewSecurityAuthFactorRepo(db))
	n, err := fs.RunBatch(context.Background())
	if err != nil {
		t.Fatalf("投影失敗：%v", err)
	}
	// 3 つの安定錨点（user/cred/ip）各 1 命中。
	if n != 3 {
		t.Fatalf("命中数 = %d，期望 3", n)
	}
	var cases int
	if err := db.Get(&cases, `SELECT count(*) FROM security_case WHERE opened_by='rule-engine' AND state='open'`); err != nil || cases < 1 {
		t.Fatalf("自動建案 = %d err=%v", cases, err)
	}
	var linked int
	if err := db.Get(&linked, `SELECT count(*) FROM security_case_link`); err != nil || linked < 1 {
		t.Fatalf("案件証拠リンク = %d", linked)
	}
	var notices int
	if err := db.Get(&notices, `SELECT count(*) FROM security_notification WHERE kind='security.case_opened'`); err != nil || notices < 1 {
		t.Fatalf("自動建案通知 = %d", notices)
	}

	// 再投影：同じ事実を再計算しても命中・案件は増えない（決定的 ID + OR IGNORE）。
	n2, err := fs.RunBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var total int
	if err := db.Get(&total, `SELECT count(*) FROM security_finding`); err != nil {
		t.Fatal(err)
	}
	if n2 != 0 || total != 3 {
		t.Fatalf("再投影で増加: 新規=%d 計=%d", n2, total)
	}
}
