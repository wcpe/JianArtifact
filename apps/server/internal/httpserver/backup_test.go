package httpserver_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

type backupEnv struct {
	h   http.Handler
	svc *domain.BackupService
	db  *persistence.DB
	dir string
	// blobs 与 svc 内部使用的是同一个根目录，seed 时必须写进同一个存储。
	blobs *blobstore.Store
	// freeze 与 backupImports 为 FR-135 / FR-137 的handler级测试补充依赖；
	// 同一实例同时注入 Deps 与 WithWriteFreeze，确保「冻上就拦截写」与「handler 读状态」一致。
	freeze        *domain.FreezeController
	backupImports *domain.BackupImportService
	backupUploads *domain.BackupUploadService
}

// newBackupEnv 装配只包含备份所需依赖的完整服务端（真实持久化 + 鉴权中间件）。
func newBackupEnv(t *testing.T) *backupEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jianartifact.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}

	blobs := blobstore.NewStore(filepath.Join(dir, "blobs"))
	backupSvc := domain.NewBackupService(db, repository.NewBackupPackageRepo(db), blobs, dir, dbPath, "test", func() string { return "np-test" })

	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	revokedRepo := repository.NewRevokedRepo(db)
	jwtMgr := auth.NewJWTManager([]byte("backup-integration-test-secret-32b!!"))
	authenticator := auth.NewAuthenticator(jwtMgr, domain.NewAuthStore(userRepo, tokenRepo, revokedRepo))

	// FR-135：冻结控制器（base=nil，单节点兼容）。FR-137：导入服务。
	// 用 upstream.NewTestClient 放行回环，使 URL 拉取测试可指向本地 httptest 服务。
	freeze := domain.NewFreezeController(nil)
	restoreSvc := domain.NewRestoreService(db, dir, dbPath, filepath.Join(dir, "restore-blobs"))
	importSvc := domain.NewBackupImportService(
		repository.NewBackupImportRepo(db), restoreSvc, dir, upstream.NewTestClient(0),
	)
	uploadSvc := domain.NewBackupUploadService(repository.NewBackupUploadRepo(db), importSvc, dir)

	handlers := api.NewHandlers(api.Deps{
		Version:       "test",
		Checks:        []func() error{db.Ping},
		Migration:     db.CurrentVersion,
		Auth:          domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Settings:      domain.NewSettingService(repository.NewSettingRepo(db)),
		Backups:       backupSvc,
		BackupLinkKey: []byte("backup-integration-test-link-key"),
		Freeze:        freeze,
		BackupImports: importSvc,
		BackupUploads: uploadSvc,
	})
	srv := httpserver.New("test",
		httpserver.WithReadinessCheck(db.Ping),
		httpserver.WithHandlers(handlers),
		httpserver.WithWriteFreeze(freeze.State),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
	)
	return &backupEnv{h: srv.Handler(nil), svc: backupSvc, db: db, dir: dir, blobs: blobs, freeze: freeze, backupImports: importSvc, backupUploads: uploadSvc}
}

