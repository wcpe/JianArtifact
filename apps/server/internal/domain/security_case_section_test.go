package domain

import (
	"context"
	"net/netip"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// FR-171：案件四栏详情——timeline/uploads/identity/actions-evidence。
func TestSecurityCaseSections(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	audit := NewSecurityAuditService(db)
	if err := audit.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("192.0.2.99")
	ctx := WithSecurityAuditIdentity(securityAuditDirect(ip), SecurityAuditIdentity{
		ActorUserID: 7, AuthMethod: "bearer_token", AuthStrength: "machine",
		CredentialID: "550e8400-e29b-41d4-a716-446655440000", SessionID: "550e8400-e29b-41d4-a716-446655440001"})
	audit.RecordDenied(ctx, SecurityAuditDenied{
		EventType: "protocol.write_denied", ErrorCode: "forbidden", AuthMethod: "bearer_token",
		ActorUserID: 7, RepositoryID: repoID, CanonicalPath: "pkg/blocked.jar", HTTPStatus: 403})
	if err := audit.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := db.Get(&eventID, `SELECT event_id FROM security_event LIMIT 1`); err != nil || eventID == "" {
		t.Fatalf("前提事件 = %q err=%v", eventID, err)
	}

	svc := NewSecurityIncidentService(repository.NewSecurityAuthFactorRepo(db), repository.NewUserRepo(db))
	p := &auth.Principal{UserID: 1, Role: "admin"}
	c, err := svc.OpenSecurityCase(ctx, p, "四栏テスト", "high", "テスト",
		[]repository.SecurityCaseLinkRow{{EventID: eventID, LinkKind: "fact"}})
	if err != nil {
		t.Fatalf("建案：%v", err)
	}

	d, err := svc.SecurityCaseDetail(context.Background(), p, c.CaseID)
	if err != nil || d.Case.CaseID != c.CaseID || len(d.Links) != 1 {
		t.Fatalf("详情 err=%v links=%d", err, len(d.Links))
	}

	tl, err := svc.SecurityCaseTimeline(context.Background(), p, c.CaseID, 10)
	if err != nil || len(tl) != 1 || tl[0].EventID != eventID || tl[0].Outcome != "denied" {
		t.Fatalf("timeline err=%v items=%+v", err, tl)
	}
	up, err := svc.SecurityCaseUploads(context.Background(), p, c.CaseID, 10)
	if err != nil || len(up) != 1 || up[0].CanonicalPath != "pkg/blocked.jar" {
		t.Fatalf("uploads err=%v items=%+v", err, up)
	}
	idn, err := svc.SecurityCaseIdentity(context.Background(), p, c.CaseID, 10)
	if err != nil || len(idn) != 1 || idn[0].ActorUserID == nil || *idn[0].ActorUserID != 7 {
		t.Fatalf("identity err=%v items=%+v", err, idn)
	}
	ae, err := svc.SecurityCaseActionsEvidence(context.Background(), p, c.CaseID, 10)
	if err != nil || len(ae.Findings) != 0 || len(ae.Holds) != 0 {
		t.Fatalf("actions-evidence err=%v %+v", err, ae)
	}

	// 非管理员拒绝。
	if _, err := svc.SecurityCaseTimeline(context.Background(), &auth.Principal{UserID: 9, Role: "user"}, c.CaseID, 10); err != ErrAdmissionForbidden {
		t.Fatalf("非管理员应被拒，err=%v", err)
	}
	// 不存在案件 → 错误。
	if _, err := svc.SecurityCaseTimeline(context.Background(), p, "case-none", 10); err == nil {
		t.Fatal("不存在案件应报错")
	}
}
