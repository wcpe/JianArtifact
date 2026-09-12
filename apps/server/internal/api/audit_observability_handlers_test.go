package api

import (
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestNotificationLessPrioritizesSeverityAfterFailure(t *testing.T) {
	now := time.Now().UTC()
	critical := AuditAttentionPreview{AttentionId: "critical", FailureCount: 1, Severity: AuditSeverityCritical, LatestOccurredAt: now.Add(-time.Hour)}
	high := AuditAttentionPreview{AttentionId: "high", FailureCount: 1, Severity: AuditSeverityHigh, LatestOccurredAt: now}
	if !notificationLess(critical, high) {
		t.Fatal("同为失败批次时，critical 必须优先于较新的 high")
	}
	if auditSeverity(repository.ObservabilityEvent{Action: "repository.delete", Result: "ok"}) != AuditSeverityCritical {
		t.Fatal("危险删除必须归为 critical")
	}
}

func TestNotificationOrderingSemantics(t *testing.T) {
	now := time.Now().UTC()
	newerSuccess := AuditAttentionPreview{AttentionId: "b-newer", FailureCount: 0, LatestOccurredAt: now}
	olderFailure := AuditAttentionPreview{AttentionId: "a-older", FailureCount: 2, LatestOccurredAt: now.Add(-time.Hour)}
	// 缺省请求（页眉预览）失败优先；显式请求（消息中心）时间倒序。
	if !notificationLess(olderFailure, newerSuccess) {
		t.Fatal("缺省请求必须失败批次优先，即使其更旧")
	}
	if !notificationTimeLess(newerSuccess, olderFailure) {
		t.Fatal("显式请求必须按最新时间倒序，不因失败置顶")
	}
	tieA := AuditAttentionPreview{AttentionId: "same-1", LatestOccurredAt: now}
	tieB := AuditAttentionPreview{AttentionId: "same-0", LatestOccurredAt: now}
	if !notificationTimeLess(tieA, tieB) {
		t.Fatal("时间相同时必须以 attentionId 倒序打破平局")
	}
}
