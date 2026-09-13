package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

func TestMigrationTaskAPIHidesLegacySourceConfig(t *testing.T) {
	task := &repository.MigrationTask{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: `{"url":"https://legacy.example.invalid","token":"不得回显"}`,
	}
	got := toAPIMigrationTask(task)
	if got.SourceConfig == nil {
		t.Fatal("安全 URL 应回显")
	}
	encoded, err := json.Marshal(got.SourceConfig)
	if err != nil || string(encoded) != `{"url":"https://legacy.example.invalid"}` {
		t.Fatalf("遗留在线配置回显 = %s，错误 = %v", encoded, err)
	}
}

func TestMigrationTaskAPIOnlyReturnsAllowedSourceConfig(t *testing.T) {
	task := &repository.MigrationTask{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: `{"path":"/data/bundle","password":"不得回显","unknown":true}`,
		SourceAuthType: sql.NullString{
			String: "basic",
			Valid:  true,
		},
	}
	got := toAPIMigrationTask(task)
	if got.SourceConfig == nil {
		t.Fatalf("回显 sourceConfig = %#v", got.SourceConfig)
	}
	encoded, err := json.Marshal(got.SourceConfig)
	if err != nil || string(encoded) != `{"path":"/data/bundle"}` {
		t.Fatalf("回显 sourceConfig = %s，错误 = %v", encoded, err)
	}
	if got.SourceAuthType == nil || *got.SourceAuthType != MigrationSourceAuthTypeBasic {
		t.Fatalf("回显 sourceAuthType = %#v", got.SourceAuthType)
	}
}

func TestAPIRepositoryHidesUnsafeLegacyRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		show bool
	}{
		{name: "合法地址", url: "https://packages.example.test/repository/raw", show: true},
		{name: "查询参数", url: "https://packages.example.test/repository/raw?token=secret", show: false},
		{name: "片段", url: "https://packages.example.test/repository/raw#secret", show: false},
		{name: "用户信息", url: "https://user:password@packages.example.test/repository/raw", show: false},
		{name: "非 HTTP 协议", url: "ftp://packages.example.test/repository/raw", show: false},
		{name: "缺少主机", url: "https:/repository/raw", show: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{RemoteURL: tt.url})
			if err != nil {
				t.Fatalf("编码 config：%v", err)
			}
			got := toAPIRepository(&repository.Repository{
				Name:       "legacy-proxy",
				Format:     "raw",
				Type:       "proxy",
				Visibility: "private",
				Config:     config,
			}, nil)
			if (got.RemoteUrl != nil) != tt.show {
				t.Fatalf("remoteUrl = %v，期望回显=%t", got.RemoteUrl, tt.show)
			}
		})
	}
}

func TestUpdateMigrationSourceConfigAllowsPlannedAndRejectsRunning(t *testing.T) {
	db := openAPITestDB(t)
	migrations := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	handler := NewHandlers(Deps{Migrations: migrations})

	created, err := migrations.Create(domain.MigrationCreateInput{
		SourceType:     repository.MigrationSourceOfflineDir,
		SourceConfig:   map[string]any{"path": "/data/nexus"},
		ConflictPolicy: repository.MigrationConflictSkip,
		PlanJSON:       `{"repositories":[]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}

	// planned：允许修改并持久化 allowPrivateSource
	rec := servePatchSourceConfig(handler, created.ID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("planned PATCH 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	persisted, err := migrations.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persisted.SourceConfig, `"allowPrivateSource":true`) {
		t.Fatalf("allowPrivateSource 未持久化：%s", persisted.SourceConfig)
	}

	// 终态 running：拒绝修改 → 409
	if _, err := migrations.Start(created.ID, nil); err != nil {
		t.Fatalf("Start：%v", err)
	}
	rec = servePatchSourceConfig(handler, created.ID, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("running PATCH 应返回 409，得 %d，响应 = %s", rec.Code, rec.Body.String())
	}
}

// servePatchSourceConfig 以管理员身份调用 PATCH /api/v1/migrations/{id}/source-config。
func servePatchSourceConfig(handler *Handlers, id int64, allowPrivate bool) *httptest.ResponseRecorder {
	router := gin.New()
	router.PATCH("/:id/source-config", func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		handler.UpdateMigrationSourceConfig(c, MigrationIdParam(id))
	})
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"allowPrivateSource":%t}`, allowPrivate)
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/%d/source-config", id), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

// openAPITestDB 在 api 包内打开临时 SQLite 并执行迁移。
func openAPITestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

func TestListRemoteNexusRepositoriesRejectsInvalidURL(t *testing.T) {
	factoryCalls := 0
	migrations := domain.NewMigrationService(nil, nil, func(string) (discover.Source, error) {
		factoryCalls++
		return nil, nil
	})
	handler := NewHandlers(Deps{Migrations: migrations})

	for _, body := range []string{
		`{"sourceConfig":{"url":"https://user:secret@public.example.test"}}`,
		`{"sourceConfig":{"url":"https://public.example.test?token=secret"}}`,
	} {
		router := gin.New()
		router.POST("/", func(c *gin.Context) {
			c.Set("auth.principal", &auth.Principal{Role: "admin"})
			handler.ListRemoteNexusRepositories(c)
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s 状态码 = %d，期望 400", body, rec.Code)
		}
	}
	if factoryCalls != 0 {
		t.Fatalf("非法 URL 被拒绝前不得创建来源客户端，实际 %d 次", factoryCalls)
	}
}

func TestListRemoteNexusRepositoriesListsDirectURLWithAuthentication(t *testing.T) {
	const basicPassword = "nexus-password"
	const bearerToken = "jwt-token"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/basic/service/rest/v1/repositories":
			user, password, ok := r.BasicAuth()
			if !ok || user != "nexus-user" || password != basicPassword {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "/bearer/service/rest/v1/repositories":
			if got := r.Header.Get("Authorization"); got != "Bearer "+bearerToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"raw-hosted","format":"raw","type":"hosted"}]`))
	}))
	t.Cleanup(source.Close)
	migrations := domain.NewMigrationService(nil, nil, func(string) (discover.Source, error) {
		return discover.NewOnlineREST(upstream.NewTestClient(time.Second)), nil
	})
	handler := NewHandlers(Deps{Migrations: migrations})
	router := gin.New()
	router.POST("/", func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		handler.ListRemoteNexusRepositories(c)
	})

	for _, tc := range []struct {
		name   string
		url    string
		auth   string
		secret string
	}{
		{
			name:   "Basic",
			url:    source.URL + "/basic",
			auth:   `{"type":"basic","username":"nexus-user","password":"` + basicPassword + `"}`,
			secret: basicPassword,
		},
		{
			name:   "Bearer",
			url:    source.URL + "/bearer",
			auth:   `{"type":"bearer","token":"` + bearerToken + `"}`,
			secret: bearerToken,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"sourceConfig":{"url":%q},"sourceAuth":%s}`, tc.url, tc.auth)
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), tc.secret) {
				t.Fatalf("响应不得回显认证材料：%s", rec.Body.String())
			}
			var response struct {
				Items []struct {
					Name string `json:"name"`
				} `json:"items"`
				Total int `json:"total"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("解析响应：%v", err)
			}
			if response.Total != 1 || len(response.Items) != 1 || response.Items[0].Name != "raw-hosted" {
				t.Fatalf("仓库索引响应 = %#v", response)
			}
		})
	}
}
