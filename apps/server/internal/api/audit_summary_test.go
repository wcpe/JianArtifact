package api

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestSafeAuditSummaryIsSpecific 摘要必须说明「做了什么」，而不是一律「操作已完成」。
func TestSafeAuditSummaryIsSpecific(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		event repository.ObservabilityEvent
		want  string
	}{
		{
			name:  "设置变更给出前后值",
			event: repository.ObservabilityEvent{Action: "setting.set", EntityKey: "anonymous_access_enabled", Detail: "true\x1ffalse", Result: "ok"},
			want:  "修改设置 anonymous_access_enabled：true → false",
		},
		{
			name:  "设置首次写入显示未设置",
			event: repository.ObservabilityEvent{Action: "setting.set", EntityKey: "public_url", Detail: "\x1fhttps://x.example", Result: "ok"},
			want:  "修改设置 public_url：未设置 → https://x.example",
		},
		{
			name:  "仓库重命名给出新旧名",
			event: repository.ObservabilityEvent{Action: "repo.rename", EntityKey: "new-name", Detail: "oldName=old-name newName=new-name", Result: "ok"},
			want:  "重命名仓库：old-name → new-name",
		},
		{
			name:  "制品上传点名路径",
			event: repository.ObservabilityEvent{Action: "asset.put", EntityKey: "repo/com/x/1.jar", Result: "ok"},
			want:  "上传制品 repo/com/x/1.jar",
		},
		{
			name:  "制品删除点名路径",
			event: repository.ObservabilityEvent{Action: "asset.delete", EntityKey: "repo/com/x/1.jar", Result: "ok"},
			want:  "删除制品 repo/com/x/1.jar",
		},
		{
			name:  "无法细化时至少按动作类别描述",
			event: repository.ObservabilityEvent{Action: "something.unknown", Result: "ok"},
			want:  "操作已完成",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeAuditSummary(tc.event); got != tc.want {
				t.Errorf("摘要 = %q，期望 %q", got, tc.want)
			}
		})
	}
}
