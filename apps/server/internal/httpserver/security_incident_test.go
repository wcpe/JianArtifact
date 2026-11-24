// FR-168：事故处置动作生命周期与全实例业务写锁。
package httpserver_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func TestIncidentInstanceWriteLockEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, secret, _ := installAdmin(t, e, "admin", "admin-pass-123")
	vis := api.CreateRepositoryRequestVisibility("private")

	// 预览 restrict(instance_write_lock) → actionId + 摘要。
	var prev api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "incident-test", IdempotencyKey: "lock-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &prev); code != http.StatusOK || prev.ActionDigest == "" {
		t.Fatalf("预览状态码 = %d，期望 200 且返回摘要", code)
	}

	// 幂等回放：同键同摘要返回同一动作。
	var prev2 api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "incident-test", IdempotencyKey: "lock-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &prev2); code != http.StatusOK || prev2.ActionId != prev.ActionId {
		t.Fatalf("幂等回放状态码 = %d，期望 200 且同 actionId", code)
	}

	// 摘要漂移执行 → 409。
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: "tampered"}, &bad); code != http.StatusConflict {
		t.Fatalf("漂移执行状态码 = %d，期望 409", code)
	}

	// 正常执行（安装登录会话带近期强认证水位）→ applied。
	var res api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK || res.State != "applied" || len(res.Items) != 1 || res.Items[0].Outcome != "applied" {
		t.Fatalf("执行状态码 = %d 状态 = %q，期望 applied", code, res.State)
	}

	// 业务写锁生效：建仓库写 → 503 incident_write_locked；安全出口仍可用（GET 200）。
	var bad2 map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "locked-repo", Type: "hosted", Format: "raw", Visibility: &vis}, &bad2); code != http.StatusServiceUnavailable {
		t.Fatalf("锁定期业务写状态码 = %d，期望 503", code)
	}
	if bad2["error"].(map[string]any)["code"] != "incident_write_locked" {
		t.Fatalf("错误码 = %v，期望 incident_write_locked", bad2["error"])
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/users", token, nil, &bad2); code != http.StatusOK {
		t.Fatalf("锁定期只读查询状态码 = %d，期望 200", code)
	}

	// 解除限制：lift_restriction 预览+执行。
	var lp api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.LiftRestriction, Reason: "incident-cleared", IdempotencyKey: "lift-1",
			Targets: []api.SecurityActionTarget{{Kind: "instance_write_lock"}},
		}, &lp); code != http.StatusOK {
		t.Fatalf("解除预览状态码 = %d，期望 200", code)
	}
	var lr api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+lp.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: lp.ActionDigest}, &lr); code != http.StatusOK || lr.State != "applied" {
		t.Fatalf("解除执行状态码 = %d 状态 = %q，期望 applied", code, lr.State)
	}

	// 解闸后业务写恢复。
	var repo api.Repository
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "unlocked-repo", Type: "hosted", Format: "raw", Visibility: &vis}, &repo); code != http.StatusCreated {
		t.Fatalf("解闸后业务写状态码 = %d，期望 201", code)
	}

	// 过期拒绝：伪造过期动作不可用——以消费后终态复执代替（动作已终结）。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &bad2); code != http.StatusConflict {
		t.Fatalf("终态复执状态码 = %d，期望 409", code)
	}
	_ = secret
}

func TestIncidentRepositoryRestrictionEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "admin", "admin-pass-123")
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", token,
		api.CreateTokenRequest{Name: "incident-test"}, &created); code != http.StatusCreated {
		t.Fatalf("创建 Token 状态码 = %d，期望 201", code)
	}
	apiToken := created.Token

	// 建两个仓库：受限 raw-locked 与对照 raw-open。
	for _, name := range []string{"raw-locked", "raw-open"} {
		e.createRawRepo(t, token, name, "private")
	}

	// 对 raw-locked 加 repository 限制。
	var prev api.SecurityActionPreview
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
		api.SecurityActionPreviewRequest{
			ActionType: api.Restrict, Reason: "repo-incident", IdempotencyKey: "repo-1",
			Targets: []api.SecurityActionTarget{{Kind: "repository", Target: strPtr("raw-locked")}},
		}, &prev); code != http.StatusOK {
		t.Fatalf("仓库限制预览状态码 = %d，期望 200", code)
	}
	var res api.SecurityActionResult
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
		api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK || res.State != "applied" {
		t.Fatalf("仓库限制执行状态码 = %d 状态 = %q，期望 applied", code, res.State)
	}

	// 受限仓写入 → 403 incident_restricted；对照仓正常。
	rec := e.rawReq(http.MethodPut, "/repository/raw-locked/a.bin", basicHeader(apiToken), "application/octet-stream", []byte("x"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("受限仓写状态码 = %d，期望 403", rec.Code)
	}
	rec = e.rawReq(http.MethodPut, "/repository/raw-open/a.bin", basicHeader(apiToken), "application/octet-stream", []byte("x"))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("对照仓写状态码 = %d，期望 2xx", rec.Code)
	}
}

func TestIncidentEvidenceHoldEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "admin", "admin-pass-123")

	runAction := func(actionType api.SecurityActionPreviewRequestActionType, key string, targets []api.SecurityActionTarget) api.SecurityActionResult {
		t.Helper()
		var prev api.SecurityActionPreview
		if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions", token,
			api.SecurityActionPreviewRequest{ActionType: actionType, Reason: "e2e", IdempotencyKey: key, Targets: targets}, &prev); code != http.StatusOK {
			t.Fatalf("预览状态码 = %d，期望 200", code)
		}
		var res api.SecurityActionResult
		if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/actions/"+prev.ActionId+"/execute", token,
			api.SecurityActionExecuteRequest{ActionDigest: prev.ActionDigest}, &res); code != http.StatusOK {
			t.Fatalf("执行状态码 = %d，期望 200", code)
		}
		return res
	}

	// 保全两枚摘要；重复摘要第二动作标 skipped。
	r1 := runAction(api.HoldEvidence, "hold-1", []api.SecurityActionTarget{{Kind: "digest", Target: strPtr("sha256:aaa")}})
	if r1.State != "applied" {
		t.Fatalf("保全状态 = %q，期望 applied", r1.State)
	}
	r2 := runAction(api.HoldEvidence, "hold-2", []api.SecurityActionTarget{{Kind: "digest", Target: strPtr("sha256:aaa")}})
	if r2.Items[0].Outcome != "skipped" {
		t.Fatalf("重复摘要结果 = %q，期望 skipped", r2.Items[0].Outcome)
	}

	// 清单含活跃保全。
	var list struct {
		Items []struct {
			HoldID    string `json:"holdId"`
			Digest    string `json:"digest"`
			ByteState string `json:"byteState"`
			State     string `json:"state"`
		} `json:"items"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/holds", token, nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ByteState != "metadata_only" {
		t.Fatalf("清单状态码 = %d 项数 = %d，期望 200 且 1 枚 metadata_only", code, len(list.Items))
	}

	// 释放后清单清空。
	r3 := runAction(api.ReleaseHold, "rel-1", []api.SecurityActionTarget{{Kind: "hold", Target: &list.Items[0].HoldID}})
	if r3.State != "applied" {
		t.Fatalf("释放状态 = %q，期望 applied", r3.State)
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/holds", token, nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].State != "released" {
		t.Fatalf("释放后清单状态 = %v，期望 released", list.Items)
	}
}

func strPtr(s string) *string { return &s }

func TestIncidentRecoveryGateEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "admin", "admin-pass-123")
	vis := api.CreateRepositoryRequestVisibility("private")

	// 恢复启动 → 隔离；业务写 503 recovery_isolated，安全出口（本命名空间）仍通。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/recovery/begin", token,
		map[string]any{"restoreId": "rst-1", "reason": "dr-restore"}, nil); code != http.StatusNoContent {
		t.Fatalf("进入隔离状态码 = %d，期望 204", code)
	}
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "r", Type: "hosted", Format: "raw", Visibility: &vis}, &bad); code != http.StatusServiceUnavailable {
		t.Fatalf("隔离期业务写状态码 = %d，期望 503", code)
	}
	var gate map[string]any
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/recovery", token, nil, &gate); code != http.StatusOK || gate["state"] != "isolated" {
		t.Fatalf("闸状态 = %v，期望 isolated", gate["state"])
	}

	// 低水位检查点 → 回 isolated 且标 stale（库内当前水位 0，报告 -1 等效低于；用 0 vs 后续抬高更真）。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/recovery/report", token,
		map[string]any{"restoreId": "rst-1", "checkpointStatus": "missing", "verifiedWatermark": 0}, nil); code != http.StatusNoContent {
		t.Fatalf("上报状态码 = %d，期望 204", code)
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/recovery", token, nil, &gate); code != http.StatusOK || gate["state"] != "isolated" || gate["gapStatus"] != "unknown" {
		t.Fatalf("缺失检查点闸状态 = %v，期望 isolated+unknown", gate["state"])
	}

	// 可信且水位齐 → ready_for_review。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/recovery/report", token,
		map[string]any{"restoreId": "rst-1", "checkpointStatus": "verified", "verifiedWatermark": 0}, nil); code != http.StatusNoContent {
		t.Fatalf("可信上报状态码 = %d，期望 204", code)
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/recovery", token, nil, &gate); code != http.StatusOK || gate["state"] != "ready_for_review" {
		t.Fatalf("核对后闸状态 = %v，期望 ready_for_review", gate["state"])
	}

	// 解闸需近期强认证（安装会话水位新鲜）→ normal；业务写恢复。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/recovery/confirm", token,
		map[string]any{"restoreId": "rst-1"}, nil); code != http.StatusNoContent {
		t.Fatalf("解闸状态码 = %d，期望 204", code)
	}
	var repo api.Repository
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token,
		api.CreateRepositoryRequest{Name: "post-restore", Type: "hosted", Format: "raw", Visibility: &vis}, &repo); code != http.StatusCreated {
		t.Fatalf("解闸后业务写状态码 = %d，期望 201", code)
	}
}

// FR-171/172：建案触发站内通知 → 游标分页 → 幂等确认。
func TestSecurityNotificationCursorEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "adm-n", "admin-pass-123")

	// 建案（同事务发射站内条目）。
	var opened api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases", token,
		map[string]any{"title": "可疑发布链", "severity": "high", "reason": "重复拒绝上传"}, &opened); code != http.StatusOK || opened.CaseId == "" {
		t.Fatalf("建案状态码 = %d，期望 200", code)
	}

	// 首开：含建案通知与水位。
	var page struct {
		Items []struct {
			NoticeId string `json:"noticeId"`
			Kind     string `json:"kind"`
			Seq      int64  `json:"seq"`
			Acked    bool   `json:"acked"`
		} `json:"items"`
		NextCursor *int64 `json:"nextCursor"`
		Watermark  int64  `json:"watermark"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/notifications", token, nil, &page); code != http.StatusOK {
		t.Fatalf("首开状态码 = %d", code)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != "security.case_opened" || page.Watermark < 1 {
		t.Fatalf("首开条目不符：%+v", page)
	}

	// 幂等确认 → acked；重复确认仍 200。
	var ack map[string]int64
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/security/notifications/"+page.Items[0].NoticeId+"/ack", token, nil, &ack); code != http.StatusOK {
		t.Fatalf("确认状态码 = %d", code)
	}
	var page2 struct {
		Items []struct {
			Acked bool `json:"acked"`
		} `json:"items"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/notifications", token, nil, &page2); code != http.StatusOK || !page2.Items[0].Acked {
		t.Fatalf("确认后 acked 应为 true：%+v", page2)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/security/notifications/nonexistent/ack", token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("不可见确认应 404，实际 %d", code)
	}

	// 命中清单端点可用（此处无规则命中 → 空列表）。
	var findings []map[string]any
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/findings", token, nil, &findings); code != http.StatusOK {
		t.Fatalf("命中清单状態码 = %d，期望 200", code)
	}

	// 案件转态：claim → close。
	var claimed api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases/"+opened.CaseId+"/transitions", token,
		map[string]any{"action": "claim", "targetVersion": opened.TargetVersion}, &claimed); code != http.StatusOK || claimed.State != "claimed" {
		t.Fatalf("认领状态码 = %d", code)
	}
	var closed api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases/"+opened.CaseId+"/transitions", token,
		map[string]any{"action": "close", "targetVersion": claimed.TargetVersion, "reason": "已处置", "resolutionCode": "resolved"}, &closed); code != http.StatusOK || closed.State != "closed" {
		t.Fatalf("结案状态码 = %d", code)
	}
	// 旧版本重放 → 409。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases/"+opened.CaseId+"/transitions", token,
		map[string]any{"action": "reopen", "targetVersion": opened.TargetVersion, "reason": "复查"}, nil); code != http.StatusConflict {
		t.Fatalf("过期版本重放应 409，实际 %d", code)
	}
}

// FR-172：建案 → 管理员メール宛 smtp 投递行（脱敏）→ 派发结算。
func TestSecurityNotificationSmtpDeliveryEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "adm-d", "admin-pass-123")
	if _, err := e.db.Exec(`UPDATE user SET email='admin@example.com' WHERE username='adm-d'`); err != nil {
		t.Fatalf("設定メール：%v", err)
	}

	var opened api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases", token,
		map[string]any{"title": "配送テスト案件", "severity": "high", "reason": "テスト"}, &opened); code != http.StatusOK {
		t.Fatalf("建案状態码 = %d", code)
	}

	// 投递登记：pending・脱敏宛先（実メールが出ないこと）。
	var dels []struct {
		DeliveryID   string `json:"deliveryId"`
		Channel      string `json:"channel"`
		TargetMasked string `json:"targetMasked"`
		State        string `json:"state"`
		Attempts     int    `json:"attempts"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/notification-deliveries", token, nil, &dels); code != http.StatusOK || len(dels) != 1 {
		t.Fatalf("投递清单状態码 = %d 件数 = %d，期望 200・1 件", code, len(dels))
	}
	d := dels[0]
	if d.Channel != "smtp" || d.State != "pending" || d.TargetMasked == "admin@example.com" || !strings.HasSuffix(d.TargetMasked, "@example.com") {
		t.Fatalf("投递行異常: %+v", d)
	}

	// 配送失敗 → 仍 pending・attempts=1・退避で再試行予約。
	if n, err := e.incidentSvc.DispatchPendingNotifications(t.Context(), func(ctx context.Context, to, subject, text string) error {
		return fmt.Errorf("dial tcp: connection refused")
	}, nil); err != nil || n != 1 {
		t.Fatalf("失敗派发 = %d, %v，期望 1 件", n, err)
	}
	row := struct {
		State     string `db:"state"`
		Attempts  int    `db:"attempts"`
		NextRetry int64  `db:"next_retry_unix"`
		LastErr   string `db:"last_error"`
	}{}
	if err := e.db.Get(&row, `SELECT state,attempts,next_retry_unix,last_error FROM security_notification_delivery WHERE delivery_id=?`, d.DeliveryID); err != nil {
		t.Fatalf("投递行読取：%v", err)
	}
	if row.State != "pending" || row.Attempts != 1 || row.NextRetry <= time.Now().Unix() || !strings.Contains(row.LastErr, "refused") {
		t.Fatalf("失敗结算異常: %+v", row)
	}

	// 再試行予約を現在に戻して成功送信 → accepted 終态、実宛先で送信されたこと。
	if _, err := e.db.Exec(`UPDATE security_notification_delivery SET next_retry_unix=0 WHERE delivery_id=?`, d.DeliveryID); err != nil {
		t.Fatalf("再試行予約：%v", err)
	}
	var gotTo string
	if n, err := e.incidentSvc.DispatchPendingNotifications(t.Context(), func(ctx context.Context, to, subject, text string) error {
		gotTo = to
		return nil
	}, nil); err != nil || n != 1 || gotTo != "admin@example.com" {
		t.Fatalf("成功派发 = %d to=%q err=%v", n, gotTo, err)
	}
	if err := e.db.Get(&row, `SELECT state,attempts,next_retry_unix,last_error FROM security_notification_delivery WHERE delivery_id=?`, d.DeliveryID); err != nil {
		t.Fatalf("終態読取：%v", err)
	}
	if row.State != "accepted" {
		t.Fatalf("終態 = %q，期望 accepted", row.State)
	}

	// SMTP 未設定(nil mailer)で新規投递は failed で可視化（成功を偽装しない）。
	var opened2 api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases", token,
		map[string]any{"title": "未設定案件", "severity": "low", "reason": "テスト2"}, &opened2); code != http.StatusOK {
		t.Fatalf("建案2状態码 = %d", code)
	}
	if n, err := e.incidentSvc.DispatchPendingNotifications(t.Context(), nil, nil); err != nil || n != 1 {
		t.Fatalf("未設定派发 = %d, %v", n, err)
	}
	var st string
	if err := e.db.Get(&st, `SELECT state FROM security_notification_delivery WHERE delivery_id!=? ORDER BY created_unix DESC LIMIT 1`, d.DeliveryID); err != nil || st != "failed" {
		t.Fatalf("未設定終態 = %q err=%v，期望 failed", st, err)
	}
}

