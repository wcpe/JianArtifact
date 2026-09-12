package discover_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
)

func TestSourceRefSanitizesOnlineConfig(t *testing.T) {
	const ref = "NEXUS_PRODUCTION"
	out, err := discover.SanitizeSourceConfig("online_rest", map[string]any{
		"sourceRef":           ref,
		"credential":          "不要落库",
		"sourceAuth":          "不要落库",
		"includeRepositories": []any{"raw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["sourceRef"] != ref {
		t.Fatalf("sourceRef = %v, want %s", out["sourceRef"], ref)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "不要落库") {
		t.Fatal("凭据不应进入安全配置")
	}
}

func TestResolveSourceRefFromEnvironment(t *testing.T) {
	const ref = "NEXUS_PRODUCTION"
	t.Setenv("JIAN_MIGRATION_SOURCE_"+ref, "https://nexus.example.invalid")
	got, err := discover.ResolveSourceRef(ref)
	if err != nil || got != "https://nexus.example.invalid" {
		t.Fatalf("resolved = %q, err = %v", got, err)
	}
	if _, err := discover.ResolveSourceRef("https://secret.example.invalid"); err == nil {
		t.Fatal("地址不应被当作 sourceRef")
	}
}

func TestResolveUnknownSourceRefDoesNotLeakCredential(t *testing.T) {
	secret := "迁移凭据不得泄露"
	const ref = "NEXUS_PRODUCTION"
	t.Setenv("JIAN_MIGRATION_SOURCES", `{"NEXUS_PRODUCTION":"https://nexus.example.invalid"}`)
	err := error(nil)
	_, err = discover.ResolveSourceRef(ref)
	if err == nil {
		t.Fatal("聚合环境变量不得作为 sourceRef 回退")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), ref) {
		t.Fatalf("sourceRef 错误不得泄露凭据或引用细节：%v", err)
	}
}

func TestResolveSourceRefDoesNotReadArbitraryProcessSecret(t *testing.T) {
	t.Setenv("JIAN_SYNC_TOKEN", "https://secret.example.invalid")
	_, err := discover.ResolveSourceRef("SYNC_TOKEN")
	if err == nil {
		t.Fatal("sourceRef 不得读取专用命名空间外的进程秘密")
	}
	if strings.Contains(err.Error(), "secret.example.invalid") || strings.Contains(err.Error(), "SYNC_TOKEN") {
		t.Fatalf("错误不得回显秘密或引用：%v", err)
	}
}

func TestSourceRefOnlyAcceptsUppercaseASCIIIdentifier(t *testing.T) {
	for _, ref := range []string{"NEXUS", "NEXUS_PRODUCTION_1"} {
		if !discover.IsSourceRef(ref) {
			t.Fatalf("合法 sourceRef 被拒绝：%q", ref)
		}
	}
	for _, ref := range []string{"", "nexus", "1NEXUS", "NEXUS-1", "https://nexus.example.invalid", strings.Repeat("A", 64)} {
		if discover.IsSourceRef(ref) {
			t.Fatalf("非法 sourceRef 被接受：%q", ref)
		}
	}
}

func TestOnlineSourceConfigRequiresSourceRef(t *testing.T) {
	out, err := discover.SanitizeSourceConfig("online_rest", map[string]any{
		"url": "https://nexus.example.invalid/",
	})
	if err != nil {
		t.Fatalf("online_rest 直接 URL 应接受：%v", err)
	}
	if got, want := out["url"], "https://nexus.example.invalid"; got != want {
		t.Fatalf("直接 URL = %v，期望 %q", got, want)
	}
	if _, err := discover.SanitizeSourceConfig("online_rest", map[string]any{
		"url": "https://nexus.example.invalid", "sourceRef": "NEXUS_PRODUCTION",
	}); err == nil {
		t.Fatal("online_rest 同时提供 URL 与 sourceRef 应拒绝")
	}
}
