package httpserver_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// tokenReq 与 jsonReq 同形但返回完整 recorder，用于断言响应头。
func tokenReq(t *testing.T, e *protocolEnv, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestLegacyTokenIssuanceCarriesDeprecationWarning(t *testing.T) {
	t.Parallel()
	e := newProtocolEnv(t)
	sessionJWT, _ := bootstrapJWT(t, e)

	// 未限用途签发（purpose 缺省 = legacy）：响应须带标准 Warning 头，含弃用时间盒日期。
	rec := tokenReq(t, e, http.MethodPost, "/api/v1/tokens", sessionJWT, api.CreateTokenRequest{Name: "legacy-warn"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("legacy 签发状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}
	warning := rec.Header().Get("Warning")
	if !strings.HasPrefix(warning, "299 ") || !strings.Contains(warning, "2027-03-01") {
		t.Fatalf("legacy 签发缺弃用 Warning 头或时间盒日期：%q", warning)
	}
}

func TestScopedTokenIssuanceHasNoDeprecationWarning(t *testing.T) {
	t.Parallel()
	e := newProtocolEnv(t)
	sessionJWT, adminToken := bootstrapJWT(t, e)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	purpose := api.CreateTokenRequestPurpose("artifact_read")
	scope := api.MachineTokenScope{Repository: "raw-hosted", Actions: []api.MachineTokenScopeActions{"read"}}
	rec := tokenReq(t, e, http.MethodPost, "/api/v1/tokens", sessionJWT, api.CreateTokenRequest{
		Name: "scoped-no-warn", Purpose: &purpose, Scopes: &[]api.MachineTokenScope{scope},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("受限签发状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}
	if warning := rec.Header().Get("Warning"); warning != "" {
		t.Fatalf("受限签发不应带 Warning 头：%q", warning)
	}
}
