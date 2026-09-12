package httpserver_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func createFormatRepo(t *testing.T, e *protocolEnv, _ string, name, format, typ string, members ...string) {
	t.Helper()
	config := "{}"
	if len(members) > 0 {
		data, _ := json.Marshal(map[string]any{"members": members})
		config = string(data)
	}
	if _, err := e.repoRepo.Create(name, format, typ, "public", config); err != nil {
		t.Fatalf("创建 %s 仓库 %s：%v", format, name, err)
	}
}

func cargoFrame(name, version string, crate []byte) []byte {
	metadata, _ := json.Marshal(map[string]any{"name": name, "vers": version, "deps": []any{}, "features": map[string]any{}})
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(metadata)))
	_, _ = out.Write(metadata)
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(crate)))
	_, _ = out.Write(crate)
	return out.Bytes()
}

func TestCargoHostedProtocolLifecycle(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "cargo-hosted", "cargo", "hosted")

	config := e.rawReq(http.MethodGet, "/cargo/cargo-hosted/config.json", "Bearer "+admin, "", nil)
	if config.Code != http.StatusOK || !bytes.Contains(config.Body.Bytes(), []byte(`"dl"`)) || !bytes.Contains(config.Body.Bytes(), []byte(`"api"`)) {
		t.Fatalf("Cargo config 无效：状态码=%d，响应=%s", config.Code, config.Body.String())
	}

	crate := []byte("cargo-crate-v1")
	frame := cargoFrame("demo-crate", "1.0.0", crate)
	res := e.rawReq(http.MethodPut, "/cargo/cargo-hosted/api/v1/crates/new", "Bearer "+admin, "application/octet-stream", frame)
	if res.Code != http.StatusOK {
		t.Fatalf("Cargo publish 状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if duplicate := e.rawReq(http.MethodPut, "/cargo/cargo-hosted/api/v1/crates/new", "Bearer "+admin, "application/octet-stream", frame); duplicate.Code != http.StatusConflict {
		t.Fatalf("重复 Cargo publish 状态码=%d，期望 409", duplicate.Code)
	}

	index := e.rawReq(http.MethodGet, "/cargo/cargo-hosted/de/mo/demo-crate", "Bearer "+admin, "", nil)
	if index.Code != http.StatusOK || !bytes.Contains(index.Body.Bytes(), []byte(`"vers":"1.0.0"`)) {
		t.Fatalf("Cargo sparse 索引无版本：状态码=%d，响应=%s", index.Code, index.Body.String())
	}
	download := e.rawReq(http.MethodGet, "/cargo/cargo-hosted/api/v1/crates/demo-crate/1.0.0/download", "Bearer "+admin, "", nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), crate) {
		t.Fatalf("Cargo 下载不一致：状态码=%d，内容=%q", download.Code, download.Body.Bytes())
	}
	if head := e.rawReq(http.MethodHead, "/cargo/cargo-hosted/api/v1/crates/demo-crate/1.0.0/download", "Bearer "+admin, "", nil); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("Cargo HEAD 状态码=%d，body=%d", head.Code, head.Body.Len())
	}

	if yank := e.rawReq(http.MethodDelete, "/cargo/cargo-hosted/api/v1/crates/demo-crate/1.0.0/yank", "Bearer "+admin, "", nil); yank.Code != http.StatusOK {
		t.Fatalf("Cargo yank 状态码=%d", yank.Code)
	}
	index = e.rawReq(http.MethodGet, "/cargo/cargo-hosted/de/mo/demo-crate", "Bearer "+admin, "", nil)
	if !bytes.Contains(index.Body.Bytes(), []byte(`"yanked":true`)) {
		t.Fatalf("Cargo yank 未反映到 sparse 索引：%s", index.Body.String())
	}
	if unyank := e.rawReq(http.MethodPut, "/cargo/cargo-hosted/api/v1/crates/demo-crate/1.0.0/unyank", "Bearer "+admin, "", nil); unyank.Code != http.StatusOK {
		t.Fatalf("Cargo unyank 状态码=%d", unyank.Code)
	}

	bad := e.rawReq(http.MethodPut, "/cargo/cargo-hosted/api/v1/crates/new", "Bearer "+admin, "application/octet-stream", []byte{1, 2, 3})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("坏 Cargo 发布帧状态码=%d，期望 400", bad.Code)
	}
}

