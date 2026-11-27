package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

type outboundProxyTestEnv struct {
	h         *Handlers
	repoRepo  *repository.RepoRepo
	proxySvc  *domain.OutboundProxyService
	auditLogs *repository.AuditLogRepo
}

func newOutboundProxyTestEnv(t *testing.T) *outboundProxyTestEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	proxyRepo := repository.NewOutboundProxyRepo(db)
	upstreamClient := upstream.NewTestClient(5 * time.Second)
	mihomoMgr := upstream.NewMihomoManager("", t.TempDir())
	proxySvc, err := domain.NewOutboundProxyService(proxyRepo, repoRepo, []byte("api-test-outbound-proxy-key-32b!"), mihomoMgr)
	if err != nil {
		t.Fatalf("NewOutboundProxyService: %v", err)
	}
	t.Cleanup(proxySvc.Close)
	upstreamClient.SetProxyResolver(proxySvc.ResolveTransport)

	repoSvc := domain.NewRepositoryService(
		repoRepo,
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
	repoSvc.SetOutboundProxyValidator(proxySvc.Exists)
	assets := domain.NewAssetService(repoRepo, repository.NewAssetRepo(db), blobstore.NewStore(t.TempDir()), upstreamClient)
	repoSvc.SetNegativeCacheInvalidator(func(repoID int64) {
		assets.InvalidateRepoNegativeCache(repoID)
	})
	auditLogs := repository.NewAuditLogRepo(db)

	h := NewHandlers(Deps{
		Repos:           repoSvc,
		Assets:          assets,
		AuditLogs:       auditLogs,
		OutboundProxies: proxySvc,
		UpstreamClient:  upstreamClient,
	})
	return &outboundProxyTestEnv{
		h:         h,
		repoRepo:  repoRepo,
		proxySvc:  proxySvc,
		auditLogs: auditLogs,
	}
}

