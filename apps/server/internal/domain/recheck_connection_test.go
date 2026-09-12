package domain_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// TestRecheckConnectionFailureToAutoBlocked 手动重测（FR-114）：上游不可达时
// 立即 HEAD 探测失败 → 进入 AUTO_BLOCKED 并携带阻止窗口；不等待自动窗口。
func TestRecheckConnectionFailureToAutoBlocked(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(time.Hour) // 窗口足够长，避免后台线程在测试期间探测
	repoID, err := repos.Create("rc-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 rc-proxy：%v", err)
	}

	// 初始 READY（尚未探测）。
	if got := svc.Status(repoID).Status; got != domain.StatusReady {
		t.Fatalf("初始应 READY，实际 %s", got)
	}

	// 重测失败 → AUTO_BLOCKED，带阻止窗口。
	got := svc.RecheckConnection(repoID, srv.URL)
	if got.Status != domain.StatusAutoBlocked {
		t.Fatalf("重测失败应 AUTO_BLOCKED，实际 %s", got.Status)
	}
	if got.BlockedUntil.IsZero() {
		t.Fatalf("AUTO_BLOCKED 应携带阻止窗口截止时间")
	}
	if !time.Now().Before(got.BlockedUntil) {
		t.Fatalf("阻止窗口应在未来，得 %v", got.BlockedUntil)
	}

	// 上游恢复后重测 → AVAILABLE，窗口清空。
	fail.Store(false)
	got2 := svc.RecheckConnection(repoID, srv.URL)
	if got2.Status != domain.StatusAvailable {
		t.Fatalf("重测成功应 AVAILABLE，实际 %s", got2.Status)
	}
	if !got2.BlockedUntil.IsZero() {
		t.Fatalf("AVAILABLE 不应携带阻止窗口，得 %v", got2.BlockedUntil)
	}
}

// TestRecheckConnectionUnavailableRepo 手动重测对不可达上游：同步探测失败进入
// AUTO_BLOCKED（验证探测路径不依赖自动窗口，能立即给出结论）。
func TestRecheckConnectionUnavailableRepo(t *testing.T) {
	svc, repos := newAssetService(t)
	repoID, err := repos.Create("fresh-proxy", "raw", "proxy", "private", proxyConfigJSON(t, "http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("建 fresh-proxy：%v", err)
	}
	if got := svc.Status(repoID).Status; got != domain.StatusReady {
		t.Fatalf("新建仓库应 READY，实际 %s", got)
	}
	// 探测不可达地址应快速失败进入 AUTO_BLOCKED（验证同步探测路径）。
	got := svc.RecheckConnection(repoID, "http://127.0.0.1:1")
	if got.Status != domain.StatusAutoBlocked {
		t.Fatalf("不可达地址重测应 AUTO_BLOCKED，实际 %s", got.Status)
	}
}

func TestRecheckConnectionUsesCredentialRef(t *testing.T) {
	const credentialRef = "HEALTHCHECK"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, "health-user:health-password")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "health-user" || password != "health-password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	repoID, err := repos.Create("credential-recheck", "raw", "proxy", "private", proxyConfigWithCredentialJSON(t, srv.URL, credentialRef))
	if err != nil {
		t.Fatalf("建带凭据的 proxy 仓库：%v", err)
	}
	if got := svc.RecheckConnection(repoID, srv.URL); got.Status == domain.StatusAutoBlocked {
		t.Fatalf("带 credentialRef 的重测不应因 401 被阻止，实际 %s", got.Status)
	}
}

// 旧库可能遗留在校验收紧前写入的配置；重测必须仍由安全出站客户端拦截。
func TestRecheckConnectionRejectsLegacyUserinfoURL(t *testing.T) {
	svc, repos := newAssetService(t)
	repoID, err := repos.Create("legacy-userinfo-proxy", "raw", "proxy", "private", `{"remoteUrl":"https://release-user:private-password@repo.example.com/raw"}`)
	if err != nil {
		t.Fatalf("写入旧 proxy 配置：%v", err)
	}
	if got := svc.RecheckConnection(repoID, "https://repo.example.com/raw"); got.Status != domain.StatusAutoBlocked {
		t.Fatalf("遗留 userinfo 地址重测应被安全阻止，实际 %s", got.Status)
	}
}
