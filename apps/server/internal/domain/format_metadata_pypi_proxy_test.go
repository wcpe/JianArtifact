package domain

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"testing"
)

func TestParsePyPIIndexAcceptsPEP691RelativeCandidate(t *testing.T) {
	payload := []byte("relative-pypi-package")
	digest := sha256.Sum256(payload)
	base, err := url.Parse("https://upstream.example/simple/proxy-pkg/")
	if err != nil {
		t.Fatal(err)
	}
	entries := parsePyPIIndex([]byte(`{"files":[{"filename":"relative_pkg-1.0.0.tar.gz","url":"../../packages/proxy-pkg/relative_pkg-1.0.0.tar.gz","hashes":{"sha256":"`+fmt.Sprintf("%x", digest)+`"}}]}`), base, "proxy-pkg")
	if len(entries) != 1 || entries[0].sourceURL != "https://upstream.example/packages/proxy-pkg/relative_pkg-1.0.0.tar.gz" {
		t.Fatalf("PEP 691 相对链接解析错误：%+v", entries)
	}
}
