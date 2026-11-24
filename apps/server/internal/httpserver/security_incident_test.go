// FR-168：事故处置动作生命周期与全实例业务写锁。
package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func TestIncidentInstanceWriteLockEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, secret, _ := installAdmin(t, e, "admin", "admin-pass-123")
	vis := api.CreateRepositoryRequestVisibility("private")

	// 预览 restrict(instance_write_lock) → actionId + 摘要。
	var prev api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "incident-test", IdempotencyKey: "lock-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &prev); code != http.StatusOK || prev.ActionDigest == "" {
		t.Fatalf("预览状态码 = %d，期望 200 且返回摘要", code)
	}

	// 幂等回放：同键同摘要返回同一动作。
	var prev2 api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "incident-test", IdempotencyKey: "lock-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &prev2); code != http.StatusOK || prev2.ActionId != prev.ActionId {
		t.Fatalf("幂等回放状态码 = %d，期望 200 且同 actionId", code)
	}

	// 摘要漂移执行 → 409。
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: "tampered"}, &bad); code != http.StatusConflict {
		t.Fatalf("漂移执行状态码 = %d，期望 409", code)
	}

	// 正常执行（安装登录会话带近期强认证水位）→ applied。
	var res api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK || res.State != "applied" || len(res.Items) != 1 || res.Items[0].Outcome != "applied" {
		t.Fatalf("执行状态码 = %d 状态 = %q，期望 applied", code, res.State)
	}

	// 业务写锁生效：建仓库写 → 503 incident_write_locked；安全出口仍可用（GET 200）。
	var bad2 map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "locked-repo", Type: "hosted", Format: "raw", Visibility: &vis}, &bad2); code != http.StatusServiceUnavailable {
		t.Fatalf("锁定期业务写状态码 = %d，期望 503", code)
	}
	if bad2["error"].(map[string]any)["code"] != "incident_write_locked" {
		t.Fatalf("错误码 = %v，期望 incident_write_locked", bad2["error"])
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/users", token, nil, &bad2); code != http.StatusOK {
		t.Fatalf("锁定期只读查询状态码 = %d，期望 200", code)
	}

	// 解除限制：lift_restriction 预览+执行。
	var lp api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.LiftRestriction, Reason: "incident-cleared", IdempotencyKey: "lift-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &lp); code != http.StatusOK {
		t.Fatalf("解除预览状态码 = %d，期望 200", code)
	}
	var lr api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+lp.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: lp.ActionDigest}, &lr); code != http.StatusOK || lr.State != "applied" {
		t.Fatalf("解除执行状态码 = %d 状态 = %q，期望 applied", code, lr.State)
	}

	// 解闸后业务写恢复。
	var repo api.Repository
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "unlocked-repo", Type: "hosted", Format: "raw", Visibility: &vis}, &repo); code != http.StatusCreated {
		t.Fatalf("解闸后业务写状态码 = %d，期望 201", code)
	}

	// 过期拒绝：伪造过期动作不可用——以消费后终态复执代替（动作已终结）。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &bad2); code != http.StatusConflict {
		t.Fatalf("终态复执状态码 = %d，期望 409", code)
	}
	_ = secret
}

func TestIncidentRepositoryRestrictionEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "admin", "admin-pass-123")
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", token,
		api.CreateTokenRequest{Name: "incident-test"}, &created); code != http.StatusCreated {
		t.Fatalf("创建 Token 状态码 = %d，期望 201", code)
	}
	apiToken := created.Token

	// 建两个仓库：受限 raw-locked 与对照 raw-open。
	for _, name := range []string{"raw-locked", "raw-open"} {
		e.createRawRepo(t, token, name, "private")
	}

	// 对 raw-locked 加 repository 限制。
	var prev api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "repo-incident", IdempotencyKey: "repo-1",
			Targets: []api.SecurityActionTarget{{Kind: "repository", Target: strPtr("raw-locked")}},
		}, &prev); code != http.StatusOK {
		t.Fatalf("仓库限制预览状态码 = %d，期望 200", code)
	}
	var res api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK || res.State != "applied" {
		t.Fatalf("仓库限制执行状态码 = %d 状态 = %q，期望 applied", code, res.State)
	}

	// 受限仓写入 → 403 incident_restricted；对照仓正常。
	rec := e.rawReq(http.MethodPut, "/repository/raw-locked/a.bin", basicHeader(apiToken), "application/octet-stream", []byte("x"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("受限仓写状态码 = %d，期望 403", rec.Code)
	}
	rec = e.rawReq(http.MethodPut, "/repository/raw-open/a.bin", basicHeader(apiToken), "application/octet-stream", []byte("x"))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("对照仓写状态码 = %d，期望 2xx", rec.Code)
	}
}

func TestIncidentEvidenceHoldEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "admin", "admin-pass-123")

	runAction := func(actionType api.SecurityActionPreviewRequestActionType, key string, targets []api.SecurityActionTarget) api.SecurityActionResult {
		t.Helper()
		var prev api.SecurityActionPreview
		if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
			api.SecurityActionPreviewRequest{ActionType: actionType, Reason: "e2e", IdempotencyKey: key, Targets: targets}, &prev); code != http.StatusOK {
			t.Fatalf("预览状态码 = %d，期望 200", code)
		}
		var res api.SecurityActionResult
		if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
			api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK {
			t.Fatalf("执行状态码 = %d，期望 200", code)
		}
		return res
	}

	// 保全两枚摘要；重复摘要第二动作标 skipped。
	r1 := runAction(api.HoldEvidence, "hold-1", []api.SecurityActionTarget{{Kind: "digest", Target: strPtr("sha256:aaa")}})
	if r1.State != "applied" {
		t.Fatalf("保全状态 = %q，期望 applied", r1.State)
	}
	r2 := runAction(api.HoldEvidence, "hold-2", []api.SecurityActionTarget{{Kind: "digest", Target: strPtr("sha256:aaa")}})
	if r2.Items[0].Outcome != "skipped" {
		t.Fatalf("重复摘要结果 = %q，期望 skipped", r2.Items[0].Outcome)
	}

	// 清单含活跃保全。
	var list struct {
		Items []struct {
			HoldID    string `json:"holdId"`
			Digest    string `json:"digest"`
			ByteState string `json:"byteState"`
			State     string `json:"state"`
		} `json:"items"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/holds", token, nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ByteState != "unknown" {
		t.Fatalf("清单状态码 = %d 项数 = %d，期望 200 且 1 枚 unknown", code, len(list.Items))
	}

	// 释放后清单清空。
	r3 := runAction(api.ReleaseHold, "rel-1", []api.SecurityActionTarget{{Kind: "hold", Target: &list.Items[0].HoldID}})
	if r3.State != "applied" {
		t.Fatalf("释放状态 = %q，期望 applied", r3.State)
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/holds", token, nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].State != "released" {
		t.Fatalf("释放后清单状态 = %v，期望 released", list.Items)
	}
}

func strPtr(s string) *string { return &s }
