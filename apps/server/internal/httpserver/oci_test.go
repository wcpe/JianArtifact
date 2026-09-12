package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func createOCIHostedRepo(t *testing.T, e *protocolEnv, token, name string) {
	t.Helper()
	visibility := api.CreateRepositoryRequestVisibility("private")
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token, api.CreateRepositoryRequest{
		Name: name, Format: api.CreateRepositoryRequestFormat("docker"), Type: api.CreateRepositoryRequestType("hosted"), Visibility: &visibility,
	}, nil); code != http.StatusCreated {
		t.Fatalf("创建 OCI hosted 仓库状态码 = %d", code)
	}
}

func ociTestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func putOCIBlob(t *testing.T, e *protocolEnv, token, repo, image string, data []byte) string {
	t.Helper()
	digest := ociTestDigest(data)
	path := "/v2/" + repo + "/" + image + "/blobs/uploads/?digest=" + url.QueryEscape(digest)
	if rec := e.rawReq(http.MethodPost, path, "Bearer "+token, "application/octet-stream", data); rec.Code != http.StatusCreated {
		t.Fatalf("OCI 单请求 blob 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	return digest
}

func putOCIChunkedBlob(t *testing.T, e *protocolEnv, token, repo, image string, data []byte) string {
	t.Helper()
	digest := ociTestDigest(data)
	start := e.rawReq(http.MethodPost, "/v2/"+repo+"/"+image+"/blobs/uploads/", "Bearer "+token, "", nil)
	if start.Code != http.StatusAccepted {
		t.Fatalf("OCI 分块上传创建会话状态码 = %d，响应 = %s", start.Code, start.Body.String())
	}
	location := start.Header().Get("Location")
	if location == "" {
		t.Fatal("OCI 分块上传未返回 Location")
	}
	if rec := e.rawReq(http.MethodPatch, location, "Bearer "+token, "application/octet-stream", data[:len(data)/2]); rec.Code != http.StatusAccepted {
		t.Fatalf("OCI 分块上传 PATCH 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if rec := e.rawReq(http.MethodPut, location+"?digest="+url.QueryEscape(digest), "Bearer "+token, "application/octet-stream", data[len(data)/2:]); rec.Code != http.StatusCreated {
		t.Fatalf("OCI 分块上传完成状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	return digest
}

func putOCIManifest(t *testing.T, e *protocolEnv, token, repo, image, reference string, body []byte) string {
	t.Helper()
	if rec := e.rawReq(http.MethodPut, "/v2/"+repo+"/"+image+"/manifests/"+reference, "Bearer "+token, "application/vnd.oci.image.manifest.v1+json", body); rec.Code != http.StatusCreated {
		t.Fatalf("OCI manifest 状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	return ociTestDigest(body)
}

func TestOCIHostedUploadHeadMissingLayerAndImmutableTag(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, adminToken, "oci-hosted")
	if err := e.repoRepo.UpdateConfig("oci-hosted", `{"immutableRelease":true}`); err != nil {
		t.Fatalf("设置 OCI Release 不可变失败：%v", err)
	}

	config := []byte("oci-config")
	layer := []byte("oci-layer")
	configDigest := putOCIBlob(t, e, adminToken, "oci-hosted", "demo", config)
	layerDigest := putOCIChunkedBlob(t, e, adminToken, "oci-hosted", "demo", layer)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(config),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := putOCIManifest(t, e, adminToken, "oci-hosted", "demo", "release", body)

	if rec := e.rawReq(http.MethodHead, "/v2/oci-hosted/demo/manifests/"+digest, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("OCI manifest HEAD 状态码 = %d，响应体长度 = %d", rec.Code, rec.Body.Len())
	}
	if rec := e.rawReq(http.MethodHead, "/v2/oci-hosted/demo/blobs/"+layerDigest, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("OCI blob HEAD 状态码 = %d，响应体长度 = %d", rec.Code, rec.Body.Len())
	}
	if rec := e.rawReq(http.MethodGet, "/v2/oci-hosted/demo/blobs/sha256/"+strings.Repeat("0", 64), "Bearer "+adminToken, "", nil); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("OCI 非法 blob digest 状态码 = %d，期望 400 或 404", rec.Code)
	}

	missing := map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": ociTestDigest([]byte("missing")), "size": 7},
	}
	missingBody, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.rawReq(http.MethodPut, "/v2/oci-hosted/demo/manifests/missing-layer", "Bearer "+adminToken, "application/json", missingBody); rec.Code != http.StatusNotFound {
		t.Fatalf("缺少 layer 的 manifest 状态码 = %d，期望 404", rec.Code)
	}

	changedBody := bytes.Replace(body, []byte(layerDigest), []byte(configDigest), 1)
	if rec := e.rawReq(http.MethodPut, "/v2/oci-hosted/demo/manifests/release", "Bearer "+adminToken, "application/json", changedBody); rec.Code != http.StatusConflict {
		t.Fatalf("不可变 tag 覆盖状态码 = %d，期望 409", rec.Code)
	}
}

func TestOCIManifestCommitAtomicallyPublishesStagedBlobs(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, adminToken, "oci-atomic")
	repo, err := e.repoRepo.GetByName("oci-atomic")
	if err != nil {
		t.Fatal(err)
	}
	beforeSeq := int64(0)

	config := []byte("oci-atomic-config")
	layer := []byte("oci-atomic-layer")
	configDigest := putOCIBlob(t, e, adminToken, "oci-atomic", "demo", config)
	layerDigest := putOCIChunkedBlob(t, e, adminToken, "oci-atomic", "demo", layer)
	assets, err := e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("manifest 提交前不应存在可见 OCI asset，实际=%d", len(assets))
	}
	if records, err := repository.NewReplicationOperationRepo(e.db).ListRecordsSince(beforeSeq, 20); err != nil || len(records) != 0 {
		t.Fatalf("manifest 提交前不应产生复制记录 records=%d err=%v", len(records), err)
	}
	if audits, err := e.auditLogs.List(repository.AuditFilter{Action: "oci.blob.put", Limit: 20}); err != nil || len(audits) != 0 {
		t.Fatalf("manifest 提交前不应产生 blob 发布审计 audits=%d err=%v", len(audits), err)
	}

	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": configDigest, "size": len(config)},
		"layers":        []map[string]any{{"digest": layerDigest, "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	putOCIManifest(t, e, adminToken, "oci-atomic", "demo", "v1", body)
	assets, err = e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil || len(assets) != 4 {
		t.Fatalf("manifest 提交后应原子公开 blob、manifest 和 tag，assets=%d err=%v", len(assets), err)
	}
	records, err := repository.NewReplicationOperationRepo(e.db).ListRecordsSince(beforeSeq, 20)
	if err != nil || len(records) != 1 || records[0].Type != "operation" || records[0].Operation == nil || len(records[0].Operation.Items) != 4 {
		t.Fatalf("OCI 逻辑发布必须写入单个完整 operation records=%+v err=%v", records, err)
	}
}

func TestOCIManifestCommitRollsBackWhenTagWriteFails(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, adminToken, "oci-rollback")
	repo, err := e.repoRepo.GetByName("oci-rollback")
	if err != nil {
		t.Fatal(err)
	}
	beforeSeq := int64(0)
	config := []byte("oci-rollback-config")
	layer := []byte("oci-rollback-layer")
	configDigest := putOCIBlob(t, e, adminToken, "oci-rollback", "demo", config)
	layerDigest := putOCIBlob(t, e, adminToken, "oci-rollback", "demo", layer)
	if _, err := e.assetRepo.DB().Exec(`CREATE TRIGGER reject_oci_tag
		BEFORE INSERT ON asset WHEN NEW.path = 'oci/tags/demo/v1'
		BEGIN SELECT RAISE(ABORT, '拒绝 tag 写入'); END`); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": configDigest, "size": len(config)},
		"layers":        []map[string]any{{"digest": layerDigest, "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.rawReq(http.MethodPut, "/v2/oci-rollback/demo/manifests/v1", "Bearer "+adminToken, "application/json", body); rec.Code != http.StatusInternalServerError {
		t.Fatalf("tag 注入失败状态码=%d，期望 500，响应=%s", rec.Code, rec.Body.String())
	}
	assets, err := e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil || len(assets) != 0 {
		t.Fatalf("tag 写入失败不能留下半 manifest 或 blob 引用，assets=%d err=%v", len(assets), err)
	}
	if records, err := repository.NewReplicationOperationRepo(e.db).ListRecordsSince(beforeSeq, 20); err != nil || len(records) != 0 {
		t.Fatalf("失败的 OCI 提交不得写入复制 operation records=%d err=%v", len(records), err)
	}
	if _, err := e.assetRepo.DB().Exec(`DROP TRIGGER reject_oci_tag`); err != nil {
		t.Fatal(err)
	}
	putOCIManifest(t, e, adminToken, "oci-rollback", "demo", "v1", body)
	assets, err = e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil || len(assets) != 4 {
		t.Fatalf("失败后重试应复用暂存 blob 完成原子发布，assets=%d err=%v", len(assets), err)
	}
}

func TestOCIHostedRejectsDigestMismatchWithoutLeavingBlob(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, adminToken, "oci-digest")
	wrong := "sha256:" + strings.Repeat("0", 64)
	data := []byte("digest mismatch")
	if rec := e.rawReq(http.MethodPost, "/v2/oci-digest/demo/blobs/uploads/?digest="+url.QueryEscape(wrong), "Bearer "+adminToken, "application/octet-stream", data); rec.Code != http.StatusBadRequest {
		t.Fatalf("digest 不匹配状态码 = %d，期望 400", rec.Code)
	}
	repo, err := e.repoRepo.GetByName("oci-digest")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("digest 不匹配后不应残留 asset，实际 %d 条", len(assets))
	}
}

func TestOCIPublishPolicyRejectionsAuditActor(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, admin, "oci-policy")
	createOCIHostedRepo(t, e, admin, "oci-policy-other")
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", admin,
		api.CreateUserRequest{Username: "oci-policy-user", Password: "oci-policy-password"}, &user); code != http.StatusCreated {
		t.Fatalf("创建 OCI 发布用户状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/oci-policy/acl", admin,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: "write"}}}, nil); code != http.StatusOK {
		t.Fatalf("设置 OCI 写 ACL 状态码=%d", code)
	}
	policy := map[string]any{"allowedPrefixes": []string{"oci/blobs"}, "maxAssetsHour": 1}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/users/"+strconv.FormatInt(user.Id, 10)+"/publish-policies/oci-policy", admin, policy, nil); code != http.StatusOK {
		t.Fatalf("保存 OCI 发布策略状态码=%d", code)
	}
	basic := basicUserPasswordHeader("oci-policy-user", "oci-policy-password")
	first := []byte("oci-policy-first")
	firstDigest := ociTestDigest(first)
	if rec := e.rawReq(http.MethodPost, "/v2/oci-policy/demo/blobs/uploads/?digest="+url.QueryEscape(firstDigest), basic, "application/octet-stream", first); rec.Code != http.StatusCreated {
		t.Fatalf("允许 OCI blob 发布状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
	second := []byte("oci-policy-second")
	if rec := e.rawReq(http.MethodPost, "/v2/oci-policy/demo/blobs/uploads/?digest="+url.QueryEscape(ociTestDigest(second)), basic, "application/octet-stream", second); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("OCI 额度超限状态码=%d，期望 429", rec.Code)
	}
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": firstDigest, "size": len(first)}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.rawReq(http.MethodPut, "/v2/oci-policy/demo/manifests/v1", basic, "application/json", manifest); rec.Code != http.StatusForbidden {
		t.Fatalf("OCI 越前缀 manifest 状态码=%d，期望 403", rec.Code)
	}
	if rec := e.rawReq(http.MethodPost, "/v2/oci-policy-other/demo/blobs/uploads/?digest="+url.QueryEscape(ociTestDigest([]byte("other"))), basic, "application/octet-stream", []byte("other")); rec.Code != http.StatusForbidden {
		t.Fatalf("无 OCI 写 ACL 发布状态码=%d，期望 403", rec.Code)
	}
	assertProtocolAudit(t, e, "oci-policy-user", user.Id, "oci.blob.put", "rejected", "quota_exceeded")
	assertProtocolAudit(t, e, "oci-policy-user", user.Id, "oci.manifest.put", "rejected", "publish_path_denied")
	assertProtocolAudit(t, e, "oci-policy-user", user.Id, "oci.blob.put", "rejected", "authorization_denied")
}

