package repository

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidSecurityEventPathMatchesAuditRule(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"group/release.bin", true},
		{"测试/制品.jar", true},
		{"", false},
		{"group//release.bin", false},
		// `#` 是制品键的合法文件名字符（Raw PUT 经 %23 解码放行），审计路径规则与之一致。
		{"group/release.bin#preview", true},
		{strings.Repeat("a", 1025), false},
	} {
		if got := ValidSecurityEventPath(tc.path); got != tc.want || got != validSecurityEventPath(tc.path) {
			t.Errorf("共享路径规则对 %q 返回 %t，预期 %t", tc.path, got, tc.want)
		}
	}
}

func TestSecurityEventRevocationCandidateFacts(t *testing.T) {
	actor, repo, otherRepo, size, negative, status := int64(1), int64(2), int64(3), int64(0), int64(-1), 204
	base := SecurityEvent{EventID: "e", OccurredAt: time.Unix(10, 0).UTC(), ActorKind: "user", ActorUserID: &actor,
		ProxyTrust: "unknown", OperationID: "f4f6df7b-a5d9-4f8a-9e90-8e92237cb582", Outcome: "succeeded", Committed: "committed"}
	account := base
	account.EventType = "auth.account_access_changed"
	user := SecurityEventObject{Ordinal: 1, ObjectKind: "user", ObjectID: "2"}
	candidate := base
	candidate.EventType, candidate.RepositoryID = "admission.candidate_staged", &repo
	asset := SecurityEventObject{Ordinal: 1, ObjectKind: "asset", RepositoryID: &repo, CanonicalPath: "group/file", Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: &size}
	for _, tc := range []struct {
		event  SecurityEvent
		object SecurityEventObject
	}{{account, user}, {candidate, asset}} {
		t.Run(tc.event.EventType, func(t *testing.T) {
			if err := validateSecurityEvent(tc.event, []SecurityEventObject{tc.object}); err != nil {
				t.Errorf("有效事实被拒绝：%v", err)
			}
			for _, objects := range [][]SecurityEventObject{nil, {tc.object, tc.object}} {
				if err := validateSecurityEvent(tc.event, objects); !errors.Is(err, ErrInvalidSecurityEvent) {
					t.Errorf("不是唯一明细，得到 %v", err)
				}
			}
			for _, mutate := range []func(*SecurityEvent){
				func(e *SecurityEvent) { e.HTTPStatus = &status },
				func(e *SecurityEvent) { e.Committed = "not_committed" },
				func(e *SecurityEvent) { e.ActorKind, e.ActorUserID = "system", nil },
			} {
				e := tc.event
				mutate(&e)
				if err := validateSecurityEvent(e, []SecurityEventObject{tc.object}); !errors.Is(err, ErrInvalidSecurityEvent) {
					t.Errorf("非法事件头得到 %v", err)
				}
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SecurityEventObject)
	}{
		{"错误仓库", func(o *SecurityEventObject) { o.RepositoryID = &otherRepo }},
		{"无目标仓库", func(o *SecurityEventObject) { o.RepositoryID = nil }},
		{"没有路径", func(o *SecurityEventObject) { o.CanonicalPath = "" }},
		{"不是资产", func(o *SecurityEventObject) { o.ObjectKind = "blob" }},
		{"没有摘要", func(o *SecurityEventObject) { o.Digest = "" }},
		{"非实际 SHA256", func(o *SecurityEventObject) { o.Digest = "sha512:" + strings.Repeat("a", 128) }},
		{"没有字节数", func(o *SecurityEventObject) { o.SizeBytes = nil }},
		{"负数大小", func(o *SecurityEventObject) { o.SizeBytes = &negative }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := asset
			tc.mutate(&o)
			if err := validateSecurityEvent(candidate, []SecurityEventObject{o}); !errors.Is(err, ErrInvalidSecurityEvent) {
				t.Errorf("非法资产明细得到 %v", err)
			}
		})
	}
	user.ObjectID = "02"
	if err := validateSecurityEvent(account, []SecurityEventObject{user}); !errors.Is(err, ErrInvalidSecurityEvent) {
		t.Errorf("非规范用户标识得到 %v", err)
	}
}
