// 开源协议清单端点：admin 专属（清单含精确依赖版本，不对匿名/普通用户暴露）。
package httpserver_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func TestLicensesEndpointAdminOnly(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)

	// 匿名 → 401。
	if rec := e.rawReq(http.MethodGet, "/api/v1/licenses", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("匿名获取协议清单状态码 = %d，期望 401", rec.Code)
	}

	// 普通用户 → 403。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "bob", Password: "bob-pass-12345"}, nil); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d", code)
	}
	var bobLogin api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "bob", Password: "bob-pass-12345"}, &bobLogin); code != http.StatusOK {
		t.Fatalf("bob 登录状态码 = %d", code)
	}
	if rec := e.rawReq(http.MethodGet, "/api/v1/licenses", "Bearer "+bobLogin.Token, "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("普通用户获取协议清单状态码 = %d，期望 403", rec.Code)
	}

	// 管理员 → 200，返回内嵌清单 JSON（含 go/npm 两段数组）。
	rec := e.rawReq(http.MethodGet, "/api/v1/licenses", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员获取协议清单状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var manifest struct {
		Go  []struct{ Name string } `json:"go"`
		Npm []struct{ Name string } `json:"npm"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("解析清单 JSON：%v", err)
	}
	if len(manifest.Go) == 0 || len(manifest.Npm) == 0 {
		t.Errorf("清单 go/npm 段不应为空：go=%d npm=%d", len(manifest.Go), len(manifest.Npm))
	}
}