// TestOCIDockerBasicLoginChallengeAndPush 覆盖 Docker 原生握手：未认证探测必须发起
// Basic 质询，Docker login 携带账号口令后，后续 push 的首个 blob 上传请求必须可写。
func TestOCIDockerBasicLoginChallengeAndPush(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createOCIHostedRepo(t, e, adminToken, "oci-docker-login")
	server := httptest.NewServer(e.h)
	t.Cleanup(server.Close)

	request := func(method, path string, credentials bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatalf("构造 Docker 请求：%v", err)
		}
		if credentials {
			req.SetBasicAuth("admin", "admin-pass-123")
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("执行 Docker 请求：%v", err)
		}
		return resp
	}

	challenge := request(http.MethodGet, "/v2/", false)
	defer func() { _ = challenge.Body.Close() }()
	if challenge.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证 Docker 探测状态码 = %d，期望 401", challenge.StatusCode)
	}
	if got := challenge.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("Docker 探测质询 = %q，期望 Basic", got)
	}
	unauthenticatedPush := request(http.MethodPost, "/v2/oci-docker-login/probe/blobs/uploads/", false)
	defer func() { _ = unauthenticatedPush.Body.Close() }()
	if unauthenticatedPush.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证 Docker push 状态码 = %d，期望 401", unauthenticatedPush.StatusCode)
	}
	if got := unauthenticatedPush.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("未认证 Docker push 协议头 = %q", got)
	}
	var unauthorized struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(unauthenticatedPush.Body).Decode(&unauthorized); err != nil {
		t.Fatalf("解析 Docker 未认证响应：%v", err)
	}
	if len(unauthorized.Errors) != 1 || unauthorized.Errors[0].Code != "UNAUTHORIZED" {
		t.Fatalf("Docker 未认证响应 = %+v", unauthorized)
	}

	login := request(http.MethodGet, "/v2/", true)
	defer func() { _ = login.Body.Close() }()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("Docker Basic login 探测状态码 = %d，期望 200", login.StatusCode)
	}
	push := request(http.MethodPost, "/v2/oci-docker-login/probe/blobs/uploads/", true)
	defer func() { _ = push.Body.Close() }()
	if push.StatusCode != http.StatusAccepted {
		t.Fatalf("Docker push 首次上传状态码 = %d，期望 202", push.StatusCode)
	}
}

