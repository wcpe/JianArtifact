package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 writeDomainErr 兜底分支的**对外暴露口径**。
//
// 该函数不是管理端点专用的：/api/v1/search、/api/v1/search/suggest、
// /api/v1/public/repositories、/api/v1/repositories 都走可选鉴权、匿名可达，
// 它们的失败路径同样落进兜底分支。一旦无差别回显 err.Error()，驱动原文
// （库名、表名、SQL 片段）就泄漏给匿名用户。故这里同时钉住两个方向：
//
//   - 非管理员（含匿名）：只有固定文案，响应体不得出现任何内部细节；
//   - 管理员：仍能看到原始错误（自助排查能力不能被这次收紧一并削掉）；
//   - 原始错误不得凭空消失：它必须落在服务端日志里，否则「查不出原因」的老问题会回来。

// driverDetailText 是一段形如 SQLite 驱动原文的错误文本：带表名与库文件路径。
// 断言「响应体不含它」才有意义——真实泄漏正是这种形态。
const driverDetailText = `no such table: acl (db=/var/lib/jianartifact/jianartifact.db)`

// domainErrResponse 是 auth.WriteError 的响应信封（带操作 ID 的变体多一个键）。
type domainErrResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	OperationID string `json:"operationId"`
}

// callDomainErrWriter 在最小 gin 上下文里调用写错误函数；withOperationID 为真时走
// 带 operationId 的变体（资产操作执行阶段的响应形状，与兜底分支共用同一段逻辑）。
func callDomainErrWriter(t *testing.T, principal *auth.Principal, err error, withOperationID bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) { c.Set("auth.principal", principal) })
	}
	router.GET("/err", func(c *gin.Context) {
		if withOperationID {
			writeDomainErrWithOperationID(c, err, "op-test-1")
			return
		}
		writeDomainErr(c, err)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/err", nil))
	return rec
}

// captureLogs 把标准日志输出改写到内存缓冲，测试结束恢复。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestWriteDomainErrHidesDetailFromNonAdmin 逐主体形态检查兜底分支的响应体与日志。
func TestWriteDomainErrHidesDetailFromNonAdmin(t *testing.T) {
	logs := captureLogs(t)

	cases := []struct {
		name      string
		principal *auth.Principal
		isAdmin   bool
	}{
		{name: "匿名", principal: nil},
		{name: "非管理员", principal: &auth.Principal{UserID: 2, Role: "user", Kind: auth.KindSession}},
		{name: "管理员", principal: &auth.Principal{UserID: 1, Role: "admin", Kind: auth.KindSession}, isAdmin: true},
	}
	for _, tc := range cases {
		for _, withOperationID := range []bool{false, true} {
			name := tc.name + "/无操作 ID"
			if withOperationID {
				name = tc.name + "/带操作 ID"
			}
			t.Run(name, func(t *testing.T) {
				logs.Reset()
				rec := callDomainErrWriter(t, tc.principal, errors.New(driverDetailText), withOperationID)
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("兜底分支应返回 500，得 %d：%s", rec.Code, rec.Body.String())
				}
				var body domainErrResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("解析响应失败：%v（体：%s）", err, rec.Body.String())
				}
				if body.Error.Code != "internal_error" {
					t.Fatalf("错误码 = %q，期望 internal_error", body.Error.Code)
				}
				if withOperationID && body.OperationID != "op-test-1" {
					t.Fatalf("operationId = %q，期望 op-test-1", body.OperationID)
				}
				// 无论主体是谁，原始错误都必须留在服务端日志里可查。
				if !strings.Contains(logs.String(), driverDetailText) {
					t.Fatalf("原始错误必须写进服务端日志，实际日志 = %q", logs.String())
				}
				if tc.isAdmin {
					if !strings.Contains(body.Error.Message, driverDetailText) {
						t.Fatalf("管理员应看到原始错误，实际 message = %q", body.Error.Message)
					}
					return
				}
				if body.Error.Message != "内部错误" {
					t.Fatalf("非管理员只应看到固定文案「内部错误」，实际 message = %q", body.Error.Message)
				}
				for _, leak := range []string{driverDetailText, "no such table", "jianartifact.db"} {
					if strings.Contains(rec.Body.String(), leak) {
						t.Fatalf("非管理员响应体泄漏了内部细节 %q：%s", leak, rec.Body.String())
					}
				}
			})
		}
	}
}

// TestAnonymousRepositoryListHidesInternalDetail 是端到端的可达路径验证：
// 仓库列表是可选鉴权、匿名可达的端点，其兜底失败（这里是匿名开关读取失败，
// 与批量 ACL 查询失败同属 writeDomainErr 的 default 分支）不得把驱动原文回显给匿名方。
func TestAnonymousRepositoryListHidesInternalDetail(t *testing.T) {
	logs := captureLogs(t)
	gin.SetMode(gin.ReleaseMode)

	db := openAPITestDB(t)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	repoSvc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db),
		repository.NewAssetRepo(db), settings, repository.NewUserRepo(db))
	handlers := NewHandlers(Deps{Repos: repoSvc, Settings: settings})
	// 关掉数据库：此后任一语句都会失败，且失败原因带驱动原文（sql: database is closed）。
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库：%v", err)
	}

	list := func(principal *auth.Principal) *httptest.ResponseRecorder {
		router := gin.New()
		if principal != nil {
			router.Use(func(c *gin.Context) { c.Set("auth.principal", principal) })
		}
		siw := ServerInterfaceWrapper{Handler: handlers}
		router.GET("/api/v1/repositories", siw.ListRepositories)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repositories?page_size=10", nil))
		return rec
	}

	// 匿名：固定文案，不含任何驱动细节。
	logs.Reset()
	rec := list(nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("匿名请求应得到 500，得 %d：%s", rec.Code, rec.Body.String())
	}
	var body domainErrResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v（体：%s）", err, rec.Body.String())
	}
	if body.Error.Message != "内部错误" {
		t.Fatalf("匿名响应体应只有固定文案「内部错误」，实际 = %q", body.Error.Message)
	}
	for _, leak := range []string{"database is closed", "sql:", "no such"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("匿名响应体泄漏了内部细节 %q：%s", leak, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "database is closed") {
		t.Fatalf("原始错误必须落日志，实际日志 = %q", logs.String())
	}

	// 管理员：同一条失败路径，仍能看到原始错误（管理面排查能力不受影响）。
	rec = list(&auth.Principal{UserID: 1, Role: "admin", Kind: auth.KindSession})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("管理员请求应得到 500，得 %d：%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v（体：%s）", err, rec.Body.String())
	}
	if !strings.Contains(body.Error.Message, "database is closed") {
		t.Fatalf("管理员应看到原始错误，实际 message = %q", body.Error.Message)
	}
}
