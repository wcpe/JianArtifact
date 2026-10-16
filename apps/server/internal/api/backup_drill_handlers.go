// 恢复演练的 HTTP 边界（FR-40）。
//
// 两个端点承载同一能力的「发起」与「查询」两侧：
//
//	POST /api/v1/backups/{id}/drill           → 202 受理，报告由后台落库
//	GET  /api/v1/backups/{id}/verifications   → 分页读历史报告（含本次演练的结果）
//
// 为什么必须是 202 + 轮询：深校验要逐 blob 读取，耗时与包体积同阶；同步等待会让
// 请求超时，而演练恰恰最需要留下那份"跑完了没有、结论是什么"的记录。
// 与既有 createBackup 同一风格，只是"受理件"从列表里找，这里的"受理件"是稍后出现的报告行。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// auditActionBackupDrill 是发起恢复演练的审计动作名（FR-40）。
//
// 本动作名**为本批新增**：既有备份端点（生成 / 删除 / 校验 / 取链接）此前都没有写审计，
// 因此没有可沿用的 "backup.*" 先例。命名沿用既有 `<实体>.<动作>` 风格；
// entityType 取 backup_package、entityKey 取包标识，与备份列表的主键口径一致。
const auditActionBackupDrill = "backup.drill"

// auditActionBackupRestore 是用本机包发起恢复的审计动作名（FR-40 追加六）。
// 与 backup.drill 一样 entityType 取 backup_package、entityKey 取包标识。
const auditActionBackupRestore = "backup.restore"

// DrillBackup 发起一次恢复演练（仅管理员）。
//
// 三段式：先同步完成"能不能开始"的判定（包存在且已完成、级别合法），再交给后台执行，
// 最后立即返回 202。这样调用方拿得到明确的 404 / 409，而不是一个永远 202、
// 失败原因只藏在日志里的黑盒。
//
// **演练未通过不算接口失败**：包体损坏、SQLite 不可用等结论都会落库为一份 ok=false 的报告，
// 由调用方轮询读到。只有"连演练都无法开始"（包不存在 / 未完成 / 级别非法）才走错误响应。
func (h *Handlers) DrillBackup(c *gin.Context, id BackupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	if h.backupDrill == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "恢复演练服务未启用")
		return
	}

	// 请求体可整体省略（缺省浅校验），因此只在确实带了体时才解析——
	// 空体走 bindJSON 会因 EOF 被误判成 400。
	level := repository.VerificationLevelShallow
	if c.Request.ContentLength > 0 {
		var req DrillBackupRequest
		if !bindJSON(c, &req) {
			return
		}
		if req.Level != nil {
			level = string(*req.Level)
			if level != repository.VerificationLevelShallow && level != repository.VerificationLevelDeep {
				auth.WriteError(c, http.StatusBadRequest, "bad_request",
					fmt.Sprintf("演练级别只能是 %s 或 %s，实际 %q",
						repository.VerificationLevelShallow, repository.VerificationLevelDeep, level))
				return
			}
		}
	}

	// 同步预检：把"包不存在 / 尚未生成完成"在受理前判掉。
	// 演练服务内部同样会校验一次——这里不是重复，而是把不可受理的请求挡在排队之前，
	// 避免调用方拿到 202 之后才发现包根本没生成好。
	rec, err := h.backups.Get(id)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	if rec.Status != repository.BackupStatusDone {
		writeBackupErr(c, fmt.Errorf("%w：%s 当前状态 %s", domain.ErrDrillPackageNotDone, id, rec.Status))
		return
	}

	pkgID := id
	acceptedAt := time.Now().UTC()

	// 后台执行：用 WithoutCancel 与请求生命周期解耦。调用方拿到 202 就会断开连接，
	// 若沿用请求上下文，演练会在第一处 ctx 检查点被取消——而它已经读了半个包。
	// 上下文里的值（主体等）仍随 WithoutCancel 保留，便于服务内日志与后续扩展。
	bgCtx := context.WithoutCancel(c.Request.Context())
	go func() {
		result, err := h.backupDrill.Drill(bgCtx, pkgID, domain.DrillBackupOptions{Level: level})
		if err != nil {
			// 走到这里的错误是"演练没能开始或报告写不进去"，不含包本身损坏
			// （后者是 ok=false 的正常报告）。记日志供运维定位，不改已发出的 202。
			log.Printf("恢复演练：包 %s 未能完成：%v", pkgID, err)
			return
		}
		log.Printf("恢复演练完成：包 %s，级别 %s，结论 %s，耗时 %dms（报告 %d）",
			result.PackageID, result.Level, drillVerdictText(result.OK), result.Duration.Milliseconds(),
			result.Verification.ID)
	}()

	h.AuditLog(c, auditActionBackupDrill, "backup_package", pkgID, "", "level="+level, "ok")
	c.JSON(http.StatusAccepted, BackupDrillAccepted{
		PackageId:  pkgID,
		Level:      BackupVerificationLevel(level),
		AcceptedAt: acceptedAt.Format(time.RFC3339Nano),
	})
}