func TestOCIProxyAndGroupReadCache(t *testing.T) {
	var hits int32
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/demo/manifests/v1" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(manifest)
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("oci-proxy", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repoRepo.Create("oci-group", "docker", "group", "public", `{"members":["oci-proxy"]}`); err != nil {
		t.Fatal(err)
	}
	path := "/v2/oci-proxy/demo/manifests/v1"
	for i := 0; i < 2; i++ {
		rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), manifest) {
			t.Fatalf("OCI proxy manifest 状态码=%d，响应=%s", rec.Code, rec.Body.String())
		}
	}
	group := e.rawReq(http.MethodGet, "/v2/oci-group/demo/manifests/v1", "Bearer "+adminToken, "", nil)
	if group.Code != http.StatusOK || !bytes.Equal(group.Body.Bytes(), manifest) {
		t.Fatalf("OCI group manifest 状态码=%d，响应=%s", group.Code, group.Body.String())
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("OCI proxy/group 应命中本地缓存，实际回源 %d 次", got)
	}
}

func TestOCIGroupPriorityFallbackAndTagDeduplication(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	for _, name := range []string{"oci-group-first", "oci-group-second"} {
		createOCIHostedRepo(t, e, admin, name)
	}
	publish := func(repo, tag string, content []byte) []byte {
		digest := putOCIBlob(t, e, admin, repo, "demo", content)
		body, err := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": digest, "size": len(content)}})
		if err != nil {
			t.Fatal(err)
		}
		putOCIManifest(t, e, admin, repo, "demo", tag, body)
		return body
	}
	firstShared := publish("oci-group-first", "shared", []byte("first-shared"))
	publish("oci-group-second", "shared", []byte("second-shared"))
	secondFallback := publish("oci-group-second", "fallback", []byte("second-fallback"))
	if _, err := e.repoRepo.Create("oci-group-priority", "docker", "group", "public", `{"members":["oci-group-first","oci-group-second"]}`); err != nil {
		t.Fatal(err)
	}
	var brokenHits int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&brokenHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	config, _ := json.Marshal(map[string]any{"remoteUrl": broken.URL})
	if _, err := e.repoRepo.Create("oci-group-broken", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repoRepo.Create("oci-group-fallback", "docker", "group", "public", `{"members":["oci-group-broken","oci-group-second"]}`); err != nil {
		t.Fatal(err)
	}
	if rec := e.rawReq(http.MethodGet, "/v2/oci-group-priority/demo/manifests/shared", "Bearer "+admin, "", nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), firstShared) {
		t.Fatalf("group 必须优先命中第一个成员，状态=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := e.rawReq(http.MethodGet, "/v2/oci-group-fallback/demo/manifests/fallback", "Bearer "+admin, "", nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), secondFallback) {
		t.Fatalf("group 首成员失败后必须回退，状态=%d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&brokenHits) != 1 {
		t.Fatalf("失败成员应被访问一次，实际=%d", brokenHits)
	}
	if rec := e.rawReq(http.MethodGet, "/v2/oci-group-priority/demo/manifests/missing", "Bearer "+admin, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("所有成员未命中时 group 必须返回 404，状态=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := e.rawReq(http.MethodGet, "/v2/oci-group-priority/demo/tags/list", "Bearer "+admin, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group tags 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var tags struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tags); err != nil || strings.Join(tags.Tags, ",") != "fallback,shared" {
		t.Fatalf("group tags 必须排序去重，tags=%v err=%v", tags.Tags, err)
	}
}

func TestOCIProxySingleFlightAndUpstreamErrorMapping(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	started := make(chan struct{})
	release := make(chan struct{})
	var hits int32
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(manifest)
	}))
	defer upstream.Close()
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("oci-concurrent-proxy", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	const consumers = 8
	results := make(chan *httptest.ResponseRecorder, consumers)
	var wg sync.WaitGroup
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- e.rawReq(http.MethodGet, "/v2/oci-concurrent-proxy/demo/manifests/v1", "Bearer "+admin, "", nil)
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("并发 proxy 未发起上游请求")
	}
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), manifest) {
			t.Fatalf("并发 proxy 响应状态=%d body=%s", result.Code, result.Body.String())
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("同路径并发必须 single-flight，实际上游次数=%d", got)
	}

	assertStatus := func(name string, upstreamHandler http.HandlerFunc, requestContext context.Context, want int, secret string) {
		t.Helper()
		server := httptest.NewServer(upstreamHandler)
		defer server.Close()
		credentialRef := "OCI_PROXY_" + strings.ToUpper(name)
		t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, "user:"+secret)
		cfg, _ := json.Marshal(map[string]any{"remoteUrl": server.URL, "credentialRef": credentialRef})
		if _, err := e.repoRepo.Create("oci-error-"+name, "docker", "proxy", "public", string(cfg)); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/v2/oci-error-"+name+"/demo/manifests/v1", nil).WithContext(requestContext)
		req.RemoteAddr = "127.0.0.1:43210"
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != want || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), credentialRef) {
			t.Fatalf("proxy %s 状态=%d body=%s，期望=%d 且不得泄露凭据", name, rec.Code, rec.Body.String(), want)
		}
	}
	assertStatus("notfound", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }, context.Background(), http.StatusNotFound, "not-found-secret")
	assertStatus("failure", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, context.Background(), http.StatusBadGateway, "failure-secret")
	timeoutContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assertStatus("timeout", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, timeoutContext, http.StatusGatewayTimeout, "timeout-secret")
}

