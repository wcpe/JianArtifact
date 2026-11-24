package main

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestAdmissionDecisionEndToEnd 覆盖 FR-169 最小流水线：私有候选→检测→待审→管理员批准提升为公开资产/驳回保持私有。
func TestAdmissionDecisionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "raw")
	const ownerName, adminName = "decide-owner", "decide-admin"
	if _, _, err := app.authSvc.Bootstrap(adminName, auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create(ownerName, auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("decide-raw", "raw", "hosted", "public", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.NewAclRepo(app.db).Replace(repo.ID, []repository.Acl{{SubjectID: owner.ID, Action: repository.ActionPublish}}); err != nil {
		t.Fatal(err)
	}
	if err := app.securityAudit.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newApplicationHandler(cfg, app, nil))
	t.Cleanup(server.Close)
	login := func(name string) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodPost, "/api/v1/auth/login", "application/json",
			fmt.Sprintf(`{"username":%q,"password":%q}`, name, auditTestPassword), "")
		if status != http.StatusOK {
			t.Fatalf("登录失败：%d %s", status, body)
		}
		return queryDecode[struct {
			Token string `json:"token"`
		}](t, body).Token
	}
	ownerToken, adminToken := login(ownerName), login(adminName)
	req := httptest.NewRequest(http.MethodPost, "http://localhost/internal-candidate-test", nil)
	req.RemoteAddr = "127.0.0.1:49153"
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	principal, err := auth.NewAuthenticator(app.jwt, app.store).AdmissionPrincipal(req)
	if err != nil {
		t.Fatal(err)
	}
	receiveCtx := auditctx.WithSource(t.Context(), cfg.SourcePolicy.Resolve(req, rand.Text()))

	prepare := func(path, content string) string {
		t.Helper()
		var op string
		st, err := app.admission.Prepare(receiveCtx, principal, repo.ID, path, "text/plain", strings.NewReader(content), func(id string) { op = id })
		if err != nil || st.OperationID != op {
			t.Fatalf("候选登记失败：%v", err)
		}
		return op
	}
	decide := func(op, action, credential string, want int) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodPost,
			"/api/v1/security/admission/candidates/"+op+"/decisions", "application/json",
			fmt.Sprintf(`{"action":%q,"reason":%q}`, action, "复核通过"), credential)
		if status != want {
			t.Fatalf("裁决 %s 返回 %d，期望 %d：%s", action, status, want, body)
		}
		return body
	}
	download := func(path string, want int) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodGet, "/repository/"+repo.Name+"/"+path, "", "", "")
		if status != want {
			t.Fatalf("下载 %s 返回 %d，期望 %d", path, status, want)
		}
		return body
	}
	state := func(op, credential string) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodGet, "/api/v1/security/admission/candidates/"+op, "", "", credential)
		if status != http.StatusOK {
			t.Fatalf("状态查询失败：%d %s", status, body)
		}
		return queryDecode[struct {
			State            string `json:"state"`
			PublishedAssetID *int64 `json:"publishedAssetId"`
		}](t, body).State
	}

	// 待审前：管理员不能裁决非待审；非管理员一律 403。
	approved := prepare("approved.txt", "获准发布的字节")
	download("approved.txt", http.StatusNotFound)
	if st := state(approved, ownerToken); st != "awaiting_review" {
		t.Fatalf("检测通过应进入待审：%s", st)
	}
	decide(approved, "approve", ownerToken, http.StatusForbidden)
	decide(approved, "decide", adminToken, http.StatusBadRequest)

	// 批准 → 提升字节为公开资产；重复裁决 409。
	body := decide(approved, "approve", adminToken, http.StatusOK)
	result := queryDecode[struct {
		State            string `json:"state"`
		PublishedAssetID *int64 `json:"publishedAssetId"`
	}](t, body)
	if result.State != "published" || result.PublishedAssetID == nil {
		t.Fatalf("批准后应发布且携带资产号：%s", body)
	}
	if got := download("approved.txt", http.StatusOK); got != "获准发布的字节" {
		t.Fatalf("提升后公开字节不符：%q", got)
	}
	decide(approved, "approve", adminToken, http.StatusConflict)

	// 驳回 → rejected，字节不公开。
	rejected := prepare("rejected.txt", "应被驳回的字节")
	if st := state(rejected, ownerToken); st != "awaiting_review" {
		t.Fatalf("检测通过应进入待审：%s", st)
	}
	decide(rejected, "reject", adminToken, http.StatusOK)
	if st := state(rejected, ownerToken); st != "rejected" {
		t.Fatalf("驳回后应为 rejected：%s", st)
	}
	download("rejected.txt", http.StatusNotFound)
	decide(rejected, "approve", adminToken, http.StatusConflict)

	// 不存在的操作号统一 404。
	decide("00000000-0000-4000-8000-000000000000", "approve", adminToken, http.StatusNotFound)
}