// ListBackupVerifications 分页返回某包的历史演练报告（最近优先，仅管理员）。
//
// 这是演练的查询面：drill 受理后从这里轮询新报告。包不存在直接 404——
// 对一个不存在的包返回空列表会让调用方把"包没了"误读成"还没演练过"。
func (h *Handlers) ListBackupVerifications(c *gin.Context, id BackupIdParam, params ListBackupVerificationsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupDrill == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "恢复演练服务未启用")
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	items, total, err := h.backupDrill.ListVerifications(id, limit, offset)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	out := BackupVerificationList{Items: make([]BackupVerificationReport, 0, len(items)), Total: total}
	for _, item := range items {
		out.Items = append(out.Items, toAPIBackupVerificationReport(item))
	}
	c.JSON(http.StatusOK, out)
}

// toAPIBackupVerificationReport 把报告行模型映射为契约类型。
func toAPIBackupVerificationReport(rec repository.BackupVerification) BackupVerificationReport {
	out := BackupVerificationReport{
		Id:         rec.ID,
		PackageId:  rec.PackageID,
		Level:      BackupVerificationLevel(rec.Level),
		Ok:         rec.OK,
		StartedAt:  rec.StartedAt,
		FinishedAt: rec.FinishedAt,
		DurationMs: rec.DurationMS,
		Checks:     parseDrillChecks(rec.ChecksJSON),
	}
	if rec.ErrorSummary != "" {
		out.ErrorSummary = &rec.ErrorSummary
	}
	return out
}

// parseDrillChecks 把库内的逐项结论 JSON 转成契约类型。
//
// 解析失败 / 空值一律返回**空数组**而不是 nil：checks 在契约里是 required，
// 前端也不必为 null 单独分支。这里刻意不把解析失败升级成错误——一份结论列缺失的
// 报告，其总判定与失败摘要仍然有查证价值，为了一个附加列把整条历史挡住得不偿失。
func parseDrillChecks(checksJSON string) []BackupDrillCheck {
	trimmed := strings.TrimSpace(checksJSON)
	if trimmed == "" {
		return []BackupDrillCheck{}
	}
	var raw []domain.DrillCheck
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return []BackupDrillCheck{}
	}
	out := make([]BackupDrillCheck, 0, len(raw))
	for _, check := range raw {
		item := BackupDrillCheck{Name: check.Name, Ok: check.OK, Detail: check.Detail}
		if check.DurationMS > 0 {
			duration := check.DurationMS
			item.DurationMs = &duration
		}
		out = append(out, item)
	}
	return out
}

// drillVerdictText 是演练结论的中文短语（仅用于日志）。
func drillVerdictText(ok bool) string {
	if ok {
		return "通过"
	}
	return "未通过"
}
