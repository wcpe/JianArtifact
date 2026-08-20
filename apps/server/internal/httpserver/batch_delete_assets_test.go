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
	h           http.Handler
	db          *persistence.DB
	repoRepo    *repository.RepoRepo
	assetRepo   *repository.AssetRepo
	auditLogs   *repository.AuditLogRepo
	replChanges *repository.ReplChangeRepo
	blobs       *blobstore.Store
	firstHash   string // 已上传制品的 blob 哈希（用于 blob 保留断言）
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
	tokenRepo := repository.NewTokenRepo(db)
	settings := repository.NewSettingRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))

	replSvc := domain.NewReplicationService(repository.NewReplChangeRepo(db), assetRepo, repoRepo,
		aclRepo, userRepo, tokenRepo, settings, blobs)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(settings), userRepo)
	repoSvc.SetChangeRecorder(replSvc)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	assetSvc.SetChangeRecorder(replSvc)
	auditLogs := repository.NewAuditLogRepo(db)
	replChanges := repository.NewReplChangeRepo(db)

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
	return &batchDeleteEnv{h: r, db: db, repoRepo: repoRepo, assetRepo: assetRepo, auditLogs: auditLogs, replChanges: replChanges, blobs: blobs}
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
	body, _ := json.Marshal(map[string]any{"paths": paths})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/batch-repo/assets/batch-delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.h.ServeHTTP(rec, req)
	var out *api.BatchDeleteAssetsResponse
	if rec.Code == http.StatusOK {
		out = &api.BatchDeleteAssetsResponse{}
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec, out
}

// TestBatchDeleteAssetsAdminDeletes 管理员批量删除成功：元数据删 + 审计 + 复制 tombstone 逐条写入。
func TestBatchDeleteAssetsAdminDeletes(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)

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

	// 复制 tombstone 逐条写入（asset delete 变更）。
	changes, err := e.replChanges.ListSince(0, 0)
	if err != nil {
		t.Fatalf("列复制变更：%v", err)
	}
	var tombCount int
	for _, c := range changes {
		if c.EntityType == domain.EntityAsset && c.Op == domain.OpDelete {
			tombCount++
		}
	}
	if tombCount != 2 {
		t.Errorf("应写 2 条 asset 复制删除变更，得 %d（%+v）", tombCount, changes)
	}

	// blob 内容保留：删除后 blob 文件仍存在于 blobstore（元数据删不回收 blob）。
	if e.firstHash == "" {
		t.Fatal("缺少已上传制品的 blob 哈希")
	}
	if !e.blobs.Exists(e.firstHash) {
		t.Errorf("删除制品后 blob 应保留，哈希 %s 不存在", e.firstHash)
	}
}

// TestBatchDeleteAssetsPartialFailure 部分失败：已存在路径删除成功，不存在路径进 failed 明细。
func TestBatchDeleteAssetsPartialFailure(t *testing.T) {
	e := newBatchDeleteEnv(t, "admin")
	e.seedBatchAssets(t)

	rec, out := e.postBatchDelete([]string{"a/1.txt", "no/such.txt"})
	if rec.Code != http.StatusOK {
		t.Fatalf("部分失败应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	if out.Deleted != 1 || len(out.Failed) != 1 {
		t.Fatalf("应删 1 失败 1，得 %+v", out)
	}
	if out.Failed[0].Path != "no/such.txt" || out.Failed[0].Error == "" {
		t.Errorf("failed 明细应含路径与原因，得 %+v", out.Failed)
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

	blank := []string{"a/1.txt", ""}
	rec, _ = e.postBatchDelete(blank)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含空路径条目应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}

	overLimit := make([]string, 501)
	for i := range overLimit {
		overLimit[i] = "x"
	}
	rec, _ = e.postBatchDelete(overLimit)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("超上限应 400，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
}

// TestBatchDeleteAssetsForbidden 非管理员请求批量删除应 403。
func TestBatchDeleteAssetsForbidden(t *testing.T) {
	e := newBatchDeleteEnv(t, "user")
	e.seedBatchAssets(t)

	rec, _ := e.postBatchDelete([]string{"a/1.txt"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
}

// TestBatchDeleteAssetsUnauthenticated 未认证（无主体）应 401。
func TestBatchDeleteAssetsUnauthenticated(t *testing.T) {
	e := newBatchDeleteEnv(t, "none")
	e.seedBatchAssets(t)

	rec, _ := e.postBatchDelete([]string{"a/1.txt"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
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
}
