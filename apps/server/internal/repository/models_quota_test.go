package repository_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRepositoryConfigStorageQuotaRoundTrip 仓库存储配额配置（FR-41）的解析与序列化：
// 显式值可往返；缺省/空配置解析为零值（0 = 不限），序列化时省略不写。
func TestRepositoryConfigStorageQuotaRoundTrip(t *testing.T) {
	repo := &repository.Repository{Config: `{"quotaBytes":1024,"quotaAssets":5}`}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		t.Fatalf("解析配置：%v", err)
	}
	if cfg.QuotaBytes != 1024 || cfg.QuotaAssets != 5 {
		t.Fatalf("配额字段解析错误：%+v", cfg)
	}

	for _, raw := range []string{"", "{}", `{"remoteUrl":"https://example.com"}`} {
		empty := &repository.Repository{Config: raw}
		parsed, err := empty.DecodeConfig()
		if err != nil {
			t.Fatalf("解析配置 %q：%v", raw, err)
		}
		if parsed.QuotaBytes != 0 || parsed.QuotaAssets != 0 {
			t.Fatalf("缺省配额必须为零值（0 = 不限），得 %+v", parsed)
		}
	}

	encoded, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{QuotaBytes: 2048, QuotaAssets: 3})
	if err != nil {
		t.Fatalf("序列化配置：%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("反序列化：%v", err)
	}
	if decoded["quotaBytes"] != float64(2048) || decoded["quotaAssets"] != float64(3) {
		t.Fatalf("序列化结果缺少配额字段：%s", encoded)
	}

	// 零值省略：旧行不写新键，保持既有 config 文本兼容。
	zeroEncoded, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("序列化零值配置：%v", err)
	}
	if strings.Contains(zeroEncoded, "quota") {
		t.Fatalf("零值配额应省略，得 %s", zeroEncoded)
	}
}
