package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
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

// TestRawPutViaAdmissionEndToEnd 交互会话的 hosted Raw PUT 经私有候选准入：净内容自动发布，
// 字节在提升前不可经公开路径读取；候选状态持久为 published。
func TestRawPutViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "raw")
	if _, _, err := app.authSvc.Bootstrap("raw-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("raw-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("raw-adm-hosted", "raw", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "raw-issuer")
	if err != nil {
		t.Fatal(err)
	}

	// 凭据 PUT → 准入管线 → 净内容提升并返回 201（协议主体为机器凭据）。
	status, _, body := auditTestRequest(t, server, http.MethodPut, "/repository/"+repo.Name+"/v1.txt", "text/plain", "经过准入的净字节", issuer)
	if status != http.StatusCreated {
		t.Fatalf("准入 PUT 应 201：%d %s", status, body)
	}
	// 候选须为 published 且带出资产号；同一主体状态查询不回 interim。
	var opID, state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT operation_id,state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&opID, &state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("净候选应提升为 published：%s", state)
	}
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/repository/"+repo.Name+"/v1.txt", "", "", "")
	if status != http.StatusOK || body != "经过准入的净字节" {
		t.Fatalf("发布后公开读取不符：%d %q", status, body)
	}
	status, _, body = auditTestRequest(t, server, http.MethodPost, "/api/v1/auth/login", "application/json",
		`{"username":"raw-owner","password":"`+auditTestPassword+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("登录失败：%d %s", status, body)
	}
	token := queryDecode[struct {
		Token string `json:"token"`
	}](t, body).Token
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/api/v1/security/admission/candidates/"+opID, "", "", token)
	if status != http.StatusOK || !strings.Contains(body, `"state":"published"`) {
		t.Fatalf("状态查询不符：%d %s", status, body)
	}
}

// TestMavenPutViaAdmissionEndToEnd Maven hosted PUT 与 Raw 共用准入管线：封存字节
// 记录仓库真实格式于清单摘要，净内容提升发布后可公开解析。
func TestMavenPutViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "maven")
	if _, _, err := app.authSvc.Bootstrap("mvn-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("mvn-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("mvn-adm-hosted", "maven", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "mvn-issuer")
	if err != nil {
		t.Fatal(err)
	}
	jar := []byte("fake-jar-bytes")
	status, _, body := auditTestRequest(t, server, http.MethodPut, "/repository/"+repo.Name+"/com/example/demo/1.0/demo-1.0.jar", "application/java-archive", string(jar), issuer)
	if status != http.StatusCreated {
		t.Fatalf("Maven 准入 PUT 应 201：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("Maven 净候选应提升为 published：%s", state)
	}
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/repository/"+repo.Name+"/com/example/demo/1.0/demo-1.0.jar", "", "", "")
	if status != http.StatusOK || body != string(jar) {
		t.Fatalf("发布后公开读取不符：%d %q", status, body)
	}
}

// TestMavenWebUploadViaAdmissionEndToEnd 管理端 Maven 表单上传的主文件经私有候选准入：
// 封存字节进入暂存批量提交，候选落 published；服务端派生文件不隔离。
func TestMavenWebUploadViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "maven")
	repo, err := app.repoSvc.Create("mvn-web-hosted", "maven", "hosted", "public", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.securityAudit.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newApplicationHandler(cfg, app, nil))
	t.Cleanup(server.Close)
	request := func(method, path, token string, body, out any) int {
		payload, _ := json.Marshal(body)
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if out != nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, out)
		}
		return resp.StatusCode
	}
	if _, _, err := app.authSvc.Bootstrap("web-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	var login api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/login", "", api.LoginRequest{Username: "web-adm", Password: auditTestPassword}, &login); code != http.StatusOK || login.Token == "" {
		t.Fatalf("登录失败：%d", code)
	}
	token := login.Token

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"groupId": "com.example", "artifactId": "app", "version": "1.0.0"} {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("file", "app.jar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("fake-jar-via-web")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/repositories/"+repo.Name+"/maven-upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("网页上传状态码 = %d，期望 201：%s", resp.StatusCode, raw)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("网页上传候选应提升为 published：%s", state)
	}
	status, _, body := auditTestRequest(t, server, http.MethodGet, "/repository/"+repo.Name+"/com/example/app/1.0.0/app-1.0.0.jar", "", "", "")
	if status != http.StatusOK || body != "fake-jar-via-web" {
		t.Fatalf("发布后公开读取不符：%d %q", status, body)
	}
}
