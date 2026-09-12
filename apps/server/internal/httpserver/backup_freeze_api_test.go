package httpserver_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// TestFreezeEndpointsRequireAdmin 覆盖三条冻结端点的鉴权：未带令牌一律 401。
func TestFreezeEndpointsRequireAdmin(t *testing.T) {
	env := newBackupEnv(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/maintenance/freeze"},
		{http.MethodPost, "/api/v1/maintenance/freeze"},
		{http.MethodDelete, "/api/v1/maintenance/freeze"},
	}
	for _, c := range cases {
		if code, _ := env.request(t, c.method, c.path, "", nil); code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未认证应 401，实际 %d", c.method, c.path, code)
		}
	}
}

// TestFreezeWithTTL 覆盖「带 ttlSeconds 冻结」：200、frozen=true、
// until ≈ now+ttl、frozenAt 非空。
func TestFreezeWithTTL(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 7200, "reason": "relocation"})
	if code != http.StatusOK {
		t.Fatalf("冻结：status=%d body=%s", code, body)
	}
	var st api.WriteFreezeState
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解析冻结响应：%v", err)
	}
	if !st.Frozen {
		t.Fatal("冻结后 frozen 应为 true")
	}
	if st.FrozenAt == nil || st.FrozenAt.IsZero() {
		t.Fatal("冻结后 frozenAt 应非空")
	}
	if st.Until == nil {
		t.Fatal("带 ttl 冻结后 until 应非空")
	}
	// until 应约等于 now+7200s（留处理容差）。
	delta := st.Until.UTC().Sub(time.Now().UTC())
	if delta < 7180*time.Second || delta > 7220*time.Second {
		t.Fatalf("until 偏差过大：距现在 %s，期望约 7200s", delta)
	}
}

// TestFreezeUntilBounds 覆盖接口层窗口策略：until 超界（>now+24h 或已过去）一律 400。
func TestFreezeUntilBounds(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 超过 now+24h → 400。
	far := time.Now().UTC().Add(25 * time.Hour)
	if code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"until": far}); code != http.StatusBadRequest {
		t.Fatalf("until 超界应 400，实际 %d body=%s", code, body)
	}
	// 已过去的 until → 400。
	past := time.Now().UTC().Add(-time.Hour)
	if code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"until": past}); code != http.StatusBadRequest {
		t.Fatalf("until 已过去应 400，实际 %d body=%s", code, body)
	}
}

// TestFreezeTTLClamp 覆盖 ttlSeconds 的钳制边界：低于下限被钳到 60s，超过上限被钳到 86400s，
// 且不报 400。
func TestFreezeTTLClamp(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 10 < 下限 60 → 钳到 60s。
	code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 10})
	if code != http.StatusOK {
		t.Fatalf("ttl=10 应 200（钳制），实际 %d body=%s", code, body)
	}
	var low api.WriteFreezeState
	_ = json.Unmarshal(body, &low)
	if low.Until == nil {
		t.Fatal("ttl=10 钳制后 until 应非空")
	}
	if d := low.Until.UTC().Sub(time.Now().UTC()); d < 55*time.Second || d > 65*time.Second {
		t.Fatalf("ttl=10 应被钳到 60s，实际距现在 %s", d)
	}

	// 999999 > 上限 86400 → 钳到 86400s（=now+24h，恰好不触发超界 400）。
	code, _ = env.request(t, http.MethodDelete, "/api/v1/maintenance/freeze", token, nil)
	if code != http.StatusOK {
		t.Fatalf("解冻：status=%d", code)
	}
	code, body = env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 999999})
	if code != http.StatusOK {
		t.Fatalf("ttl=999999 应 200（钳制），实际 %d body=%s", code, body)
	}
	var high api.WriteFreezeState
	_ = json.Unmarshal(body, &high)
	if high.Until == nil {
		t.Fatal("ttl=999999 钳制后 until 应非空")
	}
	if d := high.Until.UTC().Sub(time.Now().UTC()); d < 86395*time.Second || d > 86405*time.Second {
		t.Fatalf("ttl=999999 应被钳到 86400s，实际距现在 %s", d)
	}
}

