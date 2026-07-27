package httpserver_test

// FR-73: Maven 网页上传端点测试。
// POST /api/v1/repositories/:name/maven-upload（multipart：GAV + packaging + file）
// 服务端自动生成 pom.xml、各文件 .md5/.sha1、更新 artifact 级 maven-metadata.xml 及其校验和。
import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// mavenUploadResp 是上传成功的响应体。
type mavenUploadResp struct {
	Repository string   `json:"repository"`
	GroupID    string   `json:"groupId"`
	ArtifactID string   `json:"artifactId"`
	Version    string   `json:"version"`
	Files      []string `json:"files"`
}

// mavenUpload 发起 multipart 上传请求；fileBytes 为 nil 时不带 file 字段。
func (e *protocolEnv) mavenUpload(t *testing.T, token, repo string, fields map[string]string, filename string, fileBytes []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("写 multipart 字段 %s：%v", k, err)
		}
	}
	if fileBytes != nil {
		fw, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("建 multipart 文件：%v", err)
		}
		if _, err := fw.Write(fileBytes); err != nil {
			t.Fatalf("写 multipart 文件：%v", err)
		}
	}
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories/"+repo+"/maven-upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// getBody 读取仓库内制品内容；期望 200。
func (e *protocolEnv) getBody(t *testing.T, token, repo, path string) string {
	t.Helper()
	rec := e.rawReq(http.MethodGet, "/repository/"+repo+"/"+path, "Bearer "+token, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d，期望 200（体：%s）", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestMavenWebUploadCreatesFullSet(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-web", "hosted", "", nil)

	jar := []byte("fake-jar-for-web-upload")
	rec := e.mavenUpload(t, adminToken, "mvn-web", map[string]string{
		"groupId": "com.example", "artifactId": "app", "version": "1.0.0",
	}, "app.jar", jar)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}
	var resp mavenUploadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(resp.Files) != 9 {
		t.Errorf("files 数 = %d，期望 9（jar/pom/metadata 各 + .md5/.sha1）：%v", len(resp.Files), resp.Files)
	}

	// 主文件字节一致。
	if got := e.getBody(t, adminToken, "mvn-web", "com/example/app/1.0.0/app-1.0.0.jar"); got != string(jar) {
		t.Errorf("jar 内容不一致：%q", got)
	}

	// jar 校验和文件内容 = 实算值。
	md5sum := md5.Sum(jar)
	sha1sum := sha1.Sum(jar)
	if got := e.getBody(t, adminToken, "mvn-web", "com/example/app/1.0.0/app-1.0.0.jar.md5"); got != hex.EncodeToString(md5sum[:]) {
		t.Errorf("jar.md5 = %q，期望 %q", got, hex.EncodeToString(md5sum[:]))
	}
	if got := e.getBody(t, adminToken, "mvn-web", "com/example/app/1.0.0/app-1.0.0.jar.sha1"); got != hex.EncodeToString(sha1sum[:]) {
		t.Errorf("jar.sha1 = %q，期望 %q", got, hex.EncodeToString(sha1sum[:]))
	}

	// 生成的 pom 含 GAV，且其校验和与 pom 字节实算一致。
	pom := e.getBody(t, adminToken, "mvn-web", "com/example/app/1.0.0/app-1.0.0.pom")
	for _, want := range []string{"<modelVersion>4.0.0</modelVersion>", "<groupId>com.example</groupId>", "<artifactId>app</artifactId>", "<version>1.0.0</version>"} {
		if !strings.Contains(pom, want) {
			t.Errorf("pom 缺少 %s：%s", want, pom)
		}
	}
	pomMd5 := md5.Sum([]byte(pom))
	if got := e.getBody(t, adminToken, "mvn-web", "com/example/app/1.0.0/app-1.0.0.pom.md5"); got != hex.EncodeToString(pomMd5[:]) {
		t.Errorf("pom.md5 = %q，期望 %q", got, hex.EncodeToString(pomMd5[:]))
	}
	if rec := e.rawReq(http.MethodGet, "/repository/mvn-web/com/example/app/1.0.0/app-1.0.0.pom.sha1", "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK {
		t.Errorf("pom.sha1 状态码 = %d，期望 200", rec.Code)
	}

	// artifact 级 metadata 含版本且 latest/release 正确，校验和文件存在。
	meta := e.getBody(t, adminToken, "mvn-web", "com/example/app/maven-metadata.xml")
	for _, want := range []string{"<groupId>com.example</groupId>", "<artifactId>app</artifactId>", "<version>1.0.0</version>", "<latest>1.0.0</latest>", "<release>1.0.0</release>"} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata 缺少 %s：%s", want, meta)
		}
	}
	metaMd5 := md5.Sum([]byte(meta))
	if got := e.getBody(t, adminToken, "mvn-web", "com/example/app/maven-metadata.xml.md5"); got != hex.EncodeToString(metaMd5[:]) {
		t.Errorf("metadata.md5 = %q，期望 %q", got, hex.EncodeToString(metaMd5[:]))
	}
	if rec := e.rawReq(http.MethodGet, "/repository/mvn-web/com/example/app/maven-metadata.xml.sha1", "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK {
		t.Errorf("metadata.sha1 状态码 = %d，期望 200", rec.Code)
	}
}