func (e *backupEnv) seedAsset(t *testing.T, content string) {
	t.Helper()
	res, err := e.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	hash, _, _, size, err := e.blobs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	if _, err := e.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size) VALUES (?, 'a.bin', ?, ?)`,
		repoID, hash, size); err != nil {
		t.Fatalf("插入资产：%v", err)
	}
}

// request 发起一次请求并返回状态码与响应体。
func (e *backupEnv) request(t *testing.T, method, target, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (e *backupEnv) adminToken(t *testing.T) string {
	t.Helper()
	code, body := e.request(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		map[string]string{"username": "admin", "password": "backup-integration-pass1"})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("自举管理员失败：status=%d body=%s", code, body)
	}
	var out api.LoginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析登录响应：%v", err)
	}
	if out.Token == "" {
		t.Fatal("自举未返回令牌")
	}
	return out.Token
}

func (e *backupEnv) waitDone(t *testing.T, token, packageID string) api.BackupPackage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := e.request(t, http.MethodGet, "/api/v1/backups/"+packageID, token, nil)
		if code != http.StatusOK {
			t.Fatalf("查询备份详情：status=%d body=%s", code, body)
		}
		var rec api.BackupPackage
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Fatalf("解析备份详情：%v", err)
		}
		switch string(rec.Status) {
		case "done":
			return rec
		case "failed":
			t.Fatalf("备份生成失败：%s", deref(t, rec.ErrorSummary))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待备份生成超时")
	return api.BackupPackage{}
}

func deref(t *testing.T, s *string) string {
	t.Helper()
	if s == nil {
		return ""
	}
	return *s
}

// TestBackupEndToEndCreateLinkDownload 覆盖搬迁主链路：
// 生成 → 列表 → 签名链接 → 无会话下载 → 校验 → 删除。
func TestBackupEndToEndCreateLinkDownload(t *testing.T) {
	env := newBackupEnv(t)
	env.seedAsset(t, "payload-content")
	token := env.adminToken(t)

	// 未认证不得访问。
	if code, _ := env.request(t, http.MethodGet, "/api/v1/backups", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("未认证列表应 401，实际 %d", code)
	}

	code, body := env.request(t, http.MethodPost, "/api/v1/backups", token, map[string]any{"mode": "hot", "label": "上线前"})
	if code != http.StatusCreated {
		t.Fatalf("创建备份：status=%d body=%s", code, body)
	}
	var created api.BackupPackage
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("解析创建响应：%v", err)
	}
	if created.PackageId == "" {
		t.Fatal("创建响应缺少 packageId")
	}

	done := env.waitDone(t, token, created.PackageId)
	if done.SizeBytes <= 0 {
		t.Fatalf("包体大小 = %d，期望 > 0", done.SizeBytes)
	}
	if done.Counts == nil || done.Counts.Assets == nil || *done.Counts.Assets != 1 {
		t.Fatalf("计数未回显：%+v", done.Counts)
	}

	// 列表应能看到。
	code, body = env.request(t, http.MethodGet, "/api/v1/backups?page=1&page_size=10", token, nil)
	if code != http.StatusOK {
		t.Fatalf("列表：status=%d", code)
	}
	var list api.BackupPackageList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("解析列表：%v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("列表内容有误：total=%d len=%d", list.Total, len(list.Items))
	}

	// 生成签名链接。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/"+created.PackageId+"/link", token, nil)
	if code != http.StatusOK {
		t.Fatalf("生成链接：status=%d body=%s", code, body)
	}
	var link api.BackupLink
	if err := json.Unmarshal(body, &link); err != nil {
		t.Fatalf("解析链接：%v", err)
	}
	parsed, err := url.Parse(link.Url)
	if err != nil {
		t.Fatalf("链接不可解析：%v", err)
	}
	target := parsed.RequestURI()

	// 关键：不带任何会话头，凭签名链接直接下载（模拟新机器）。
	code, raw := env.request(t, http.MethodGet, target, "", nil)
	if code != http.StatusOK {
		t.Fatalf("签名链接下载：status=%d body=%s", code, raw)
	}
	// tar.gz 魔数。
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("下载内容不是 gzip：前两字节 % x", raw[:min(2, len(raw))])
	}

	// 篡改令牌必须被拒。
	bad := strings.Replace(target, "token=", "token=x", 1)
	if code, _ := env.request(t, http.MethodGet, bad, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("篡改令牌应 401，实际 %d", code)
	}
	// 无令牌且无会话也必须被拒。
	noToken := fmt.Sprintf("/api/v1/backups/%s/download", created.PackageId)
	if code, _ := env.request(t, http.MethodGet, noToken, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("无令牌下载应 401，实际 %d", code)
	}

	// 校验完整性。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/"+created.PackageId+"/verify?deep=true", token, nil)
	if code != http.StatusOK {
		t.Fatalf("校验：status=%d body=%s", code, body)
	}
	var verification api.BackupVerification
	if err := json.Unmarshal(body, &verification); err != nil {
		t.Fatalf("解析校验结果：%v", err)
	}
	if !verification.Ok {
		t.Fatalf("校验未通过：%+v", verification)
	}

	// 删除后不可再查。
	if code, _ := env.request(t, http.MethodDelete, "/api/v1/backups/"+created.PackageId, token, nil); code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d", code)
	}
	if code, _ := env.request(t, http.MethodGet, "/api/v1/backups/"+created.PackageId, token, nil); code != http.StatusNotFound {
		t.Fatalf("删除后应 404，实际 %d", code)
	}
}

// TestBackupVerifyReportsCorruptionAs422 覆盖契约形状：包存在但内容校验未通过时，
// 返回 422 + BackupVerification（业务结论），而不是泛化成 500。
func TestBackupVerifyReportsCorruptionAs422(t *testing.T) {
	env := newBackupEnv(t)
	env.seedAsset(t, "payload-to-corrupt")
	token := env.adminToken(t)

	code, body := env.request(t, http.MethodPost, "/api/v1/backups", token, map[string]any{"mode": "hot"})
	if code != http.StatusCreated {
		t.Fatalf("创建备份：status=%d body=%s", code, body)
	}
	var created api.BackupPackage
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("解析创建响应：%v", err)
	}
	env.waitDone(t, token, created.PackageId)

	// 翻转包体中段一个字节，制造"包在但内容不合法"。
	path := env.svc.PackagePath(created.PackageId)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取包体：%v", err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写回包体：%v", err)
	}

	code, body = env.request(t, http.MethodPost, "/api/v1/backups/"+created.PackageId+"/verify", token, nil)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("内容损坏应 422，实际 %d body=%s", code, body)
	}
	var verification api.BackupVerification
	if err := json.Unmarshal(body, &verification); err != nil {
		t.Fatalf("解析 422 响应：%v（原文 %s）", err, body)
	}
	if verification.Ok || verification.Error == nil || *verification.Error == "" {
		t.Fatalf("422 应带失败原因：%+v", verification)
	}
}

// TestBackupVerifyUnknownPackageIs404 验证"包不存在"仍走通用错误映射（不是 422）。
func TestBackupVerifyUnknownPackageIs404(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, _ := env.request(t, http.MethodPost, "/api/v1/backups/bk-nope/verify", token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("不存在的包应 404，实际 %d", code)
	}
}

func TestBackupCreateRejectsUnknownMode(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	code, body := env.request(t, http.MethodPost, "/api/v1/backups", token, map[string]any{"mode": "nope"})
	if code != http.StatusBadRequest {
		t.Fatalf("未知模式应 400，实际 %d body=%s", code, body)
	}
}

func TestBackupLinkRequiresAdmin(t *testing.T) {
	env := newBackupEnv(t)
	if code, _ := env.request(t, http.MethodPost, "/api/v1/backups/bk-x/link", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("未认证生成链接应 401，实际 %d", code)
	}
}
