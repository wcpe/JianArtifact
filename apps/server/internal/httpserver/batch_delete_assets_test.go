package httpserver_test

// FR-103 批量删除：handler 单测覆盖鉴权、校验、逐条尽力与审计/复制联动。
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// batchDeleteEnv 装配批量删除端点所需依赖，返回路由与数据句柄。
type batchDeleteEnv struct {
	h         http.Handler
	db        *persistence.DB
	repoRepo  *repository.RepoRepo
	assetRepo *repository.AssetRepo
	auditLogs *repository.AuditLogRepo
	blobs     *blobstore.Store
	firstHash string // 已上传制品的 blob 哈希（用于 blob 保留断言）
}

// newBatchDeleteEnv 建库并装配路由；principal 中间件按 role 注入管理员或普通用户。
func newBatchDeleteEnv(t *testing.T, role string) *batchDeleteEnv {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "batch-delete.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	userRepo := repository.NewUserRepo(db)
	settings := repository.NewSettingRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))

	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(settings), userRepo)
	repoSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	assetSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	assetSvc.SetNodeIdentity(domain.NewNodeIdentity(settings))
	auditLogs := repository.NewAuditLogRepo(db)

	handlers := api.NewHandlers(api.Deps{
		Repos:     repoSvc,
		Assets:    assetSvc,
		AuditLogs: auditLogs,
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		switch role {
		case "admin":
			c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		case "user":
			c.Set("auth.principal", &auth.Principal{Role: "user", Username: "dev", UserID: 2})
		default:
			// 不注入主体，模拟未认证。
		}
		c.Next()
	})
	r.POST("/api/v1/repositories/:name/assets/batch-delete", func(c *gin.Context) {
		handlers.BatchDeleteRepositoryAssets(c, api.RepoNameParam(c.Param("name")))
	})
	r.POST("/api/v1/repositories/:name/assets/operations", func(c *gin.Context) {
		handlers.ApplyRepositoryAssetOperation(c, api.RepoNameParam(c.Param("name")))
	})
	return &batchDeleteEnv{h: r, db: db, repoRepo: repoRepo, assetRepo: assetRepo, auditLogs: auditLogs, blobs: blobs}
}

func (e *batchDeleteEnv) postOperation(body any) (*httptest.ResponseRecorder, *api.AssetOperationResponse) {
	return e.postOperationAt("batch-repo", body)
}

func (e *batchDeleteEnv) postOperationAt(repo string, body any) (*httptest.ResponseRecorder, *api.AssetOperationResponse) {
	encoded, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/"+repo+"/assets/operations", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	e.h.ServeHTTP(rec, req)
	var out *api.AssetOperationResponse
	if rec.Code == http.StatusOK {
		out = &api.AssetOperationResponse{}
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec, out
}

func assertAssetOperationFailure(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("状态码 = %d，期望 %d（体：%s）", rec.Code, wantStatus, rec.Body.String())
	}
	var response api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析错误响应：%v（体：%s）", err, rec.Body.String())
	}
	if response.Error.Code != wantCode {
		t.Fatalf("错误码 = %q，期望 %q（体：%s）", response.Error.Code, wantCode, rec.Body.String())
	}
}