func TestMavenWebUploadSecondVersionUpdatesMetadata(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-multi", "hosted", "", nil)

	for _, v := range []string{"1.0.0", "1.1.0"} {
		rec := e.mavenUpload(t, adminToken, "mvn-multi", map[string]string{
			"groupId": "com.example", "artifactId": "lib", "version": v,
		}, "lib.jar", []byte("jar-"+v))
		if rec.Code != http.StatusCreated {
			t.Fatalf("上传 %s 状态码 = %d（体：%s）", v, rec.Code, rec.Body.String())
		}
	}

	meta := e.getBody(t, adminToken, "mvn-multi", "com/example/lib/maven-metadata.xml")
	for _, want := range []string{"<version>1.0.0</version>", "<version>1.1.0</version>", "<latest>1.1.0</latest>"} {
		if !strings.Contains(meta, want) {
			t.Errorf("metadata 缺少 %s：%s", want, meta)
		}
	}

	// 重传同版本：metadata 不重复 version。
	if rec := e.mavenUpload(t, adminToken, "mvn-multi", map[string]string{
		"groupId": "com.example", "artifactId": "lib", "version": "1.1.0",
	}, "lib.jar", []byte("jar-1.1.0-re")); rec.Code != http.StatusCreated {
		t.Fatalf("重传状态码 = %d", rec.Code)
	}
	meta = e.getBody(t, adminToken, "mvn-multi", "com/example/lib/maven-metadata.xml")
	if n := strings.Count(meta, "<version>1.1.0</version>"); n != 1 {
		t.Errorf("重传后 1.1.0 出现 %d 次，期望 1：%s", n, meta)
	}
}

func TestMavenWebUploadRejections(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-host", "hosted", "", nil)
	e.createRawRepo(t, adminToken, "raw-host", "private")

	valid := map[string]string{"groupId": "com.example", "artifactId": "app", "version": "1.0.0"}

	// SNAPSHOT 版本 → 400。
	if rec := e.mavenUpload(t, adminToken, "mvn-host", map[string]string{
		"groupId": "com.example", "artifactId": "app", "version": "1.0.0-SNAPSHOT",
	}, "a.jar", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("SNAPSHOT 上传状态码 = %d，期望 400", rec.Code)
	}

	// 非 maven 仓 → 409。
	if rec := e.mavenUpload(t, adminToken, "raw-host", valid, "a.jar", []byte("x")); rec.Code != http.StatusConflict {
		t.Errorf("raw 仓上传状态码 = %d，期望 409", rec.Code)
	}

	// maven proxy 仓 → 409。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nf", http.StatusNotFound)
	}))
	defer srv.Close()
	e.createMavenRepo(t, adminToken, "mvn-proxy", "proxy", srv.URL, nil)
	if rec := e.mavenUpload(t, adminToken, "mvn-proxy", valid, "a.jar", []byte("x")); rec.Code != http.StatusConflict {
		t.Errorf("proxy 仓上传状态码 = %d，期望 409", rec.Code)
	}

	// 非法 groupId（含路径穿越字符）→ 400。
	if rec := e.mavenUpload(t, adminToken, "mvn-host", map[string]string{
		"groupId": "com/../etc", "artifactId": "app", "version": "1.0.0",
	}, "a.jar", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 groupId 状态码 = %d，期望 400", rec.Code)
	}
	// 纯点 artifactId（拼出 .. 目录段）→ 400。
	if rec := e.mavenUpload(t, adminToken, "mvn-host", map[string]string{
		"groupId": "com.example", "artifactId": "..", "version": "1.0.0",
	}, "a.jar", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("纯点 artifactId 状态码 = %d，期望 400", rec.Code)
	}
	// 缺 file 字段 → 400。
	if rec := e.mavenUpload(t, adminToken, "mvn-host", valid, "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 file 状态码 = %d，期望 400", rec.Code)
	}

	// 匿名（私有 maven 仓）→ 401。
	vis := api.CreateRepositoryRequestVisibility("private")
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, api.CreateRepositoryRequest{
		Name:       "mvn-private",
		Format:     api.CreateRepositoryRequestFormat("maven"),
		Type:       api.CreateRepositoryRequestType("hosted"),
		Visibility: &vis,
	}, nil); code != http.StatusCreated {
		t.Fatalf("建私有 maven 仓状态码 = %d", code)
	}
	if rec := e.mavenUpload(t, "", "mvn-private", valid, "a.jar", []byte("x")); rec.Code != http.StatusUnauthorized {
		t.Errorf("匿名上传状态码 = %d，期望 401", rec.Code)
	}

	// 有主体无 write 权 → 403。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "bob", Password: "bob-pass-12345"}, nil); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d", code)
	}
	var bobLogin api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "bob", Password: "bob-pass-12345"}, &bobLogin); code != http.StatusOK {
		t.Fatalf("bob 登录状态码 = %d", code)
	}
	if rec := e.mavenUpload(t, bobLogin.Token, "mvn-private", valid, "a.jar", []byte("x")); rec.Code != http.StatusForbidden {
		t.Errorf("无 write 权上传状态码 = %d，期望 403", rec.Code)
	}
}

func TestMavenWebUploadPomPackaging(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-pom", "hosted", "", nil)

	userPom := []byte("<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>bom</artifactId><version>2.0.0</version><packaging>pom</packaging></project>")
	rec := e.mavenUpload(t, adminToken, "mvn-pom", map[string]string{
		"groupId": "com.example", "artifactId": "bom", "version": "2.0.0", "packaging": "pom",
	}, "bom.pom", userPom)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传 pom 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var resp mavenUploadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	// packaging=pom：主文件即 pom，不再生成骨架 → 6 个文件（pom + metadata 各 + .md5/.sha1）。
	if len(resp.Files) != 6 {
		t.Errorf("files 数 = %d，期望 6：%v", len(resp.Files), resp.Files)
	}
	// 主文件保留用户上传字节（未被生成骨架覆盖）。
	if got := e.getBody(t, adminToken, "mvn-pom", "com/example/bom/2.0.0/bom-2.0.0.pom"); got != string(userPom) {
		t.Errorf("pom 内容被覆盖：%q", got)
	}
}