func serveOutboundProxyRoute(h *Handlers, principal *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: h,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/outbound-proxies", siw.ListOutboundProxies)
	router.POST("/api/v1/outbound-proxies", siw.CreateOutboundProxy)
	router.PATCH("/api/v1/outbound-proxies/:id", siw.UpdateOutboundProxy)
	router.DELETE("/api/v1/outbound-proxies/:id", siw.DeleteOutboundProxy)
	router.POST("/api/v1/outbound-proxies/:id/test", siw.TestOutboundProxy)
	router.POST("/api/v1/repositories", siw.CreateRepository)
	router.PATCH("/api/v1/repositories/:name", siw.UpdateRepository)

	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestOutboundProxyEndpointsRequireAdmin(t *testing.T) {
	env := newOutboundProxyTestEnv(t)

	cases := []struct {
		name      string
		principal *auth.Principal
		method    string
		path      string
		body      any
		want      int
	}{
		{"anon list", nil, http.MethodGet, "/api/v1/outbound-proxies", nil, http.StatusUnauthorized},
		{"user list", userPrincipal(), http.MethodGet, "/api/v1/outbound-proxies", nil, http.StatusForbidden},
		{"anon create", nil, http.MethodPost, "/api/v1/outbound-proxies", map[string]any{"name": "p1", "kind": "http", "config": "http://1.2.3.4:7890"}, http.StatusUnauthorized},
		{"user create", userPrincipal(), http.MethodPost, "/api/v1/outbound-proxies", map[string]any{"name": "p1", "kind": "http", "config": "http://1.2.3.4:7890"}, http.StatusForbidden},
		{"user patch", userPrincipal(), http.MethodPatch, "/api/v1/outbound-proxies/1", map[string]any{"enabled": false}, http.StatusForbidden},
		{"user delete", userPrincipal(), http.MethodDelete, "/api/v1/outbound-proxies/1", nil, http.StatusForbidden},
		{"user test", userPrincipal(), http.MethodPost, "/api/v1/outbound-proxies/1/test", map[string]any{}, http.StatusForbidden},
	}

	for _, tc := range cases {
		rec := serveOutboundProxyRoute(env.h, tc.principal, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestOutboundProxyCRUDAndRepoBindingAndNegCacheOverride(t *testing.T) {
	env := newOutboundProxyTestEnv(t)

	// 1. Create an HTTP proxy with credentials and ensure password is redacted in response.
	secretPass := "super-secret-proxy-password-999"
	createRec := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodPost, "/api/v1/outbound-proxies", map[string]any{
		"name":        "hk-edge",
		"kind":        "http",
		"description": "HK outbound",
		"config":      "http://alice:" + secretPass + "@proxy.example.com:7890",
		"enabled":     true,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create http proxy: got %d, body=%s", createRec.Code, createRec.Body.String())
	}
	if strings.Contains(createRec.Body.String(), secretPass) {
		t.Fatalf("response must never contain secret password: %s", createRec.Body.String())
	}
	var created OutboundProxy
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.Id <= 0 || created.Name != "hk-edge" || created.Kind != Http {
		t.Fatalf("unexpected created proxy: %+v", created)
	}

	// 2. Create a Mihomo proxy from a VLESS share URI and verify UUID is redacted.
	vlessUUID := "11111111-2222-3333-4444-555555555555"
	mihomoRec := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodPost, "/api/v1/outbound-proxies", map[string]any{
		"name":   "mihomo-reality",
		"kind":   "mihomo",
		"config": "vless://" + vlessUUID + "@hk.example.com:443?security=reality&sni=www.microsoft.com&pbk=publickey#HK",
	})
	if mihomoRec.Code != http.StatusCreated {
		t.Fatalf("create mihomo proxy: got %d, body=%s", mihomoRec.Code, mihomoRec.Body.String())
	}
	if strings.Contains(mihomoRec.Body.String(), vlessUUID) {
		t.Fatalf("mihomo response must never contain UUID: %s", mihomoRec.Body.String())
	}

	// 3. Create a proxy repository bound to hk-edge with negativeCacheTtlSeconds=0.
	repoRec := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", map[string]any{
		"name":                    "mc-libraries",
		"format":                  "maven",
		"type":                    "proxy",
		"visibility":              "public",
		"remoteUrl":               "https://libraries.minecraft.net",
		"outboundProxyId":         created.Id,
		"negativeCacheTtlSeconds": 0,
	})
	if repoRec.Code != http.StatusCreated {
		t.Fatalf("create proxy repo: got %d, body=%s", repoRec.Code, repoRec.Body.String())
	}
	var repoResp Repository
	if err := json.Unmarshal(repoRec.Body.Bytes(), &repoResp); err != nil {
		t.Fatalf("unmarshal repo: %v", err)
	}
	if repoResp.OutboundProxyId == nil || *repoResp.OutboundProxyId != created.Id {
		t.Fatalf("expected outboundProxyId=%d, got %+v", created.Id, repoResp.OutboundProxyId)
	}
	if repoResp.OutboundProxyName == nil || *repoResp.OutboundProxyName != "hk-edge" {
		t.Fatalf("expected outboundProxyName=hk-edge, got %+v", repoResp.OutboundProxyName)
	}
	if repoResp.NegativeCacheTtlSeconds == nil || *repoResp.NegativeCacheTtlSeconds != 0 {
		t.Fatalf("expected negativeCacheTtlSeconds=0, got %+v", repoResp.NegativeCacheTtlSeconds)
	}

	// 4. Deleting the bound proxy must fail with 409 Conflict.
	delConflict := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodDelete, fmt.Sprintf("/api/v1/outbound-proxies/%d", created.Id), nil)
	if delConflict.Code != http.StatusConflict {
		t.Fatalf("delete bound proxy: got %d, want 409 (body=%s)", delConflict.Code, delConflict.Body.String())
	}

	// 5. Detach proxy and reset negativeCacheTtlSeconds back to inherit (-1).
	patchRepo := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/mc-libraries", map[string]any{
		"outboundProxyId":         0,
		"negativeCacheTtlSeconds": -1,
	})
	if patchRepo.Code != http.StatusOK {
		t.Fatalf("patch repo detach: got %d, body=%s", patchRepo.Code, patchRepo.Body.String())
	}
	var updatedRepo Repository
	if err := json.Unmarshal(patchRepo.Body.Bytes(), &updatedRepo); err != nil {
		t.Fatalf("unmarshal updated repo: %v", err)
	}
	if updatedRepo.OutboundProxyId != nil && *updatedRepo.OutboundProxyId != 0 {
		t.Fatalf("expected detached outboundProxyId, got %+v", updatedRepo.OutboundProxyId)
	}
	if updatedRepo.NegativeCacheTtlSeconds != nil {
		t.Fatalf("expected negativeCacheTtlSeconds nil (inherit), got %d", *updatedRepo.NegativeCacheTtlSeconds)
	}

	// 6. Now deleting the proxy succeeds with 204.
	delOK := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodDelete, fmt.Sprintf("/api/v1/outbound-proxies/%d", created.Id), nil)
	if delOK.Code != http.StatusNoContent {
		t.Fatalf("delete detached proxy: got %d, want 204 (body=%s)", delOK.Code, delOK.Body.String())
	}

	// 7. Hosted repository must reject outboundProxyId and negativeCacheTtlSeconds with 400.
	hostedBad := serveOutboundProxyRoute(env.h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", map[string]any{
		"name":                    "hosted-bad",
		"format":                  "maven",
		"type":                    "hosted",
		"visibility":              "private",
		"negativeCacheTtlSeconds": 0,
	})
	if hostedBad.Code != http.StatusBadRequest {
		t.Fatalf("hosted with negativeCacheTtlSeconds: got %d, want 400 (body=%s)", hostedBad.Code, hostedBad.Body.String())
	}
}
