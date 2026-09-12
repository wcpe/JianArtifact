// 真实 npm 客户端 e2e：exec 本机 npm，覆盖「用户配置的 registry 基址」形态
// （`…/repository/<repo>/`）下的发布与全新 install 全链路。
// 此前该前缀把 npm 仓库分派到 RawHandler——publish 报成功但 `_attachments`
// 内的 tarball 静默丢失（0.8.0 验收 BUG-1），且仅打 `/npm/` 前缀的手工构造体
// 测试无法暴露。
//
// 发布以构造体直打 /repository/ 前缀（BUG-1 的直接回归断言；认证用协议
// Bearer token 直填，绕开 npm 11 对项目级 .npmrc 双斜杠 auth 键的忽略）；
// 消费端以真实 npm 客户端对公开仓库匿名 install——覆盖 packument 读取、
// dist.tarball 重写地址解析、tarball 下载与解压的完整真实链路。
package httpserver_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNpmRealClientInstallViaRepositoryPrefixPublish(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("本机未安装 npm，跳过 npm 原生客户端验收")
	}
	e := newProtocolEnv(t)
	server := httptest.NewServer(e.h)
	t.Cleanup(server.Close)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "npm-e2e", "npm", "hosted")

	pkg := "e2e-real-npm"
	version := "1.0.0"
	tarball := buildNpmTarball(t, pkg, version)
	publishBody, err := json.Marshal(map[string]any{
		"_id": pkg, "name": pkg,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{version: map[string]any{
			"name": pkg, "version": version,
			"dist": map[string]any{"tarball": fmt.Sprintf("http://placeholder.invalid/%s/-/%s-%s.tgz", pkg, pkg, version)},
		}},
		"_attachments": map[string]any{
			fmt.Sprintf("%s-%s.tgz", pkg, version): map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// BUG-1 回归点：publish 必须走用户 registry 配置的 /repository/ 前缀。
	if rec := e.rawReq(http.MethodPut, "/repository/npm-e2e/"+pkg, "Bearer "+admin, "application/json", publishBody); rec.Code != http.StatusCreated {
		t.Fatalf("publish(/repository/) 状态码=%d（体：%s）", rec.Code, rec.Body.String())
	}

	workdir := t.TempDir()
	consumer := filepath.Join(workdir, "consumer")
	if err := os.MkdirAll(consumer, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "package.json"),
		[]byte(`{"name":"e2e-real-npm-consumer","version":"1.0.0","dependencies":{"e2e-real-npm":"1.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runNpm(t, consumer, "install", "--registry", server.URL+"/repository/npm-e2e/",
		"--cache", filepath.Join(workdir, "npm-cache"), "--no-audit", "--no-fund")
	out := runNpm(t, consumer, "exec", "-c", "node -e \"console.log(require('e2e-real-npm'))\"")
	if !strings.Contains(out, "real-npm-e2e-ok") {
		t.Fatalf("安装的包内容不符：%s", out)
	}
}

// buildNpmTarball 生成合法的最小 npm tarball（gzip tar，含 package.json 与入口文件）。
func buildNpmTarball(t *testing.T, pkg, version string) []byte {
	t.Helper()
	files := map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, pkg, version),
		"package/index.js":     "module.exports = 'real-npm-e2e-ok';\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func runNpm(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("本机未安装 npm，跳过 npm 原生客户端验收")
	}
	cmd := exec.Command(npm, argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s 失败：%v\n%s", strings.Join(argv, " "), err, out)
	}
	return string(out)
}