func assertFailureOperationID(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var response struct {
		OperationID string `json:"operationId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析失败 operationId：%v（体：%s）", err, rec.Body.String())
	}
	if response.OperationID == "" {
		t.Fatalf("失败响应必须携带 operationId（体：%s）", rec.Body.String())
	}
}

func (e *batchDeleteEnv) assertAssetsRemain(t *testing.T, paths ...string) {
	t.Helper()
	repo, err := e.repoRepo.GetByName("batch-repo")
	if err != nil {
		t.Fatalf("查仓库：%v", err)
	}
	for _, path := range paths {
		if _, err := e.assetRepo.GetByPath(repo.ID, path); err != nil {
			t.Fatalf("失败响应后制品 %s 不应变化：%v", path, err)
		}
	}
}

func TestAssetOperationAPIDeletesDirectoryAtomically(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)
	rec, out := e.postOperation(api.AssetOperationRequest{
		Action:         api.Delete,
		OverrideReason: "清理测试制品",
		Targets:        []api.AssetOperationTarget{{Type: api.RawPath, Path: "a"}},
	})
	if rec.Code != http.StatusOK || out == nil || out.Affected != 2 || out.OperationId == "" {
		t.Fatalf("统一操作 API 删除目录失败：状态=%d 响应=%s out=%+v", rec.Code, rec.Body.String(), out)
	}
	repo, err := e.repoRepo.GetByName("batch-repo")
	if err != nil {
		t.Fatalf("查仓库：%v", err)
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "a/1.txt"); err == nil {
		t.Fatal("目录删除后文件仍存在")
	}
}

// TestAssetOperationAPIAuditFailureRollsBackOperation 确保源端审计 SQL 失败时，
// 资产视图与 operation outbox 都随同一事务回滚。
func TestAssetOperationAPIAuditFailureRollsBackOperation(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)
	// 以 seed 完成后的水位为基线：FR-138 后 Put 自身也写 v2 outbox，基线已含若干记录。
	opRepo := repository.NewReplicationOperationRepo(e.db)
	base, err := opRepo.ListRecordsSince(0, 100)
	if err != nil {
		t.Fatalf("列 seed 后 operation outbox：%v", err)
	}
	var baseSeq int64
	if len(base) > 0 {
		baseSeq = base[len(base)-1].Seq
	}
	if _, err := e.db.Exec(`CREATE TRIGGER reject_asset_operation_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'asset.delete' BEGIN SELECT RAISE(ABORT, '注入审计失败'); END`); err != nil {
		t.Fatalf("创建审计失败触发器：%v", err)
	}
	rec, _ := e.postOperation(api.AssetOperationRequest{
		Action: api.Delete, OverrideReason: "验证审计原子性",
		Targets: []api.AssetOperationTarget{{Type: api.RawPath, Path: "a"}},
	})
	assertAssetOperationFailure(t, rec, http.StatusInternalServerError, "internal_error")
	e.assertAssetsRemain(t, "a/1.txt", "a/2.txt")
	// 审计失败必须让资产视图与 outbox 在同一事务回滚：失败操作不得产生任何新增 outbox 记录。
	records, err := opRepo.ListRecordsSince(baseSeq, 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("审计失败不得写 operation outbox：records=%+v err=%v", records, err)
	}
}

// TestAssetOperationAPIFailureResponsesLeaveAssetsUntouched 覆盖统一操作 API 的 HTTP
// 失败契约：鉴权、校验、冲突、未找到及 blob 隔离失败都不得留下部分元数据变更。
func TestAssetOperationAPIFailureResponsesLeaveAssetsUntouched(t *testing.T) {
	request := func(action api.AssetOperationRequestAction, targets []api.AssetOperationTarget) api.AssetOperationRequest {
		return api.AssetOperationRequest{
			Action:         action,
			OverrideReason: "验收失败原子性",
			Targets:        targets,
		}
	}

	t.Run("未认证返回 401 且不变更", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "none")
		e.seedBatchAssets(t)
		rec, _ := e.postOperation(request(api.Delete, []api.AssetOperationTarget{{Type: api.RawPath, Path: "a/1.txt"}}))
		assertAssetOperationFailure(t, rec, http.StatusUnauthorized, "unauthenticated")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("非管理员返回 403 且不变更", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "user")
		e.seedBatchAssets(t)
		rec, _ := e.postOperation(request(api.Delete, []api.AssetOperationTarget{{Type: api.RawPath, Path: "a/1.txt"}}))
		assertAssetOperationFailure(t, rec, http.StatusForbidden, "forbidden")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("不存在仓库返回 404", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "admin")
		e.seedBatchAssets(t)
		rec, _ := e.postOperationAt("missing", request(api.Delete, []api.AssetOperationTarget{{Type: api.RawPath, Path: "a/1.txt"}}))
		assertAssetOperationFailure(t, rec, http.StatusNotFound, "not_found")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("非法路径返回 400 且不变更", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "admin")
		e.seedBatchAssets(t)
		rec, _ := e.postOperation(request(api.Delete, []api.AssetOperationTarget{{Type: api.RawPath, Path: ""}}))
		assertAssetOperationFailure(t, rec, http.StatusBadRequest, "validation_error")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("请求体格式错误返回 400 且不变更", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "admin")
		e.seedBatchAssets(t)
		rec, _ := e.postOperation("不是操作对象")
		assertAssetOperationFailure(t, rec, http.StatusBadRequest, "bad_request")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("目标冲突返回 409 且不变更", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "admin")
		e.seedBatchAssets(t)
		destination := "a"
		rec, _ := e.postOperation(api.AssetOperationRequest{
			Action:          api.Move,
			DestinationPath: &destination,
			OverrideReason:  "验收失败原子性",
			Targets:         []api.AssetOperationTarget{{Type: api.RawPath, Path: "a/1.txt"}},
		})
		assertAssetOperationFailure(t, rec, http.StatusConflict, "conflict")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt")
	})

	t.Run("blob 隔离失败返回 500 且整批元数据回滚", func(t *testing.T) {
		e := newBatchDeleteEnv(t, "admin")
		e.seedBatchAssets(t)
		if err := e.blobs.Remove(e.firstHash); err != nil {
			t.Fatalf("模拟 blob 缺失：%v", err)
		}
		rec, _ := e.postOperation(request(api.Delete, []api.AssetOperationTarget{
			{Type: api.RawPath, Path: "a"},
			{Type: api.RawPath, Path: "b"},
		}))
		assertAssetOperationFailure(t, rec, http.StatusInternalServerError, "internal_error")
		assertFailureOperationID(t, rec)
		e.assertAssetsRemain(t, "a/1.txt", "a/2.txt", "b/3.txt")
	})
}

func TestAssetOperationAPIMavenRejectsFileAndDeepTargets(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	repoID, err := e.repoRepo.Create("maven-logical-api", "maven", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建 Maven 仓库：%v", err)
	}
	assets := domain.NewAssetService(e.repoRepo, e.assetRepo, e.blobs, nil)
	for path, body := range map[string]string{
		"com/example/demo/1.0/demo-1.0.pom":        "pom",
		"com/example/demo/1.0/demo-1.0.jar.sha256": "checksum",
		"com/example/demo/maven-metadata.xml":      `<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>`,
	} {
		if _, err := assets.Put("maven-logical-api", path, bytes.NewBufferString(body), "application/octet-stream"); err != nil {
			t.Fatalf("准备 Maven 资产 %s：%v", path, err)
		}
	}
	for _, target := range []api.AssetOperationTarget{
		{Type: api.MavenVersion, Path: "com/example/demo/1.0/demo-1.0.pom"},
		{Type: api.MavenVersion, Path: "com/example/demo/1.0/demo-1.0.jar.sha256"},
		{Type: api.MavenArtifact, Path: "com/example/demo/maven-metadata.xml"},
		{Type: api.MavenVersion, Path: "com/example/demo/1.0/nested"},
	} {
		rec, _ := e.postOperationAt("maven-logical-api", api.AssetOperationRequest{
			Action: api.Delete, OverrideReason: "验证 Maven 逻辑删除边界", Targets: []api.AssetOperationTarget{target},
		})
		assertAssetOperationFailure(t, rec, http.StatusConflict, "logical_delete_required")
		assertFailureOperationID(t, rec)
	}
	if _, err := e.assetRepo.GetByPath(repoID, "com/example/demo/1.0/demo-1.0.pom"); err != nil {
		t.Fatalf("失败请求不得删除 POM：%v", err)
	}
}

// seedBatchAssets 建仓库并写入三个制品（复用 env.blobs 以便后续断言 blob 保留）。
func (e *batchDeleteEnv) seedBatchAssets(t *testing.T) {
	t.Helper()
	aclRepo := repository.NewAclRepo(e.db)
	userRepo := repository.NewUserRepo(e.db)
	settings := repository.NewSettingRepo(e.db)
	repoSvc := domain.NewRepositoryService(e.repoRepo, aclRepo, e.assetRepo, domain.NewSettingService(settings), userRepo)
	if _, err := repoSvc.Create("batch-repo", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	assetSvc := domain.NewAssetService(e.repoRepo, e.assetRepo, e.blobs, nil)
	for _, p := range []string{"a/1.txt", "a/2.txt", "b/3.txt"} {
		asset, err := assetSvc.Put("batch-repo", p, strings.NewReader("data"), "text/plain")
		if err != nil {
			t.Fatalf("传制品 %s：%v", p, err)
		}
		if e.firstHash == "" {
			e.firstHash = asset.BlobHash
		}
	}
}

// postBatchDelete 发一次批量删除请求并返回响应码与解码后的响应体。
func (e *batchDeleteEnv) postBatchDelete(paths []string) (*httptest.ResponseRecorder, *api.BatchDeleteAssetsResponse) {
	return e.postBatchDeleteBody(map[string]any{"paths": paths, "overrideReason": "清理测试制品"})
}

func (e *batchDeleteEnv) postBatchDeleteBody(body any) (*httptest.ResponseRecorder, *api.BatchDeleteAssetsResponse) {
	encoded, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/batch-repo/assets/batch-delete", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	e.h.ServeHTTP(rec, req)
	var out *api.BatchDeleteAssetsResponse
	if rec.Code == http.StatusOK {
		out = &api.BatchDeleteAssetsResponse{}
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec, out
}

// TestBatchDeleteAssetsAdminDeletes 管理员批量删除成功：元数据删 + 审计 + 单条 v2 operation outbox。
func TestBatchDeleteAssetsAdminDeletes(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)

	// FR-138 退役复制通道后，AssetService.Put 自身也会写 v2 outbox 记录，
	// 故以 seed 完成后的水位为基线，只度量批量删除产生的增量 operation。
	opRepo := repository.NewReplicationOperationRepo(e.db)
	base, err := opRepo.ListRecordsSince(0, 100)
	if err != nil {
		t.Fatalf("列 seed 后 operation outbox：%v", err)
	}
	var baseSeq int64
	if len(base) > 0 {
		baseSeq = base[len(base)-1].Seq
	}

	rec, out := e.postBatchDelete([]string{"a/1.txt", "a/2.txt"})
	if rec.Code != http.StatusOK {
		t.Fatalf("批量删除应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	if out.Deleted != 2 || len(out.Failed) != 0 {
		t.Fatalf("应删 2 失败 0，得 %+v", out)
	}
	// 元数据已删：ListByRepo 不含已删路径。
	repoID, err := e.repoRepo.GetByName("batch-repo")
	if err != nil {
		t.Fatalf("查仓库：%v", err)
	}
	rows, err := e.assetRepo.ListByRepo(repoID.ID, "", 100, 0)
	if err != nil {
		t.Fatalf("列制品：%v", err)
	}
	for _, r := range rows {
		if r.Path == "a/1.txt" || r.Path == "a/2.txt" {
			t.Errorf("制品 %s 应已删除，得 %+v", r.Path, rows)
		}
	}

	// 审计逐条写入 asset.delete。
	entries, err := e.auditLogs.List(repository.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	var auditCount int
	for _, row := range entries {
		if row.Action == "asset.delete" && row.Result == "ok" {
			auditCount++
		}
	}
	if auditCount != 2 {
		t.Errorf("应写 2 条 asset.delete 审计，得 %d（%+v）", auditCount, entries)
	}

	// 复制操作必须作为单条 v2 envelope 发布，不能拆成逐制品 v1 tombstone。
	// 以 seed 完成后的水位为基线，只度量批量删除产生的增量 operation。
	records, err := opRepo.ListRecordsSince(baseSeq, 10)
	if err != nil {
		t.Fatalf("列增量 v2 operation outbox：%v", err)
	}
	if len(records) != 1 || records[0].Change != nil {
		t.Fatalf("批删应仅新增一条 v2 operation，不得拆成 v1 tombstone，得 %+v", records)
	}
	records, err = opRepo.ListRecordsSince(baseSeq, 10)
	if err != nil {
		t.Fatalf("读取增量 v2 operation outbox：%v", err)
	}
	if len(records) != 1 || records[0].Operation == nil || len(records[0].Operation.Items) != 2 {
		t.Fatalf("批删应发布包含 2 项的 v2 operation，得 %+v", records)
	}
	for _, item := range records[0].Operation.Items {
		if item.Type != domain.EntityAsset || item.Op != domain.OpDelete || (item.Key != domain.AssetKey("batch-repo", "a/1.txt") && item.Key != domain.AssetKey("batch-repo", "a/2.txt")) {
			t.Errorf("v2 删除项不正确：%+v", item)
		}
	}

	// blob 内容保留：删除后 blob 文件仍存在于 blobstore（元数据删不回收 blob）。
	if e.firstHash == "" {
		t.Fatal("缺少已上传制品的 blob 哈希")
	}
	if !e.blobs.Exists(e.firstHash) {
		t.Errorf("删除制品后 blob 应保留，哈希 %s 不存在", e.firstHash)
	}
}

// TestBatchDeleteAssetsPartialFailure 任一路径不存在时整批失败且不产生部分删除。
func TestBatchDeleteAssetsPartialFailure(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)

	rec, out := e.postBatchDelete([]string{"a/1.txt", "no/such.txt"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("部分失败应 404，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)

	rec, _ = e.postBatchDeleteBody("不是批删请求")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("错误请求体应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)
	if out != nil {
		t.Fatalf("失败不应返回兼容成功体：%+v", out)
	}
	repo, err := e.repoRepo.GetByName("batch-repo")
	if err != nil {
		t.Fatalf("查仓库：%v", err)
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "a/1.txt"); err != nil {
		t.Fatalf("整批失败不应删除已存在路径：%v", err)
	}
}

// TestBatchDeleteAssetsValidation 空 paths / 空路径条目 / 超上限应 400。
func TestBatchDeleteAssetsValidation(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)

	empty := []string{}
	rec, _ := e.postBatchDelete(empty)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 paths 应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)

	blank := []string{"a/1.txt", ""}
	rec, _ = e.postBatchDelete(blank)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含空路径条目应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)

	overLimit := make([]string, 501)
	for i := range overLimit {
		overLimit[i] = "x"
	}
	rec, _ = e.postBatchDelete(overLimit)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("超上限应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)
}

// TestBatchDeleteAssetsForbidden 非管理员请求批量删除应 403。
func TestBatchDeleteAssetsForbidden(t *testing.T) {
	e := newBatchDeleteEnv(t, "user")
	e.seedBatchAssets(t)

	rec, _ := e.postBatchDelete([]string{"a/1.txt"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)
}

// TestBatchDeleteAssetsUnauthenticated 未认证（无主体）应 401。
func TestBatchDeleteAssetsUnauthenticated(t *testing.T) {
	e := newBatchDeleteEnv(t, "none")
	e.seedBatchAssets(t)

	rec, _ := e.postBatchDelete([]string{"a/1.txt"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)
}

// TestBatchDeleteAssetsRepoNotFound 仓库不存在应 404（与契约及 devmock 行为一致）。
func TestBatchDeleteAssetsRepoNotFound(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")

	body, _ := json.Marshal(map[string]any{"paths": []string{"a/1.txt"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/ghost-repo/assets/batch-delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("仓库不存在应 404，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	assertFailureOperationID(t, rec)
}
