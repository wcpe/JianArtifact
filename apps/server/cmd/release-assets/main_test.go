package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseTargetsIncludeArm64AndStableNames(t *testing.T) {
	seen := map[string]bool{}
	for _, target := range releaseTargets {
		name := "jianartifact-0.8.0-" + target.goos + "-" + target.goarch
		if target.goos == "windows" {
			name += ".exe"
		}
		if seen[name] {
			t.Fatalf("重复发布目标：%s", name)
		}
		seen[name] = true
	}
	if !seen["jianartifact-0.8.0-linux-arm64"] {
		t.Fatal("发布目标缺少 linux arm64")
	}
}

func TestWriteChecksumsCoversOnlyManifestAssets(t *testing.T) {
	dir := t.TempDir()
	names := []string{"jianartifact-0.8.0-linux-arm64", "jianartifact-0.8.0-windows-amd64.exe"}
	for i, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", i+1)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeChecksums(dir, names); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, name := range names {
		if !strings.Contains(text, "  "+name+"\n") {
			t.Errorf("校验和缺少 %s：%q", name, text)
		}
	}
}
