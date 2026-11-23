package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestSecurityAuditIngressToQuery(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	const username = "vertical-admin"
	_, admin, err := svc.authSvc.Bootstrap(username, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := svc.repoSvc.Create("vertical-raw", "raw", "hosted", "public", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	publisher, _, err := svc.tokenSvc.Create(admin.ID, "vertical-publisher")
	if err != nil {
		t.Fatal(err)
	}
	startTestSecurityAudit(t, svc)
	server := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	t.Cleanup(server.Close)
	from := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	to := from.Add(5 * time.Minute)
	status, _, loginBody := auditTestRequest(t, server, http.MethodPost, "/api/v1/auth/login", "application/json", fmt.Sprintf(`{"username":%q,"password":%q}`, username, auditTestPassword), "")
	if status != http.StatusOK {
		t.Fatalf("交互登录失败：%d", status)
	}
	login := queryDecode[api.LoginResponse](t, loginBody)
	if login.Token == "" {
		t.Fatal("交互登录未返回会话")
	}

	read := func(t *testing.T, path string) string {
		t.Helper()
		status, _, body := auditTestRequest(t, server, http.MethodGet, path, "", "", login.Token)
		if status != http.StatusOK {
			t.Fatalf("管理查询失败：%d", status)
		}
		return body
	}
	list := func(t *testing.T, eventType, key, value string) api.SecurityAuditEventPage {
		t.Helper()
		params := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}, "eventType": {eventType}, key: {value}}
		return queryDecode[api.SecurityAuditEventPage](t, read(t, securityEventsPath+"?"+params.Encode()))
	}

	t.Run("制品提交立即可查", func(t *testing.T) {
		const path = "docs/验收 sample.txt"
		const content = "Raw 端到端验收。\n"
		artifactURL := "/repository/" + repo.Name + (&url.URL{Path: "/" + path}).EscapedPath()
		status, requestID, _ := auditTestRequest(t, server, http.MethodPut, artifactURL, "application/octet-stream", content, publisher)
		if status != http.StatusCreated || requestID == "" {
			t.Fatalf("普通 Raw 上传未返回成功与请求标识：%d", status)
		}

		// 发布事实与制品共事务提交；首个管理查询必须可见，不等待后台采集。
		const committedType = "asset.change_committed"
		page := list(t, committedType, "requestId", requestID)
		if len(page.Items) != 1 || page.HasMore || page.NextCursor != nil {
			t.Fatalf("该请求应恰有一条已提交事实，实际页长度=%d，另有续页=%v", len(page.Items), page.HasMore)
		}
		event := page.Items[0]
		if event.EventId == "" || event.EventType != committedType || event.Committed != "committed" || event.Outcome != "succeeded" || event.RequestId == nil || *event.RequestId != requestID || event.OperationId == nil || *event.OperationId == "" || event.ActorUserId == nil || *event.ActorUserId != admin.ID || event.RepositoryId == nil || *event.RepositoryId != repo.ID {
			t.Fatal("列表事实未对应发布请求、操作、主体和仓库")
		}
		operationPage := list(t, committedType, "operationId", *event.OperationId)
		if len(operationPage.Items) != 1 || operationPage.HasMore || operationPage.NextCursor != nil || operationPage.Items[0].EventId != event.EventId {
			t.Fatal("同一操作应只有同一条提交事实")
		}

		detailURL := securityEventsPath + "/" + url.PathEscape(event.EventId)
		detailBody := read(t, detailURL)
		detail := queryDecode[api.SecurityAuditEvent](t, detailBody)
		if detail.EventId != event.EventId || detail.EventType != committedType || detail.Committed != "committed" || detail.Outcome != "succeeded" || detail.RequestId == nil || *detail.RequestId != requestID || detail.OperationId == nil || *detail.OperationId != *event.OperationId || detail.RepositoryId == nil || *detail.RepositoryId != repo.ID || !detail.OccurredAt.Equal(event.OccurredAt) || detail.HttpStatus != nil || strings.Contains(detailBody, "\"committedAt\"") || strings.Contains(detailBody, strings.TrimSpace(content)) {
			t.Fatal("详情未保留同一提交事实，或扩展了没有证据的字段")
		}
		objects := queryDecode[api.SecurityAuditEventObjectPage](t, read(t, detailURL+"/objects"))
		if len(objects.Items) != 1 || objects.HasMore || objects.NextCursor != nil {
			t.Fatalf("单制品提交应恰有一个对象，实际=%d", len(objects.Items))
		}
		object := objects.Items[0]
		wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
		if object.Ordinal != 1 || object.ObjectKind != "asset" || object.RepositoryId == nil || *object.RepositoryId != repo.ID || object.CanonicalPath == nil || *object.CanonicalPath != path || object.SizeBytes == nil || *object.SizeBytes != int64(len(content)) || object.Digest == nil || *object.Digest != wantDigest {
			t.Fatalf("对象未匹配仓库、路径、实际字节数 %d 和摘要", len(content))
		}
		status, _, download := auditTestRequest(t, server, http.MethodGet, artifactURL, "", "", "")
		if status != http.StatusOK || download != content {
			t.Fatalf("普通 Raw 下载未读回上传字节：%d，长度=%d", status, len(download))
		}
	})

	t.Run("RawPublishDenied", func(t *testing.T) {
		reader, err := svc.userSvc.Create("vertical-reader", auditTestPassword, "user")
		if err != nil {
			t.Fatal(err)
		}
		readerToken, _, err := svc.tokenSvc.Create(reader.ID, "vertical-reader-token")
		if err != nil {
			t.Fatal(err)
		}
		const content = "Raw 权限拒绝验收正文。"
		artifactURL := "/repository/" + repo.Name + "/docs/denied.txt"
		status, requestID, body := auditTestRequest(t, server, http.MethodPut, artifactURL, "application/octet-stream", content, readerToken)
		if status != http.StatusForbidden || requestID == "" {
			t.Fatalf("无发布权的已认证主体应得到 403 和请求号，实际状态=%d", status)
		}
		rejection := queryDecode[api.Error](t, body)
		if rejection.Error.Code != "forbidden" || rejection.OperationId != nil {
			t.Fatal("原生响应未保留权限拒绝，或错误带有成功操作标识")
		}
		if status, _, _ := auditTestRequest(t, server, http.MethodGet, artifactURL, "", "", ""); status != http.StatusNotFound {
			t.Fatalf("拒绝的目标不应成为公开资产，下载状态=%d", status)
		}

		// 协议拒绝是后台采集，不要求首个列表请求即能读到。
		const deniedType = "protocol.write_denied"
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		page := list(t, deniedType, "requestId", requestID)
		for len(page.Items) == 0 {
			select {
			case <-deadline.C:
				t.Fatal("限定时间内未采集真实 Raw 权限拒绝")
			case <-tick.C:
				page = list(t, deniedType, "requestId", requestID)
			}
		}
		if len(page.Items) != 1 || page.HasMore || page.NextCursor != nil || page.Items[0].EventId == "" {
			t.Fatalf("按请求号查询应恰有一条持久拒绝，页长度=%d", len(page.Items))
		}
		if committed := list(t, "asset.change_committed", "requestId", requestID); len(committed.Items) != 0 || committed.HasMore {
			t.Fatal("拒绝的请求不应产生制品提交事实")
		}
		detailURL := securityEventsPath + "/" + url.PathEscape(page.Items[0].EventId)
		detailBody := read(t, detailURL)
		event := queryDecode[api.SecurityAuditEvent](t, detailBody)
		if event.EventId != page.Items[0].EventId || event.EventType != deniedType || event.RequestId == nil || *event.RequestId != requestID || event.ActorKind != "user" || event.ActorUserId == nil || *event.ActorUserId != reader.ID || event.RepositoryId == nil || *event.RepositoryId != repo.ID || event.ErrorCode == nil || *event.ErrorCode != "forbidden" || event.Outcome != "denied" || event.Committed != "not_committed" || event.OperationId != nil || event.HttpStatus == nil || *event.HttpStatus != http.StatusForbidden {
			t.Fatal("持久拒绝详情未匹配请求、主体、仓库或既定拒绝结果")
		}
		loopback, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if event.ProxyTrust != "direct" || event.ClientIp == nil || *event.ClientIp != loopback.Hostname() || event.PeerIp == nil || *event.PeerIp != loopback.Hostname() || event.ForwardedChain != nil {
			t.Fatal("拒绝事件应使用真实回环连接地址，不采用未经授权的转发链")
		}
		for _, marker := range []string{readerToken, content, `"committedAt"`, `"body"`, `"tokenPreview"`} {
			if strings.Contains(detailBody, marker) {
				t.Fatal("拒绝事件详情含有超出白名单的字段或内容")
			}
		}
		for _, caller := range []struct {
			token string
			want  int
			code  string
		}{{"", http.StatusUnauthorized, "unauthenticated"}, {readerToken, http.StatusForbidden, "forbidden"}} {
			status, _, body := auditTestRequest(t, server, http.MethodGet, detailURL, "", "", caller.token)
			if status != caller.want || queryDecode[api.Error](t, body).Error.Code != caller.code || strings.Contains(body, event.EventId) || strings.Contains(body, requestID) {
				t.Fatalf("无权限调用方不应获知持久拒绝详情，实际状态=%d", status)
			}
		}
	})

	t.Run("真实登录拒绝异步入库", func(t *testing.T) {
		status, requestID, _ := auditTestRequest(t, server, http.MethodPost, "/api/v1/auth/login", "application/json", fmt.Sprintf(`{"username":%q,"password":%q}`, username, "vertical-wrong-test-password"), "")
		if status != http.StatusUnauthorized || requestID == "" {
			t.Fatalf("拒绝响应未携带请求标识：%d", status)
		}
		// 登录尝试由后台采集；只为该精确请求设置有界等待，不将其视作同步提交。
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			page := list(t, "auth.attempt", "requestId", requestID)
			if len(page.Items) > 0 {
				event := page.Items[0]
				if len(page.Items) != 1 || page.HasMore || event.RequestId == nil || *event.RequestId != requestID || event.Outcome != "denied" || event.Committed != "not_applicable" || event.ErrorCode == nil || *event.ErrorCode != "invalid_credentials" || event.ActorUserId != nil || event.HttpStatus != nil {
					t.Fatal("拒绝事件未匹配真实请求，或编造了主体与响应状态")
				}
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("限定时间内未采集真实登录拒绝")
			case <-tick.C:
			}
		}
	})
}
