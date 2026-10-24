package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 存储体检端点（仅管理员，**非契约端点**）。
//
// 为什么按 /api/v1/search/index 的同一先例不进 api/openapi.yaml：它服务运维手动排障，
// 返回的是「此刻磁盘上可回收的东西有多少」，形状会随清理能力演进而变，不属于协议客户端
// 依赖的稳定面。写进契约会为一份易变的运维快照引入对外承诺。
//
// 为什么只读、且**不得被轮询**：一次体检要遍历 blob 目录与若干暂存目录，成本随数据规模线性
// 增长；更重要的是它**不清理任何东西**——仓库删除与 blob 回收都是不可逆的运维决策，
// 自动清理会把一次误判直接变成数据丢失。清理动作仍交给既有作业按明确间隔执行。

// storageHealthBudget 是体检单次调用的服务端预算。
//
// 与搜索索引状态端点同量级：常规实例是秒级，真正跑飞的目录遍历不应把连接与调用方一起拖住。
const storageHealthBudget = 15 * time.Second

// GetStorageHealth 返回存储体检快照（仅管理员）。
//
// 不写审计：本端点只统计与返回磁盘现状，不删除、不移动、不改任何状态，是纯只读快照。
// 只读操作进审计会把审计变成访问日志，真正需要追责的清理动作反而被淹没；口径与负缓存
// 查看端点、清理预览端点一致——只有写动作才留痕。
//
// 单项统计失败不会让整份报告不可用：响应仍返回已统计到的项，并把失败说明放在 error 字段，
// 因为「看到一个有缺项的全貌」比「什么都看不到」更接近排障需要。
func (h *Handlers) GetStorageHealth(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.storageHealth == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "not_ready", "存储体检未装配")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), storageHealthBudget)
	defer cancel()
	type result struct {
		health domain.StorageHealth
		err    error
	}
	done := make(chan result, 1)
	go func() {
		health, err := h.storageHealth.Snapshot()
		done <- result{health: health, err: err}
	}()
	select {
	case r := <-done:
		body := gin.H{
			"orphanBlobs":        r.health.OrphanBlobs,
			"orphanBlobBytes":    r.health.OrphanBlobBytes,
			"quarantinedBlobs":   r.health.QuarantinedBlobs,
			"quarantinedBytes":   r.health.QuarantinedBytes,
			"emptyDirs":          r.health.EmptyDirs,
			"preRestoreDirs":     r.health.PreRestoreDirs,
			"preRestoreBytes":    r.health.PreRestoreBytes,
			"backupUploadsBytes": r.health.BackupUploadsBytes,
			"backupImportsBytes": r.health.BackupImportsBytes,
			"backupDrillBytes":   r.health.BackupDrillBytes,
			"totalBytes":         r.health.TotalBytes,
		}
		if r.err != nil {
			// 部分失败如实标注，不粉饰成「一切正常」。
			body["partialError"] = r.err.Error()
		}
		c.JSON(http.StatusOK, body)
	case <-ctx.Done():
		// 与搜索索引端点同口径：客户端断开不写响应体；服务端预算耗尽则如实回 504。
		if c.Request.Context().Err() != nil {
			return
		}
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error": gin.H{"code": "storage_health_timeout", "message": "存储体检超时，请稍后重试"},
		})
	}
}