func TestCargoConfigUsesConfiguredPublicURL(t *testing.T) {
	const publicURL = "https://standby.example.test"
	e := newProtocolEnvWithPublicURL(t, publicURL)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "cargo-public-url", "cargo", "hosted")

	req := httptest.NewRequest(http.MethodGet, "http://origin.internal:8081/cargo/cargo-public-url/config.json", nil)
	req.Host = "origin.internal:8081"
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Cargo config 状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
	var config struct {
		Dl  string `json:"dl"`
		Api string `json:"api"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &config); err != nil {
		t.Fatalf("解析 Cargo config：%v", err)
	}
	if want := publicURL + "/cargo/cargo-public-url/api/v1/crates/{crate}/{version}/download"; config.Dl != want {
		t.Fatalf("Cargo dl=%q，期望=%q", config.Dl, want)
	}
	if want := publicURL + "/cargo/cargo-public-url"; config.Api != want {
		t.Fatalf("Cargo api=%q，期望=%q", config.Api, want)
	}
}

func TestCargoHostedPublishAcceptsRegistryToken(t *testing.T) {
	e := newProtocolEnv(t)
	registryToken := e.bootstrapAdmin(t)
	createFormatRepo(t, e, registryToken, "cargo-registry-token", "cargo", "hosted")

	res := e.rawReq(
		http.MethodPut,
		"/cargo/cargo-registry-token/api/v1/crates/new",
		registryToken,
		"application/octet-stream",
		cargoFrame("registry-token-crate", "1.0.0", []byte("crate")),
	)
	if res.Code != http.StatusOK {
		t.Fatalf("Cargo 裸 registry token 发布状态码=%d，响应=%s", res.Code, res.Body.String())
	}
}

func TestCargoPublishPolicyRejectionsAuditActor(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	visibility := api.CreateRepositoryRequestVisibility("private")
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", admin, api.CreateRepositoryRequest{
		Name: "cargo-policy", Format: "cargo", Type: "hosted", Visibility: &visibility,
	}, nil); code != http.StatusCreated {
		t.Fatalf("创建 Cargo 仓库状态码=%d", code)
	}
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", admin,
		api.CreateUserRequest{Username: "cargo-policy-user", Password: "cargo-policy-password"}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布用户状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/cargo-policy/acl", admin,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: "write"}}}, nil); code != http.StatusOK {
		t.Fatalf("设置 Cargo 写 ACL 状态码=%d", code)
	}
	policy := map[string]any{"allowedPrefixes": []string{"cargo/crates/cargo-policy"}, "maxAssetsHour": 1}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/users/"+strconv.FormatInt(user.Id, 10)+"/publish-policies/cargo-policy", admin, policy, nil); code != http.StatusOK {
		t.Fatalf("保存 Cargo 发布策略状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPatch, "/api/v1/repositories/cargo-policy", admin, map[string]any{"immutableRelease": true}, nil); code != http.StatusOK {
		t.Fatalf("设置 Cargo 仓库不可变 Release 状态码=%d", code)
	}
	basic := basicUserPasswordHeader("cargo-policy-user", "cargo-policy-password")
	publish := func(name, version string) *httptest.ResponseRecorder {
		return e.rawReq(http.MethodPut, "/cargo/cargo-policy/api/v1/crates/new", basic, "application/octet-stream", cargoFrame(name, version, []byte(name+version)))
	}
	if rec := publish("cargo-policy", "1.0.0"); rec.Code != http.StatusOK {
		t.Fatalf("允许 Cargo 发布状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
	if rec := e.rawReq(http.MethodDelete, "/cargo/cargo-policy/api/v1/crates/cargo-policy/1.0.0/yank", basic, "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("不可变 Cargo yank 状态码=%d，期望 409", rec.Code)
	}
	if rec := publish("cargo-blocked", "1.0.0"); rec.Code != http.StatusForbidden {
		t.Fatalf("越前缀 Cargo 发布状态码=%d，期望 403", rec.Code)
	}
	if rec := publish("cargo-policy", "1.0.0"); rec.Code != http.StatusConflict {
		t.Fatalf("不可变 Cargo 覆盖状态码=%d，期望 409", rec.Code)
	}
	if rec := publish("cargo-policy", "2.0.0"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("Cargo 额度超限状态码=%d，期望 429", rec.Code)
	}
	assertProtocolAudit(t, e, "cargo-policy-user", user.Id, "cargo.publish", "rejected", "publish_path_denied")
	assertProtocolAudit(t, e, "cargo-policy-user", user.Id, "cargo.publish", "rejected", "immutable_release")
	assertProtocolAudit(t, e, "cargo-policy-user", user.Id, "cargo.yank", "rejected", "immutable_release")
	assertProtocolAudit(t, e, "cargo-policy-user", user.Id, "cargo.publish", "rejected", "quota_exceeded")
}

func TestCargoRegistryTokenIsRejectedByRawRoute(t *testing.T) {
	e := newProtocolEnv(t)
	registryToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, registryToken, "raw-no-cargo-token", "public")

	res := e.rawReq(
		http.MethodPut,
		"/repository/raw-no-cargo-token/file.txt",
		registryToken,
		"text/plain",
		[]byte("payload"),
	)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("Raw 路由接受了 Cargo 裸 registry token：状态码=%d，响应=%s", res.Code, res.Body.String())
	}
}

func TestCargoGroupMergesMembersAndRejectsPublish(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "cargo-a", "cargo", "hosted")
	createFormatRepo(t, e, admin, "cargo-b", "cargo", "hosted")
	createFormatRepo(t, e, admin, "cargo-group", "cargo", "group", "cargo-a", "cargo-b")
	if res := e.rawReq(http.MethodPut, "/cargo/cargo-a/api/v1/crates/new", "Bearer "+admin, "application/octet-stream", cargoFrame("group-crate", "1.0.0", []byte("a"))); res.Code != http.StatusOK {
		t.Fatalf("成员发布失败：%d %s", res.Code, res.Body.String())
	}
	if res := e.rawReq(http.MethodPut, "/cargo/cargo-group/api/v1/crates/new", "Bearer "+admin, "application/octet-stream", cargoFrame("group-crate", "2.0.0", []byte("b"))); res.Code != http.StatusConflict {
		t.Fatalf("Cargo group 发布状态码=%d，期望 409", res.Code)
	}
	index := e.rawReq(http.MethodGet, "/cargo/cargo-group/gr/ou/group-crate", "Bearer "+admin, "", nil)
	if index.Code != http.StatusOK || !bytes.Contains(index.Body.Bytes(), []byte(`"vers":"1.0.0"`)) {
		t.Fatalf("Cargo group 索引未合并：状态码=%d，响应=%s", index.Code, index.Body.String())
	}
}

func TestCargoProxyCachesIndexAndCrate(t *testing.T) {
	var indexHits, crateHits int32
	crate := []byte("proxy-crate-bytes")
	checksum := sha256.Sum256(crate)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			_, _ = w.Write([]byte(`{"dl":"` + upstream.URL + `/downloads/{crate}/{version}/download"}`))
		case "/pr/ox/proxy-crate":
			atomic.AddInt32(&indexHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"proxy-crate","vers":"1.0.0","deps":[],"cksum":"` + hex.EncodeToString(checksum[:]) + `","yanked":false}` + "\n"))
		case "/downloads/proxy-crate/1.0.0/download":
			atomic.AddInt32(&crateHits, 1)
			_, _ = w.Write(crate)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("cargo-proxy", "cargo", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, "/cargo/cargo-proxy/pr/ox/proxy-crate", "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(`"vers":"1.0.0"`)) {
			t.Fatalf("Cargo proxy 索引状态码=%d，响应=%s", res.Code, res.Body.String())
		}
	}
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, "/cargo/cargo-proxy/api/v1/crates/proxy-crate/1.0.0/download", "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || res.Body.String() != "proxy-crate-bytes" {
			t.Fatalf("Cargo proxy 下载状态码=%d，内容=%q", res.Code, res.Body.String())
		}
	}
	if atomic.LoadInt32(&indexHits) != 1 || atomic.LoadInt32(&crateHits) != 1 {
		t.Fatalf("Cargo proxy 未命中缓存：index=%d crate=%d", indexHits, crateHits)
	}
}

