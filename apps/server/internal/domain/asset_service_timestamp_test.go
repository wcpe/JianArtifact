package domain_test

import (
	"bytes"
	"testing"
	"time"
)

// TestAssetServicePutWithTimestamps 在真实库验证：在线迁移保留源资产时间戳，
// asset 行 created_at/updated_at 与源端时间一致，且格式为 asset 表 datetime('now') 的
// UTC "YYYY-MM-DD HH:MM:SS"（非 RFC3339）。
func TestAssetServicePutWithTimestamps(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("maven-hosted", "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	// 源端时间取非 UTC 时区，验证写入值被规整为 UTC。
	src := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("CET", 2*3600))
	expected := src.UTC().Format("2006-01-02 15:04:05") // 应为 2020-01-02 01:04:05

	path := "com/example/lib/1.0/lib-1.0.jar"
	got, err := svc.PutWithTimestamps("maven-hosted", path, bytes.NewReader([]byte("artifact-bytes")), "application/java-archive", src)
	if err != nil {
		t.Fatalf("PutWithTimestamps：%v", err)
	}
	if got.CreatedAt != expected || got.UpdatedAt != expected {
		t.Fatalf("返回资产时间戳不符：created_at=%q updated_at=%q want %q", got.CreatedAt, got.UpdatedAt, expected)
	}

	// 直接读库核对 asset 行（GetByPath 同样返回写回的时间戳）。
	asset, rc, err := svc.Get("maven-hosted", path)
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	_ = rc.Close()
	if asset.CreatedAt != expected {
		t.Fatalf("asset 行 created_at 不符：got %q want %q", asset.CreatedAt, expected)
	}
	if asset.UpdatedAt != expected {
		t.Fatalf("asset 行 updated_at 不符：got %q want %q", asset.UpdatedAt, expected)
	}

	// 零值时间戳回退到 Put 语义：时间使用本地当前值（非空、格式相同），而非源端固定值。
	_, err = svc.PutWithTimestamps("maven-hosted", "com/example/lib/2.0/lib-2.0.jar",
		bytes.NewReader([]byte("more")), "application/java-archive", time.Time{})
	if err != nil {
		t.Fatalf("PutWithTimestamps(零值)：%v", err)
	}
	a2, rc2, err := svc.Get("maven-hosted", "com/example/lib/2.0/lib-2.0.jar")
	if err != nil {
		t.Fatalf("Get(零值)：%v", err)
	}
	_ = rc2.Close()
	if a2.CreatedAt == "" || a2.UpdatedAt == "" {
		t.Fatalf("零值时时间戳不应为空：created_at=%q updated_at=%q", a2.CreatedAt, a2.UpdatedAt)
	}
	if a2.CreatedAt == expected {
		t.Fatalf("零值不应回写源端固定时间 %q，应回退本地时间", expected)
	}
}
