package httpserver_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// createMavenRepo 以管理员身份创建指定 type 的 maven 仓库；remoteURL/members 按需传入。
func (e *protocolEnv) createMavenRepo(t *testing.T, adminToken, name, typ, remoteURL string, members []string) {
	t.Helper()
	vis := api.CreateRepositoryRequestVisibility("public")
	req := api.CreateRepositoryRequest{
		Name:       name,
		Format:     api.CreateRepositoryRequestFormat("maven"),
		Type:       api.CreateRepositoryRequestType(typ),
		Visibility: &vis,
	}
	if remoteURL != "" {
		req.RemoteUrl = &remoteURL
	}
	if len(members) > 0 {
		req.Members = &members
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, req, nil); code != http.StatusCreated {
		t.Fatalf("建 maven %s 仓库 %s 状态码 = %d，期望 201", typ, name, code)
	}
}

func TestMavenHostedDeployAndGet(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-releases", "hosted", "", nil)

	jar := []byte("fake-jar-bytes")
	pom := []byte("<project><modelVersion>4.0.0</modelVersion></project>")
	jarPath := "/repository/mvn-releases/com/example/app/1.0.0/app-1.0.0.jar"
	pomPath := "/repository/mvn-releases/com/example/app/1.0.0/app-1.0.0.pom"

	if rec := e.rawReq(http.MethodPut, jarPath, "Bearer "+adminToken, "application/java-archive", jar); rec.Code != http.StatusCreated {
		t.Fatalf("部署 jar 状态码 = %d", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, pomPath, "Bearer "+adminToken, "application/xml", pom); rec.Code != http.StatusCreated {
		t.Fatalf("部署 pom 状态码 = %d", rec.Code)
	}

	// 拉取 jar 字节一致。
	rec := e.rawReq(http.MethodGet, jarPath, "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), jar) {
		t.Fatalf("拉取 jar 状态码 = %d，内容一致 = %v", rec.Code, bytes.Equal(rec.Body.Bytes(), jar))
	}

	// 校验和文件未部署 → 据 jar 现算 .sha1 返回。
	sum := sha1.Sum(jar)
	want := hex.EncodeToString(sum[:])
	rec = e.rawReq(http.MethodGet, jarPath+".sha1", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("拉取 .sha1 状态码 = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("现算 .sha1 = %q，期望 %q", got, want)
	}

	// 不存在的制品 → 404。
	if rec := e.rawReq(http.MethodGet, "/repository/mvn-releases/com/example/app/9.9.9/app-9.9.9.jar", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("缺失制品状态码 = %d，期望 404", rec.Code)
	}
}

func TestMavenProxyFetchesUpstream(t *testing.T) {
	upstreamJar := []byte("upstream maven jar")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/app-1.0.0.jar") {
			http.Error(w, "nf", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(upstreamJar)
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-central", "proxy", srv.URL, nil)

	path := "/repository/mvn-central/com/example/app/1.0.0/app-1.0.0.jar"
	rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), upstreamJar) {
		t.Fatalf("proxy 回源 jar 状态码 = %d，内容一致 = %v", rec.Code, bytes.Equal(rec.Body.Bytes(), upstreamJar))
	}

	// 上游无该路径 → 404。
	if rec := e.rawReq(http.MethodGet, "/repository/mvn-central/com/example/app/1.0.0/missing.pom", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("上游缺失状态码 = %d，期望 404", rec.Code)
	}
}

func TestMavenGroupMetadataMerge(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-a", "hosted", "", nil)
	e.createMavenRepo(t, adminToken, "mvn-b", "hosted", "", nil)
	e.createMavenRepo(t, adminToken, "mvn-all", "group", "", []string{"mvn-a", "mvn-b"})

	metaPath := "com/example/app/maven-metadata.xml"
	metaA := `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <latest>1.1.0</latest>
    <release>1.1.0</release>
    <versions>
      <version>1.0.0</version>
      <version>1.1.0</version>
    </versions>
    <lastUpdated>20240101000000</lastUpdated>
  </versioning>
</metadata>`
	metaB := `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>app</artifactId>
  <versioning>
    <latest>2.0.0-SNAPSHOT</latest>
    <release>1.2.0</release>
    <versions>
      <version>1.2.0</version>
      <version>2.0.0-SNAPSHOT</version>
    </versions>
    <lastUpdated>20240202000000</lastUpdated>
  </versioning>
</metadata>`
	if rec := e.rawReq(http.MethodPut, "/repository/mvn-a/"+metaPath, "Bearer "+adminToken, "application/xml", []byte(metaA)); rec.Code != http.StatusCreated {
		t.Fatalf("部署 mvn-a metadata 状态码 = %d", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/mvn-b/"+metaPath, "Bearer "+adminToken, "application/xml", []byte(metaB)); rec.Code != http.StatusCreated {
		t.Fatalf("部署 mvn-b metadata 状态码 = %d", rec.Code)
	}

	rec := e.rawReq(http.MethodGet, "/repository/mvn-all/"+metaPath, "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group metadata 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "2.0.0-SNAPSHOT"} {
		if !strings.Contains(body, "<version>"+v+"</version>") {
			t.Errorf("合并元数据缺版本 %s：%s", v, body)
		}
	}
	// release 应为最后一个非 SNAPSHOT 版本（1.2.0），latest 为合并列表末项（2.0.0-SNAPSHOT）。
	if !strings.Contains(body, "<release>1.2.0</release>") {
		t.Errorf("合并 release 期望 1.2.0：%s", body)
	}
	if !strings.Contains(body, "<latest>2.0.0-SNAPSHOT</latest>") {
		t.Errorf("合并 latest 期望 2.0.0-SNAPSHOT：%s", body)
	}

	// 全成员皆无的 metadata → 404。
	if rec := e.rawReq(http.MethodGet, "/repository/mvn-all/com/example/none/maven-metadata.xml", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("group 缺失 metadata 状态码 = %d，期望 404", rec.Code)
	}
}
