// 分片上传（FR-137 第三通道）的 HTTP 边界。
//
// 五端点：发起会话 → 逐片上传（octet-stream）→ 查询会话（续传用）→ 拼装完成 → 取消。
// 全部仅管理员可见。错误映射复用 backup_import 的冲突/404 词表：target_not_empty /
// restore_pending / import_in_progress 都是 409，其余走通用领域错误映射。
package api

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// CreateBackupUpload 发起分片上传会话（仅管理员）。
//
// 返回服务端决定的分片大小（chunkSize）；前端据此切分。sha256 可选，拼装时核对。
func (h *Handlers) CreateBackupUpload(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.backupUploads == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上传服务未启用")
		return
	}
	var req CreateBackupUploadRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := h.backupUploads.Init(c.Request.Context(), domain.InitUploadOptions{
		FileName:   req.FileName,
		TotalBytes: req.TotalBytes,
		SHA256:     derefString(req.Sha256),
		Operator:   principal.Username,
	})
	if err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	// 新会话尚未落任何分片。
	c.JSON(http.StatusCreated, toAPIBackupUploadSession(res.Upload, res.ChunkSize, nil))
}

// UploadBackupChunk 上传单个分片（仅管理员）。
//
// 请求体为原始分片字节（application/octet-stream）。index 由生成的中间件解析为 int，
// 非法值已由中间件判为 400。单片体积用 http.MaxBytesReader 按 chunkSize 兜底，
// 超界直接 400，避免超大请求打爆内存/磁盘；同样正数交给服务层做序号/累计护栏。
func (h *Handlers) UploadBackupChunk(c *gin.Context, id BackupUploadIdParam, index BackupUploadChunkIndexParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupUploads == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上传服务未启用")
		return
	}
	view, err := h.backupUploads.Get(c.Request.Context(), id)
	if err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	chunkSize := view.ChunkSize

	// 按分片上限兜底：超过 chunkSize 的字节视为非法超额分片。
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, chunkSize)
	data, rerr := io.ReadAll(limited)
	if rerr != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request",
			"分片体积超过上限 "+strconv.FormatInt(chunkSize, 10)+" 字节")
		return
	}
	if len(data) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "分片体为空")
		return
	}

	if _, err := h.backupUploads.PutChunk(c.Request.Context(), id, int(index), bytes.NewReader(data), int64(len(data))); err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	// 回当前会话（含已落盘分片），便于前端推进进度。
	view, err = h.backupUploads.Get(c.Request.Context(), id)
	if err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIBackupUploadSession(view.Upload, view.ChunkSize, view.UploadedChunks))
}

// GetBackupUpload 查询上传会话（续传用，仅管理员）。
// uploadedChunks 由磁盘上真实存在的分片推导——前端据此知道还缺哪些片。
func (h *Handlers) GetBackupUpload(c *gin.Context, id BackupUploadIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupUploads == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上传服务未启用")
		return
	}
	view, err := h.backupUploads.Get(c.Request.Context(), id)
	if err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIBackupUploadSession(view.Upload, view.ChunkSize, view.UploadedChunks))
}

// CompleteBackupUpload 拼装归档并触发本地导入（仅管理员）。
// body 可空（仅 sha256/overwrite/deep，都可选）。成功返回 202 + 一条 queued 导入记录，需轮询看进度。
func (h *Handlers) CompleteBackupUpload(c *gin.Context, id BackupUploadIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupUploads == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上传服务未启用")
		return
	}
	var req CompleteBackupUploadRequest
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &req) {
			return
		}
	}
	imp, err := h.backupUploads.Complete(c.Request.Context(), id, derefString(req.Sha256),
		req.Overwrite != nil && *req.Overwrite, req.Deep != nil && *req.Deep)
	if err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, toAPIBackupImport(imp))
}

// AbortBackupUpload 取消上传会话（仅管理员）：置 aborted 并清磁盘。
func (h *Handlers) AbortBackupUpload(c *gin.Context, id BackupUploadIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupUploads == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上传服务未启用")
		return
	}
	if err := h.backupUploads.Abort(c.Request.Context(), id); err != nil {
		writeBackupUploadErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// toAPIBackupUploadSession 把会话记录映射为契约视图；uploadedChunks 为空时给空切片而非 null。
func toAPIBackupUploadSession(rec repository.BackupUpload, chunkSize int64, uploadedChunks []int) BackupUploadSession {
	chunks := uploadedChunks
	if chunks == nil {
		chunks = []int{}
	}
	return BackupUploadSession{
		UploadId:       rec.UploadID,
		FileName:       rec.FileName,
		TotalBytes:     rec.TotalBytes,
		ChunkSize:      chunkSize,
		UploadedChunks: chunks,
		Status:         BackupUploadStatus(rec.Status),
		ExpiresAt:      parseExpiresAt(rec.ExpiresAt),
	}
}

// parseExpiresAt 把 RFC3339Nano 字符串解析为 time.Time；格式异常时回零值（不应发生）。
func parseExpiresAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// writeBackupUploadErr 把上传相关错误映射为明确状态码，复用导入的冲突/404 词表。
func writeBackupUploadErr(c *gin.Context, err error) {
	writeBackupImportErr(c, err)
}
