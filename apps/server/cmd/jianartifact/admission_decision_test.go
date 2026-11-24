package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

// TestCargoPublishViaAdmissionEndToEnd Cargo hosted 发布的 crate 字节经私有候选准入：
// 封存→裁定→提升后 crate 可下载、候选落 published。
func TestCargoPublishViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "cargo")
	if _, _, err := app.authSvc.Bootstrap("cargo-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("cargo-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("cargo-adm-hosted", "cargo", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "cargo-issuer")
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"name": "probe-crate", "vers": "1.0.0", "deps": []any{}, "features": map[string]any{}})
	crate := []byte("fake-crate-bytes")
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.LittleEndian, uint32(len(metadata)))
	_, _ = frame.Write(metadata)
	_ = binary.Write(&frame, binary.LittleEndian, uint32(len(crate)))
	_, _ = frame.Write(crate)
	status, _, body := auditTestRequest(t, server, http.MethodPut, "/cargo/"+repo.Name+"/api/v1/crates/new", "application/octet-stream", frame.String(), issuer)
	if status != http.StatusOK {
		t.Fatalf("Cargo 准入发布应 200：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("Cargo 净候选应提升为 published：%s", state)
	}
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/cargo/"+repo.Name+"/api/v1/crates/probe-crate/1.0.0/download", "", "", "")
	if status != http.StatusOK || body != string(crate) {
		t.Fatalf("发布后 crate 读取不符：%d %q", status, body)
	}
}

// TestNpmPublishViaAdmissionEndToEnd npm hosted 发布的每个 tarball 附件各自经私有候选准入：
// 封存→裁定→提升后 tarball 与 packument 可公开读取、候选落 published。
func TestNpmPublishViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "npm")
	if _, _, err := app.authSvc.Bootstrap("npm-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("npm-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("npm-adm-hosted", "npm", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "npm-issuer")
	if err != nil {
		t.Fatal(err)
	}
	tarball := []byte("fake-tarball-bytes")
	doc := map[string]any{
		"_id": "probe-pkg", "name": "probe-pkg", "dist-tags": map[string]any{"latest": "1.0.0"},
		"versions": map[string]any{"1.0.0": map[string]any{"name": "probe-pkg", "version": "1.0.0",
			"dist": map[string]any{"tarball": "http://x.invalid/probe-pkg/-/probe-pkg-1.0.0.tgz"}}},
		"_attachments": map[string]any{"probe-pkg-1.0.0.tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(tarball),
			"length":       len(tarball)}},
	}
	raw, _ := json.Marshal(doc)
	status, _, body := auditTestRequest(t, server, http.MethodPut, "/npm/"+repo.Name+"/probe-pkg", "application/json", string(raw), issuer)
	if status != http.StatusCreated {
		t.Fatalf("npm 准入发布应 201：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("npm tarball 候选应提升为 published：%s", state)
	}
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/npm/"+repo.Name+"/probe-pkg/-/probe-pkg-1.0.0.tgz", "", "", "")
	if status != http.StatusOK || body != string(tarball) {
		t.Fatalf("发布后 tarball 读取不符：%d %q", status, body)
	}
}

// TestOCIPublishViaAdmissionEndToEnd OCI hosted 的 blob/monolithic PUT 与 manifest PUT
// 均经私有候选准入：封存→裁定→提升后可公开解析、候选落 published。
func TestOCIPublishViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "docker")
	if _, _, err := app.authSvc.Bootstrap("oci-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("oci-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("oci-adm-hosted", "docker", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "oci-issuer")
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("fake-oci-blob")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(blob))
	status, _, body := auditTestRequest(t, server, http.MethodPut,
		"/v2/"+repo.Name+"/demo/blobs/uploads/?digest="+digest, "application/octet-stream", string(blob), issuer)
	if status != http.StatusCreated {
		t.Fatalf("OCI blob 准入发布应 201：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("OCI blob 候选应提升为 published：%s", state)
	}
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"mediaType":"application/octet-stream","digest":"` + digest + `","size":` + fmt.Sprint(len(blob)) + `}]}`
	status, _, body = auditTestRequest(t, server, http.MethodPut,
		"/v2/"+repo.Name+"/demo/manifests/v1", "application/vnd.oci.image.manifest.v1+json", manifest, issuer)
	if status != http.StatusCreated {
		t.Fatalf("OCI manifest 准入发布应 201：%d %s", status, body)
	}
	var n int
	if err := app.db.Get(&n, `SELECT count(*) FROM private_admission_candidate WHERE state='published'`); err != nil || n < 2 {
		t.Fatalf("manifest 与 blob 候选均应 published：n=%d err=%v", n, err)
	}
	status, _, body = auditTestRequest(t, server, http.MethodGet, "/v2/"+repo.Name+"/demo/blobs/"+digest, "", "", "")
	if status != http.StatusOK || body != string(blob) {
		t.Fatalf("发布后 blob 读取不符：%d %q", status, body)
	}
}

// TestNuGetPushViaAdmissionEndToEnd nupkg 推包经私有候选准入：封存→裁定→提升→published。
func TestNuGetPushViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "nuget")
	if _, _, err := app.authSvc.Bootstrap("nu-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("nu-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("nu-adm-hosted", "nuget", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "nu-issuer")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("package", "probe.1.0.0.nupkg")
	if err != nil {
		t.Fatal(err)
	}
	pkg := testNuGetPackageBytes(t)
	if _, err := part.Write(pkg); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	status, _, body := auditTestRequest(t, server, http.MethodPut, "/nuget/"+repo.Name+"/api/v2/package", w.FormDataContentType(), buf.String(), issuer)
	if status != http.StatusCreated {
		t.Fatalf("NuGet 推包准入应 201：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("NuGet 候选应提升为 published：%s", state)
	}
}

// testNuGetPackageBytes 生成最小合法 nupkg（含单个 nuspec 的 zip）。
func testNuGetPackageBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("probe.nuspec")
	if err != nil {
		t.Fatal(err)
	}
	spec := `<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata><id>probe</id><version>1.0.0</version><description>p</description><authors>a</authors></metadata></package>`
	if _, err := f.Write([]byte(spec)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestPyPIUploadViaAdmissionEndToEnd Twine 上传经私有候选准入：封存→裁定→提升→published。
func TestPyPIUploadViaAdmissionEndToEnd(t *testing.T) {
	cfg, app := newSourceTestApplication(t, "pypi")
	if _, _, err := app.authSvc.Bootstrap("py-adm", auditTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := app.userSvc.Create("py-owner", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := app.repoSvc.Create("py-adm-hosted", "pypi", "hosted", "public", "", repository.RepositoryConfig{})
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
	issuer, _, err := app.tokenSvc.Create(owner.ID, "py-issuer")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"name": "probe", "version": "1.0.0", "filetype": "sdist", "pyversion": "source"} {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := w.CreateFormFile("content", "probe-1.0.0.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("fake-sdist-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	status, _, body := auditTestRequest(t, server, http.MethodPost, "/pypi/"+repo.Name+"/legacy/", w.FormDataContentType(), buf.String(), issuer)
	if status != http.StatusOK {
		t.Fatalf("PyPI 上传准入应 200：%d %s", status, body)
	}
	var state string
	var assetID *int64
	if err := app.db.QueryRow(`SELECT state,published_asset_id FROM private_admission_candidate ORDER BY rowid DESC LIMIT 1`).Scan(&state, &assetID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || assetID == nil {
		t.Fatalf("PyPI 候选应提升为 published：%s", state)
	}
}
