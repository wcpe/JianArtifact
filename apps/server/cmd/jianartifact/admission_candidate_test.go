package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type admissionReader func([]byte) (int, error)

func (r admissionReader) Read(p []byte) (int, error) { return r(p) }

func TestAdmissionCandidateApplicationRestart(t *testing.T) {
	cfg, first := newSourceTestApplication(t, "raw")
	const ownerName = "candidate-owner"
	const adminName = "candidate-admin"
	_, admin, err := first.authSvc.Bootstrap(adminName, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := first.userSvc.Create(ownerName, auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := first.repoSvc.Create("candidate-raw", "raw", "hosted", "public", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	acl := repository.NewAclRepo(first.db)
	if err := acl.Replace(repo.ID, []repository.Acl{{SubjectID: owner.ID, Action: repository.ActionPublish}}); err != nil {
		t.Fatal(err)
	}
	issuer, _, err := first.tokenSvc.Create(admin.ID, "candidate-publisher")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.securityAudit.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	http1 := httptest.NewServer(newApplicationHandler(cfg, first, nil))
	t.Cleanup(http1.Close)
	login := func(server *httptest.Server, name string) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodPost, "/api/v1/auth/login", "application/json", fmt.Sprintf(`{"username":%q,"password":%q}`, name, auditTestPassword), "")
		if status != http.StatusOK {
			t.Fatalf("真实登录失败：%d %s", status, body)
		}
		return queryDecode[api.LoginResponse](t, body).Token
	}
	token := login(http1, ownerName)
	const unique = "仅私有文件"
	const shared = "既有同摘要字节"
	oldPath := "/repository/" + repo.Name + "/old.txt"
	status, _, body := auditTestRequest(t, http1, http.MethodPut, oldPath, "text/plain", shared, issuer)
	if status != http.StatusCreated {
		t.Fatalf("原生 Raw PUT 应发布并返回 201：%d %s", status, body)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/internal-candidate-test", nil)
	req.RemoteAddr = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+token)
	principal, err := auth.NewAuthenticator(first.jwt, first.store).AdmissionPrincipal(req)
	if err != nil {
		t.Fatal(err)
	}
	source := cfg.SourcePolicy.Resolve(req, rand.Text())
	receiveCtx := auditctx.WithSource(t.Context(), source)
	prepare := func(path string, body io.Reader, want error) domain.AdmissionStatus {
		t.Helper()
		var op string
		got, err := first.admission.Prepare(receiveCtx, principal, repo.ID, path, "text/plain", body, func(id string) { op = id })
		if op == "" || got.OperationID != op || !errors.Is(err, want) {
			t.Fatalf("登记与接收结果不匹配：登记 %v, %v", op != "", err)
		}
		return got
	}
	t.Run("RejectUnsupportedPathBeforeRegistration", func(t *testing.T) {
		var gotRead, notified bool
		got, err := first.admission.Prepare(receiveCtx, principal, repo.ID, "notes#draft.txt", "text/plain", admissionReader(func(p []byte) (int, error) {
			gotRead = true
			return copy(p, "普通草稿"), io.EOF
		}), func(string) { notified = true })
		var intents, events int
		if countErr := first.db.QueryRow(`SELECT (SELECT COUNT(*) FROM private_admission_candidate),
			(SELECT COUNT(*) FROM security_event WHERE event_type = 'admission.candidate_staged')`).Scan(&intents, &events); countErr != nil {
			t.Fatal(countErr)
		}
		entries, dirErr := os.ReadDir(filepath.Join(cfg.BlobDir, "tmp", "admission-candidates", "registered"))
		if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
			t.Fatal(dirErr)
		}
		if !errors.Is(err, domain.ErrValidation) || got.OperationID != "" || gotRead || notified || intents != 0 || events != 0 || len(entries) != 0 {
			t.Errorf("不合规格路径须在登记及 I/O 前拒绝：错误 %v，读流 %v，回执 %v，意图/事件/文件 %d/%d/%d", err, gotRead, notified, intents, events, len(entries))
		}
	})
	staged := prepare("private.txt", strings.NewReader(unique), nil)
	same := prepare("old.txt", strings.NewReader(shared), nil)
	broken := prepare("broken.txt", strings.NewReader("待检测损坏"), nil)
	corrupt := prepare("corrupt.txt", strings.NewReader("旧"), nil)
	injected := errors.New("测试中断")
	interrupted := prepare("interrupted.txt", admissionReader(func([]byte) (int, error) { return 0, injected }), injected)
	if _, err := first.db.Exec(`CREATE TRIGGER test_admission_no_event BEFORE INSERT ON security_event WHEN NEW.event_type='admission.candidate_staged' BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	blocked := prepare("no-audit.txt", strings.NewReader("封存事务失败对照"), domain.ErrAdmissionUnavailable)
	if _, err := first.db.Exec(`DROP TRIGGER test_admission_no_event`); err != nil {
		t.Fatal(err)
	}
	frozen := prepare("frozen.txt", admissionReader(func(p []byte) (int, error) {
		first.freeze.Freeze(time.Time{}, "本机冻结对照")
		return copy(p, "私有接收完成前冻结"), io.EOF
	}), domain.ErrWriteFrozen)
	first.freeze.Unfreeze()
	keyFor := func(op string) string {
		t.Helper()
		var key string
		if err := first.db.Get(&key, `SELECT object_key FROM private_admission_candidate WHERE operation_id=?`, op); err != nil {
			t.Fatal(err)
		}
		return key
	}
	privateDir := filepath.Join(cfg.BlobDir, "tmp", "admission-candidates", "registered")
	interruptedFile := filepath.Join(privateDir, keyFor(interrupted.OperationID))
	if err := os.WriteFile(interruptedFile, []byte("接收中断后保留的文件"), 0600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(privateDir, "87654321876543218765432187654321")
	if err := os.WriteFile(orphan, []byte("无主文件"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(privateDir, keyFor(broken.OperationID))); err != nil {
		t.Fatal(err)
	}
	corruptFile := filepath.Join(privateDir, keyFor(corrupt.OperationID))
	if err := os.WriteFile(corruptFile, []byte("新"), 0600); err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := first.db.Get(&failures, `SELECT count(*) FROM security_event WHERE event_type='admission.candidate_staged' AND operation_id IN (?,?,?)`, blocked.OperationID, interrupted.OperationID, frozen.OperationID); err != nil || failures != 0 {
		t.Fatalf("未封存出现成功审计：%d %v", failures, err)
	}
	t.Run("RevalidateLoggedOutSessionBeforeLookup", func(t *testing.T) {
		adminReq := req.Clone(req.Context())
		adminReq.Header.Set("Authorization", "Bearer "+login(http1, adminName))
		adminPrincipal, err := auth.NewAuthenticator(first.jwt, first.store).AdmissionPrincipal(adminReq)
		if err != nil {
			t.Fatal(err)
		}
		adminCtx := auditctx.WithSource(t.Context(), cfg.SourcePolicy.Resolve(adminReq, rand.Text()))
		other, err := first.admission.Prepare(adminCtx, adminPrincipal, repo.ID, "other.txt", "text/plain", strings.NewReader("另一账号的普通候选"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.admission.Status(t.Context(), principal, staged.OperationID); err != nil {
			t.Fatalf("登出前合法本人状态：%v", err)
		}
		ids := []struct{ name, id string }{
			{"格式不符", "not-an-operation-id"},
			{"未登记", uuid.NewString()},
			{"他人", other.OperationID},
			{"本人", staged.OperationID},
		}
		for _, tc := range ids[:3] {
			if _, err := first.admission.Status(t.Context(), principal, tc.id); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("有效会话的%s目标应统一不可枚举：%v", tc.name, err)
			}
		}
		if status, _, _ := auditTestRequest(t, http1, http.MethodPost, "/api/v1/auth/logout", "", "", token); status != http.StatusNoContent {
			t.Fatalf("正常会话登出失败：%d", status)
		}
		// 以登录后已交给领域调用方的同一主体验证事务内凭据复验，而非再经过 HTTP 中间件。
		for _, tc := range ids {
			got, err := first.admission.Status(t.Context(), principal, tc.id)
			if !errors.Is(err, domain.ErrAdmissionUnauthenticated) || got.OperationID != "" {
				t.Errorf("登出后%s请求必须先失效且不返回状态：%v", tc.name, err)
			}
		}
	})
	http1.Close()
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.securityAudit.Close(stop); err != nil {
		t.Fatal(err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatal(err)
	}

	next, err := openServices(cfg)
	if err == nil {
		next.authSvc.SetMfaService(nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.db.Close() })
	http2 := httptest.NewServer(newApplicationHandler(cfg, next, nil))
	t.Cleanup(http2.Close)
	token = login(http2, ownerName)
	other := login(http2, adminName)
	get := func(op, credential string, want int, code string) api.PrivateAdmissionCandidateStatus {
		t.Helper()
		status, _, body := auditTestRequest(t, http2, http.MethodGet, "/api/v1/security/admission/candidates/"+op, "", "", credential)
		if status != want {
			t.Fatalf("候选 GET HTTP %d，期望 %d：%s", status, want, body)
		}
		if code != "" {
			result := queryDecode[api.Error](t, body)
			if result.Error.Code != code || result.OperationId != nil {
				t.Fatalf("错误不符合契约：%s", body)
			}
			return api.PrivateAdmissionCandidateStatus{}
		}
		for _, field := range []string{`"objectKey"`, `"canonicalPath"`, `"actorUserId"`, `"downloadUrl"`, `"clientIp"`, `"accountEpoch"`, unique, shared} {
			if strings.Contains(body, field) {
				t.Fatal("状态泄露内部字段或内容")
			}
		}
		return queryDecode[api.PrivateAdmissionCandidateStatus](t, body)
	}
	get(staged.OperationID, "", http.StatusUnauthorized, "unauthenticated")
	get(staged.OperationID, issuer, http.StatusForbidden, "admission_forbidden")
	get(staged.OperationID, other, http.StatusNotFound, "admission_not_found")
	get("invalid", token, http.StatusNotFound, "admission_not_found")
	check := get(staged.OperationID, token, http.StatusOK, "")
	if check.OperationId != staged.OperationID || check.RepositoryId != repo.ID || check.State != "staged" || check.Coverage != "unknown" || check.ManifestDigest == nil || *check.ManifestDigest != *staged.ManifestDigest || check.ManifestVersion == nil || *check.ManifestVersion != 1 {
		t.Fatalf("重启后不一致：%+v", check)
	}
	if get(same.OperationID, token, http.StatusOK, "").State != "staged" {
		t.Fatal("同摘要私有对象也应独立封存")
	}
	// 目录暂不可判定不可被写成永久损坏，修复本机测试目录后原状态可读取。
	offline := privateDir + "-test-offline"
	if err := os.Rename(privateDir, offline); err != nil {
		t.Fatal(err)
	}
	get(staged.OperationID, token, http.StatusServiceUnavailable, "admission_unavailable")
	if err := os.Rename(offline, privateDir); err != nil {
		t.Fatal(err)
	}
	if get(staged.OperationID, token, http.StatusOK, "").State != "staged" {
		t.Fatal("暂时 I/O 被误判为硬失败")
	}
	for _, op := range []string{broken.OperationID, corrupt.OperationID} {
		bad := get(op, token, http.StatusOK, "")
		reread := get(op, token, http.StatusOK, "")
		if bad.State != "rejected" || bad.ErrorCode == nil || *bad.ErrorCode != "admission_integrity_failed" || bad.TargetVersion != reread.TargetVersion {
			t.Fatalf("硬完整性失败未持久化或不幂等：%+v", bad)
		}
	}
	for _, op := range []string{interrupted.OperationID, blocked.OperationID, frozen.OperationID} {
		bad := get(op, token, http.StatusOK, "")
		if bad.State != "rejected" || bad.ManifestDigest != nil || bad.ErrorCode != nil {
			t.Fatalf("仅意图不应视为封存或硬完整性失败：%+v", bad)
		}
	}
	for _, p := range []string{orphan, interruptedFile, corruptFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("对账不能删证据：%v", err)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, err := http.NewRequest(method, http2.URL+"/repository/"+repo.Name+"/private.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-1")
		response, err := http2.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("私有路径对外可见：%s %d", method, response.StatusCode)
		}
	}
	store := blobstore.NewStore(cfg.BlobDir)
	if rc, _, err := store.Open(fmt.Sprintf("%x", sha256.Sum256([]byte(unique)))); !errors.Is(err, os.ErrNotExist) {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatalf("活动摘要读应失败：%v", err)
	}
	status, _, body = auditTestRequest(t, http2, http.MethodGet, oldPath, "", "", "")
	if status != http.StatusOK || body != shared {
		t.Fatal("同摘要现有合法公开引用未保持可读")
	}
	if err := repository.NewAclRepo(next.db).Replace(repo.ID, nil); err != nil {
		t.Fatal(err)
	}
	get(staged.OperationID, token, http.StatusNotFound, "admission_not_found")
	if err := next.db.Close(); err != nil {
		t.Fatal(err)
	}
	get(staged.OperationID, token, http.StatusServiceUnavailable, "admission_unavailable")
}