// FR-171/172：案件既読水位（同版本冪等・超版本 409）と失败投递的手動再試行。
func TestSecurityCaseReadAndDeliveryRetry(t *testing.T) {
	e := newProtocolEnv(t)
	token, _, _ := installAdmin(t, e, "adm-r", "admin-pass-123")
	if _, err := e.db.Exec(`UPDATE user SET email='r@example.com' WHERE username='adm-r'`); err != nil {
		t.Fatal(err)
	}

	var opened api.SecurityCase
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/cases", token,
		map[string]any{"title": "既読案件", "severity": "medium", "reason": "テスト"}, &opened); code != http.StatusOK {
		t.Fatalf("建案状態码 = %d", code)
	}

	// 既読：同一版本重放冪等。
	var read1 map[string]any
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/security/cases/"+opened.CaseId+"/read", token,
		map[string]any{"seenVersion": 1}, &read1); code != http.StatusOK {
		t.Fatalf("既読状態码 = %d，期望 200", code)
	}
	var read2 map[string]any
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/security/cases/"+opened.CaseId+"/read", token,
		map[string]any{"seenVersion": 1}, &read2); code != http.StatusOK || read2["seenVersion"] != read1["seenVersion"] {
		t.Fatalf("既読重放 = %d %+v", code, read2)
	}
	// 超現版本 → 409。
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/security/cases/"+opened.CaseId+"/read", token,
		map[string]any{"seenVersion": 99}, nil); code != http.StatusConflict {
		t.Fatalf("超版本既読 = %d，期望 409", code)
	}

	// 未設定 SMTP で failed 化 → 手動再試行で pending に戻る（attempts CAS）。
	if _, err := e.incidentSvc.DispatchPendingNotifications(t.Context(), nil, nil); err != nil {
		t.Fatalf("派发：%v", err)
	}
	var dels []struct {
		DeliveryID string `json:"deliveryId"`
		State      string `json:"state"`
		Attempts   int    `json:"attempts"`
	}
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/security/notification-deliveries", token, nil, &dels); code != http.StatusOK || len(dels) != 1 || dels[0].State != "failed" {
		t.Fatalf("投递 = %d %+v，期望 1 件 failed", code, dels)
	}
	var retried map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/notification-deliveries/"+dels[0].DeliveryID+"/retry", token,
		map[string]any{"targetVersion": dels[0].Attempts}, &retried); code != http.StatusOK || retried["state"] != "pending" {
		t.Fatalf("再試行 = %d %+v，期望 pending", code, retried)
	}
	// 漂移 attempts → 409。
	// 同一 targetVersion 重放は冪等（pending のまま 200）。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/notification-deliveries/"+dels[0].DeliveryID+"/retry", token,
		map[string]any{"targetVersion": dels[0].Attempts}, nil); code != http.StatusOK {
		t.Fatalf("冪等重放 = %d，期望 200", code)
	}
	// 未曾一致した版本 → 409。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/security/notification-deliveries/"+dels[0].DeliveryID+"/retry", token,
		map[string]any{"targetVersion": 42}, nil); code != http.StatusConflict {
		t.Fatalf("漂移再試行 = %d，期望 409", code)
	}
}
