package protocol

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

func TestValidGoProxyPath(t *testing.T) {
	valid := []string{
		"example.com/mod/@v/list",
		"example.com/mod/@v/latest",
		"example.com/mod/@v/v1.2.3.info",
		"example.com/!acme/!lib/@v/v1.0.0.zip",
	}
	for _, path := range valid {
		if !validGoProxyPath(path) {
			t.Errorf("合法 Go proxy 路径被拒绝：%s", path)
		}
	}
	invalid := []string{
		"example.com/Acme/@v/list",
		"example.com/!Acme/@v/list",
		"example.com/mod/@v/v1.2.3.exe",
		"example.com/mod/../x/@v/list",
		"example.com/mod/@v//list",
	}
	for _, path := range invalid {
		if validGoProxyPath(path) {
			t.Errorf("非法 Go proxy 路径被接受：%s", path)
		}
	}
}

func TestNormalizePyPIProject(t *testing.T) {
	if got := domain.NormalizePyPIProject(" My_Package..Name "); got != "my-package-name" {
		t.Fatalf("PyPI 项目名规范化错误：%q", got)
	}
}

func TestParseNuPkg(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	w, err := zw.Create("Demo.nuspec")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<package><metadata><id>Demo.Package</id><version>1.2.3</version></metadata></package>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	id, version, _, err := parseNuPkg(body.Bytes())
	if err != nil || id != "Demo.Package" || version != "1.2.3" {
		t.Fatalf("NuGet nuspec 解析错误：id=%q version=%q err=%v", id, version, err)
	}
}