func TestCargoProxyRejectsChecksumMismatchBeforeCaching(t *testing.T) {
	var downloadHits int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			_, _ = w.Write([]byte(`{"dl":"` + upstream.URL + `/downloads/{crate}/{version}/download"}`))
		case "/ba/d-/bad-crate":
			_, _ = w.Write([]byte(`{"name":"bad-crate","vers":"1.0.0","deps":[],"cksum":"` + strings.Repeat("0", 64) + `","yanked":false}` + "\n"))
		case "/downloads/bad-crate/1.0.0/download":
			atomic.AddInt32(&downloadHits, 1)
			_, _ = w.Write([]byte("wrong-checksum"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("cargo-checksum-proxy", "cargo", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	if res := e.rawReq(http.MethodGet, "/cargo/cargo-checksum-proxy/ba/d-/bad-crate", "Bearer "+admin, "", nil); res.Code != http.StatusOK {
		t.Fatalf("Cargo proxy 索引状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if res := e.rawReq(http.MethodGet, "/cargo/cargo-checksum-proxy/api/v1/crates/bad-crate/1.0.0/download", "Bearer "+admin, "", nil); res.Code != http.StatusBadGateway {
		t.Fatalf("Cargo proxy checksum 不符状态码=%d，期望 502，响应=%s", res.Code, res.Body.String())
	}
	if downloadHits != 1 {
		t.Fatalf("checksum 不符时下载次数=%d，期望 1", downloadHits)
	}
	repo, err := e.repoRepo.GetByName("cargo-checksum-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "cargo/crates/bad-crate/1.0.0/bad-crate-1.0.0.crate"); err == nil {
		t.Fatal("checksum 不符不得缓存 crate")
	}
}

func TestCargoProxyRejectsUnsafeDownloadTemplate(t *testing.T) {
	crate := []byte("safe-template-crate")
	checksum := sha256.Sum256(crate)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			_, _ = w.Write([]byte(`{"dl":"file:///tmp/{crate}-{version}.crate"}`))
		case "/sa/fe/safe-template":
			_, _ = w.Write([]byte(`{"name":"safe-template","vers":"1.0.0","deps":[],"cksum":"` + hex.EncodeToString(checksum[:]) + `","yanked":false}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("cargo-unsafe-template", "cargo", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	if res := e.rawReq(http.MethodGet, "/cargo/cargo-unsafe-template/sa/fe/safe-template", "Bearer "+admin, "", nil); res.Code != http.StatusOK {
		t.Fatalf("Cargo proxy 索引状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if res := e.rawReq(http.MethodGet, "/cargo/cargo-unsafe-template/api/v1/crates/safe-template/1.0.0/download", "Bearer "+admin, "", nil); res.Code != http.StatusBadGateway {
		t.Fatalf("不安全 Cargo dl 模板状态码=%d，期望 502，响应=%s", res.Code, res.Body.String())
	}
}

func TestCargoRejectsInvalidIndexPathAndNonCargoRepository(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		_, _ = w.Write([]byte(`{"name":"path","vers":"1.0.0","deps":[],"cksum":"` + strings.Repeat("0", 64) + `","yanked":false}` + "\n"))
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("cargo-path-proxy", "cargo", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	if res := e.rawReq(http.MethodGet, "/cargo/cargo-path-proxy/not/a/path", "Bearer "+admin, "", nil); res.Code != http.StatusNotFound {
		t.Fatalf("非法 Cargo 索引路径状态码=%d，期望 404，响应=%s", res.Code, res.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("非法 Cargo 索引路径不得回源，实际 %d 次", upstreamHits)
	}

	e.createRawRepo(t, admin, "raw-not-cargo", "public")
	if res := e.rawReq(http.MethodPut, "/repository/raw-not-cargo/cargo/index/de/mo/demo", "Bearer "+admin, "application/json", []byte(`{"name":"demo","vers":"1.0.0"}`)); res.Code != http.StatusCreated {
		t.Fatalf("准备非 Cargo 制品状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if res := e.rawReq(http.MethodGet, "/cargo/raw-not-cargo/de/mo/demo", "Bearer "+admin, "", nil); res.Code != http.StatusNotFound {
		t.Fatalf("非 Cargo 仓库经 Cargo 路由状态码=%d，期望 404，响应=%s", res.Code, res.Body.String())
	}
}