func TestOCIProxyExchangesBearerTokenWithoutForwardingStaticCredential(t *testing.T) {
	const credentialRef = "OCI_PROXY_CREDENTIAL"
	const credential = "proxy-user:static-secret"
	const accessToken = "short-lived-pull-token"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest := ociTestDigest(manifest)
	var registryHits int32
	var tokenHits int32
	var tokenURL string
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenHits, 1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("跨域 token 交换不得携带静态凭据，得 %q", got)
		}
		if got := r.URL.Query().Get("scope"); got != "repository:demo:pull" {
			t.Fatalf("token scope 必须收缩为单仓 pull，得 %q", got)
		}
		if got := r.URL.Query().Get("service"); got != "registry-test" {
			t.Fatalf("token service = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": accessToken})
	}))
	defer authServer.Close()
	tokenURL = authServer.URL + "/token"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&registryHits, 1)
		switch r.Header.Get("Authorization") {
		case "Basic cHJveHktdXNlcjpzdGF0aWMtc2VjcmV0":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenURL+`",service="registry-test",scope="repository:demo:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer " + accessToken:
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifest)
		default:
			t.Fatalf("registry 收到意外认证头 %q", r.Header.Get("Authorization"))
		}
	}))
	defer registry.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": registry.URL, "credentialRef": credentialRef})
	if _, err := e.repoRepo.Create("oci-secure-proxy", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatalf("创建 OCI proxy 仓库：%v", err)
	}
	path := "/v2/oci-secure-proxy/demo/manifests/v1"
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, path, "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), manifest) {
			t.Fatalf("OCI Bearer proxy 读取状态=%d body=%s", res.Code, res.Body.String())
		}
	}
	if got := atomic.LoadInt32(&tokenHits); got != 1 {
		t.Fatalf("缓存命中后不得重复 token 交换，实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&registryHits); got != 2 {
		t.Fatalf("OCI proxy 应首跳+令牌重试各一次，实际 %d 次", got)
	}
}

func TestOCIProxyDigestMismatchDoesNotCacheOrLeakUpstreamSecrets(t *testing.T) {
	const credentialRef = "OCI_DIGEST_CREDENTIAL"
	const credential = "digest-user:must-not-leak"
	const privateRealm = "https://private-realm.example/token"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	wrongBody := []byte(`{"schemaVersion":2,"wrong":true}`)
	expected := ociTestDigest([]byte("expected manifest"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", expected)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(wrongBody)
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL, "credentialRef": credentialRef})
	if _, err := e.repoRepo.Create("oci-digest-proxy", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatalf("创建 OCI proxy 仓库：%v", err)
	}
	for _, requestPath := range []string{
		"/v2/oci-digest-proxy/demo/manifests/" + expected,
		"/v2/oci-digest-proxy/demo/blobs/" + expected,
	} {
		res := e.rawReq(http.MethodGet, requestPath, "Bearer "+admin, "", nil)
		if res.Code != http.StatusBadGateway {
			t.Fatalf("摘要不符路径=%s 状态码=%d，期望 502，响应=%s", requestPath, res.Code, res.Body.String())
		}
		for _, secret := range []string{credential, privateRealm, "must-not-leak"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("OCI proxy 错误不得泄露敏感信息 %q：%s", secret, res.Body.String())
			}
		}
	}
	repo, err := e.repoRepo.GetByName("oci-digest-proxy")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := e.assetRepo.ListByRepo(repo.ID, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("OCI 摘要不符不得写入缓存，实际 %d 条", len(assets))
	}
}

func TestOCIProxyRejectsUnsafeBearerRealmWithoutLeakingCredential(t *testing.T) {
	const credentialRef = "OCI_UNSAFE_REALM_CREDENTIAL"
	const credential = "realm-user:realm-secret"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://token-user:token-secret@auth.example.test/token",service="registry",scope="repository:demo:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL, "credentialRef": credentialRef})
	if _, err := e.repoRepo.Create("oci-unsafe-realm", "docker", "proxy", "public", string(config)); err != nil {
		t.Fatalf("创建 OCI proxy 仓库：%v", err)
	}
	res := e.rawReq(http.MethodGet, "/v2/oci-unsafe-realm/demo/manifests/v1", "Bearer "+admin, "", nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("不安全 realm 状态码=%d，期望 502，响应=%s", res.Code, res.Body.String())
	}
	for _, secret := range []string{credential, "token-secret", "auth.example.test"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("不安全 realm 错误不得泄露 %q：%s", secret, res.Body.String())
		}
	}
}