// TestFreezeGetReflectsState 覆盖 GET 反映当前状态，且**未冻结**时响应 JSON
// 不应出现 until / frozenAt / reason 键（直接解析成 map[string]any 断言键缺失）。
func TestFreezeGetReflectsState(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 未冻结：仅应有 frozen 键。
	code, body := env.request(t, http.MethodGet, "/api/v1/maintenance/freeze", token, nil)
	if code != http.StatusOK {
		t.Fatalf("查询冻结状态：status=%d body=%s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析为 map：%v", err)
	}
	if v, _ := raw["frozen"].(bool); v {
		t.Fatal("初始状态 frozen 应为 false")
	}
	for _, k := range []string{"until", "frozenAt", "reason"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("未冻结时响应不应含键 %q：%s", k, body)
		}
	}

	// 冻结后再查：until / frozenAt 必须出现。
	if code, body = env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 3600}); code != http.StatusOK {
		t.Fatalf("冻结：status=%d body=%s", code, body)
	}
	if code, body = env.request(t, http.MethodGet, "/api/v1/maintenance/freeze", token, nil); code != http.StatusOK {
		t.Fatalf("再查冻结状态：status=%d body=%s", code, body)
	}
	raw = map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析为 map：%v", err)
	}
	if v, _ := raw["frozen"].(bool); !v {
		t.Fatal("冻结后 frozen 应为 true")
	}
	if _, ok := raw["until"]; !ok {
		t.Fatalf("冻结后响应应含 until 键：%s", body)
	}
	if _, ok := raw["frozenAt"]; !ok {
		t.Fatalf("冻结后响应应含 frozenAt 键：%s", body)
	}
}

// TestFreezeUnfreezeIdempotent 覆盖解冻端点：一次 200；再解一次仍 200 且 frozen=false。
func TestFreezeUnfreezeIdempotent(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	if code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 3600}); code != http.StatusOK {
		t.Fatalf("冻结：status=%d body=%s", code, body)
	}
	code, body := env.request(t, http.MethodDelete, "/api/v1/maintenance/freeze", token, nil)
	if code != http.StatusOK {
		t.Fatalf("解冻：status=%d body=%s", code, body)
	}
	var st api.WriteFreezeState
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解析解冻响应：%v", err)
	}
	if st.Frozen {
		t.Fatal("已解冻，frozen 应为 false")
	}
	// 幂等：再解一次仍 200 且 frozen=false。
	code, body = env.request(t, http.MethodDelete, "/api/v1/maintenance/freeze", token, nil)
	if code != http.StatusOK {
		t.Fatalf("再次解冻应 200，实际 %d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解析二次解冻响应：%v", err)
	}
	if st.Frozen {
		t.Fatal("幂等解冻后 frozen 应为 false")
	}
}

// TestFreezeBlocksBusinessWrites 覆盖关键回归：冻结生效后业务写被 503 + write_frozen 拦下，
// 而读请求不受影响（非 503）。
func TestFreezeBlocksBusinessWrites(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	if code, body := env.request(t, http.MethodPost, "/api/v1/maintenance/freeze", token,
		map[string]any{"ttlSeconds": 3600, "reason": "relocation"}); code != http.StatusOK {
		t.Fatalf("冻结：status=%d body=%s", code, body)
	}

	// 业务写被拦：503 + write_frozen（中间件在鉴权前拦截，故无需令牌）。
	code, body := env.request(t, http.MethodPost, "/api/v1/users", "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("冻结中 POST /users 应 503，实际 %d body=%s", code, body)
	}
	var errResp api.Error
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("解析错误响应：%v", err)
	}
	if errResp.Error.Code != "write_frozen" {
		t.Fatalf("错误码=%q，期望 write_frozen", errResp.Error.Code)
	}

	// 读不受影响：不应是 503（用管理员读取 /backups 列表，验证冻结中间件只拦写不拦读）。
	if code, body := env.request(t, http.MethodGet, "/api/v1/backups", token, nil); code == http.StatusServiceUnavailable {
		t.Fatalf("冻结中读请求被错误拦成 503：%s", body)
	}
}
