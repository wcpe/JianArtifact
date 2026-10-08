package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// grantAcls 以管理员身份一次性写入某仓库的全部 ACL 条目。
// 必须**一次写完**：ACL 端点是覆盖写（SetAcl 先 DELETE 后 INSERT），
// 分次调用会让后一次把前一次的条目抹掉。
func grantAcls(t *testing.T, e *protocolEnv, adminToken, repoName string, entries ...api.AclEntry) {
	t.Helper()
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/"+repoName+"/acl", adminToken,
		api.PutAclRequest{Items: entries}, nil); code != http.StatusOK {
		t.Fatalf("写入 %s 的 ACL 状态码 = %d，期望 200", repoName, code)
	}
}

// TestPublishActionConsistentAcrossProtocols 「发布」在所有协议端点上是同一个动作。
//
// 背景：FR-36 把 write 细化为 publish 后，若只有 raw 端点改判 publish 而 npm/oci/
// pypi/cargo/maven 仍判 write，则「只发 publish 授权」的账号能走 raw 发布却发不了
// npm 包——同一权限动作在不同协议给出不同结果。本用例把两类端点放在同一个仓库
// 授权下对照，钉住「改动后仍需一致」这件事：
//   - write 授权：两类端点都应放行（write ⊇ publish，向后兼容，既有用户不受影响）；
//   - read 授权：两类端点都应拒绝（只读不得发布）。
//
// publish-only 授权的完整矩阵在 domain/repository 层用例中覆盖（契约的 AclEntryAction
// 枚举在阶段三才放开 publish，HTTP 层此刻还发不出该动作）。
func TestPublishActionConsistentAcrossProtocols(t *testing.T) {
	t.Parallel()
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)

	// 建一个 raw 仓库与一个 npm 仓库（同一用户的相同授权，跨协议对照）。
	e.createRawRepo(t, adminToken, "xproto-raw", "private")
	e.createNpmRepo(t, adminToken, "xproto-npm", "hosted", "", nil)

	// 建两个普通用户：一个授 write、一个授 read。
	writer := api.User{}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "xproto-writer", Password: "xproto-writer-password-456"}, &writer); code != http.StatusCreated {
		t.Fatalf("建 write 用户状态码 = %d，期望 201", code)
	}
	reader := api.User{}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "xproto-reader", Password: "xproto-reader-password-456"}, &reader); code != http.StatusCreated {
		t.Fatalf("建 read 用户状态码 = %d，期望 201", code)
	}

	// 两个仓库都按同样的授权铺设，才能做「同一授权 × 不同协议」的对照。
	for _, repo := range []string{"xproto-raw", "xproto-npm"} {
		grantAcls(t, e, adminToken, repo,
			api.AclEntry{SubjectId: writer.Id, Action: api.AclEntryActionWrite},
			api.AclEntry{SubjectId: reader.Id, Action: api.AclEntryActionRead},
		)
	}

	// 各自签发协议 Token。
	writerToken := e.tokenFor(t, "xproto-writer")
	readerToken := e.tokenFor(t, "xproto-reader")

	t.Run("write 授权跨协议均可发布", func(t *testing.T) {
		// raw 端点（原生 PUT）。
		if rec := e.rawReq(http.MethodPut, "/repository/xproto-raw/w.txt", "Bearer "+writerToken, "text/plain", []byte("w")); rec.Code != http.StatusCreated {
			t.Errorf("raw PUT 状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
		}
		// npm 端点（PUT packument）：同为「发布」，判定必须与 raw 一致。
		body := npmPublishBody(t, "xproto-pkg", "1.0.0", "xproto-pkg-1.0.0.tgz", []byte("fake tgz"))
		if rec := e.rawReq(http.MethodPut, "/npm/xproto-npm/xproto-pkg", "Bearer "+writerToken, "application/json", body); rec.Code != http.StatusCreated {
			t.Errorf("npm publish 状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
		}
	})

	t.Run("read 授权跨协议均拒绝发布", func(t *testing.T) {
		if rec := e.rawReq(http.MethodPut, "/repository/xproto-raw/r.txt", "Bearer "+readerToken, "text/plain", []byte("r")); rec.Code != http.StatusForbidden {
			t.Errorf("raw PUT（仅 read）状态码 = %d，期望 403（体：%s）", rec.Code, rec.Body.String())
		}
		body := npmPublishBody(t, "xproto-pkg-read", "1.0.0", "xproto-pkg-read-1.0.0.tgz", []byte("fake tgz"))
		if rec := e.rawReq(http.MethodPut, "/npm/xproto-npm/xproto-pkg-read", "Bearer "+readerToken, "application/json", body); rec.Code != http.StatusForbidden {
			t.Errorf("npm PUT（仅 read）状态码 = %d，期望 403（体：%s）", rec.Code, rec.Body.String())
		}
	})
}

// tokenFor 以用户口令登录并签发一枚 API Token，返回其明文。
// 用户名与口令按被测用户的建号约定推导（<name> / <name>-password-456）。
func (e *protocolEnv) tokenFor(t *testing.T, name string) string {
	t.Helper()
	var login api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: name, Password: name + "-password-456"}, &login); code != http.StatusOK {
		t.Fatalf("用户 %s 登录状态码 = %d，期望 200", name, code)
	}
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", login.Token,
		api.CreateTokenRequest{Name: name + "-tok"}, &created); code != http.StatusCreated {
		t.Fatalf("签发 Token 状态码 = %d，期望 201（用户 %s）", code, name)
	}
	return created.Token
}
