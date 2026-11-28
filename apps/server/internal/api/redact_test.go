package api

import (
	"errors"
	"strings"
	"testing"
)

// TestRedactErrorForLogStripsCredentials 验证进入日志/响应前的错误文本脱敏。
//
// 兜底分支会把 err 原文写进服务端日志与管理员响应，而出站代理的节点解析错误、
// 上游 URL 解析错误都可能让 userinfo 或节点字段进入 err。日志会落到 stdout /
// journald / 容器日志，属于长期留存位置，因此必须在这里抹掉。
func TestRedactErrorForLogStripsCredentials(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		mustNotKeep []string
		mustKeep    []string
	}{
		{
			name:        "YAML 节点字段",
			err:         errors.New("解析节点失败：proxies[0].password: hunter2"),
			mustNotKeep: []string{"hunter2"},
			mustKeep:    []string{"password", "解析节点失败"},
		},
		{
			name:        "JSON 风格键值",
			err:         errors.New(`{"uuid":"1111-2222","secret":"s3cr3t"}`),
			mustNotKeep: []string{"1111-2222", "s3cr3t"},
			mustKeep:    []string{"uuid", "secret"},
		},
		{
			name:        "URL userinfo",
			err:         errors.New("回源失败：http://alice:pa55w0rd@proxy.example:7890/x"),
			mustNotKeep: []string{"alice", "pa55w0rd"},
			mustKeep:    []string{"proxy.example:7890"},
		},
		{
			name:        "Authorization 头",
			err:         errors.New("上游拒绝：Authorization: Bearer abcdef123456"),
			mustNotKeep: []string{"abcdef123456"},
			mustKeep:    []string{"Authorization"},
		},
		{
			name:     "无凭据的错误原样保留",
			err:      errors.New("磁盘权限不足"),
			mustKeep: []string{"磁盘权限不足"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactErrorForLog(tc.err)
			for _, secret := range tc.mustNotKeep {
				if strings.Contains(got, secret) {
					t.Fatalf("脱敏后仍含敏感值 %q：%s", secret, got)
				}
			}
			for _, keep := range tc.mustKeep {
				if !strings.Contains(got, keep) {
					t.Fatalf("脱敏后丢失了可诊断信息 %q：%s", keep, got)
				}
			}
		})
	}
}

// TestRedactErrorForLogHandlesNil 验证 nil error 不会 panic
// （原实现用 %v 直接格式化，改成 %s 后必须自己兜住 nil）。
func TestRedactErrorForLogHandlesNil(t *testing.T) {
	if got := redactErrorForLog(nil); got != "" {
		t.Fatalf("nil error 应返回空串，得 %q", got)
	}
}
