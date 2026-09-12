package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

const maxOCIRequestManifestBytes = 32 << 20

const defaultOCIUploadTTL = time.Hour

type ociUploadSession struct {
	filePath  string
	repo      string
	image     string
	createdAt time.Time
	timer     *time.Timer
	closed    bool
	mu        sync.Mutex
	size      int64
	limit     int64
	settle    func(bool, int64)
	userID    int64
}

// OCIHandler 适配 OCI Distribution v2 的 manifest、tag、blob 与分块上传。
type OCIHandler struct {
	*RawHandler
	oci       *domain.OCIService
	sessions  sync.Map
	uploadTTL time.Duration
}

// NewOCIHandler 构造 OCI 协议处理器。
func NewOCIHandler(raw *RawHandler, oci *domain.OCIService) *OCIHandler {
	return &OCIHandler{RawHandler: raw, oci: oci, uploadTTL: defaultOCIUploadTTL}
}

// RegisterOCIRoutes 注册 OCI Distribution v2 端点。
func RegisterOCIRoutes(r gin.IRouter, h *OCIHandler, mw ...gin.HandlerFunc) {
	grp := r.Group("/v2", mw...)
	grp.GET("/", h.Probe)
	grp.GET("/:repo/*rest", h.Get)
	grp.HEAD("/:repo/*rest", h.Get)
	grp.POST("/:repo/*rest", h.Post)
	grp.PATCH("/:repo/*rest", h.Patch)
	grp.PUT("/:repo/*rest", h.Put)
}

// Probe 返回 Distribution v2 能力探测响应。
func (h *OCIHandler) Probe(c *gin.Context) {
	if _, ok := auth.PrincipalFrom(c); !ok {
		writeOCIUnauthorized(c)
		return
	}
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.Status(http.StatusOK)
}

// Get 处理 manifest、blob 和 tag 列表读取。
func (h *OCIHandler) Get(c *gin.Context) {
	repo, image, kind, value, ok := parseOCIPath(c.Param("repo"), c.Param("rest"))
	if !ok {
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "OCI 路径不存在")
		return
	}
	if !h.authorize(c, repo, "read") {
		return
	}
	switch kind {
	case "manifest":
		h.getManifest(c, repo, image, value)
	case "blob":
		h.getBlob(c, repo, image, value)
	case "tags":
		h.getTags(c, repo, image)
	default:
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "OCI 路径不存在")
	}
}

// Post 创建或单请求完成一个 blob 上传会话。
func (h *OCIHandler) Post(c *gin.Context) {
	repo, image, kind, _, ok := parseOCIPath(c.Param("repo"), c.Param("rest"))
	if !ok || kind != "upload" {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, "oci.blob.put", repo, ociAuditPath(c.Param("rest")), "authorization_denied")
		return
	}
	if digest := c.Query("digest"); digest != "" {
		h.completeDirectUpload(c, repo, image, digest)
		return
	}
	settle, limit, err := h.beginUnresolvedPublish(c, repo)
	if err != nil {
		h.auditRejected(c, "oci.blob.put", repo, ociAuditPath(c.Param("rest")), publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布被拒绝")
		return
	}
	file, err := h.oci.CreateUploadTemp()
	if err != nil {
		settle(false, 0)
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "创建上传暂存失败")
		return
	}
	filePath := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(filePath)
		settle(false, 0)
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "关闭上传暂存失败")
		return
	}
	id := newUploadID()
	h.storeUpload(id, &ociUploadSession{filePath: filePath, repo: repo, image: image, createdAt: time.Now().UTC(), limit: limit, settle: settle, userID: ociRequestUserID(c)})
	location := "/v2/" + repo + "/" + image + "/blobs/uploads/" + id
	c.Header("Location", location)
	c.Header("Range", "0-0")
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.Status(http.StatusAccepted)
}

// Patch 追加分块上传内容，正文始终流式写入暂存文件。
func (h *OCIHandler) Patch(c *gin.Context) {
	repo, image, kind, id, ok := parseOCIPath(c.Param("repo"), c.Param("rest"))
	if !ok || kind != "upload" {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, "oci.blob.put", repo, ociAuditPath(c.Param("rest")), "authorization_denied")
		return
	}
	value, exists := h.sessions.Load(id)
	if !exists {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	session := value.(*ociUploadSession)
	if session.repo != repo || session.image != image {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	if session.userID != ociRequestUserID(c) {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	session.mu.Lock()
	var cleanup func()
	defer func() {
		session.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
	}()
	if session.closed {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	file, err := os.OpenFile(session.filePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "打开上传暂存失败")
		return
	}
	_, err = h.copyUploadChunk(session, file, c.Request.Body)
	closeErr := file.Close()
	if errors.Is(err, domain.ErrQuotaExceeded) {
		h.auditRejected(c, "oci.blob.put", repo, ociAuditPath(c.Param("rest")), "quota_exceeded")
		cleanup = h.closeUploadLocked(id, session, false)
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布被拒绝")
		return
	}
	if err != nil {
		writeOCIError(c, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "上传数据无效")
		return
	}
	if closeErr != nil {
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "关闭上传暂存失败")
		return
	}
	c.Header("Location", "/v2/"+repo+"/"+image+"/blobs/uploads/"+id)
	c.Header("Range", "0-"+strconv.FormatInt(maxInt64(session.size-1, 0), 10))
	c.Status(http.StatusAccepted)
}

// Put 完成分块上传或发布 manifest。
func (h *OCIHandler) Put(c *gin.Context) {
	repo, image, kind, value, ok := parseOCIPath(c.Param("repo"), c.Param("rest"))
	if !ok {
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "OCI 路径不存在")
		return
	}
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, ociAction(kind), repo, ociAuditPath(c.Param("rest")), "authorization_denied")
		return
	}
	switch kind {
	case "manifest":
		h.putManifest(c, repo, image, value)
	case "upload":
		if value == "" {
			digest := c.Query("digest")
			if digest == "" {
				writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", "缺少 digest")
				return
			}
			h.completeDirectUpload(c, repo, image, digest)
			return
		}
		h.completeUpload(c, repo, image, value)
	default:
		writeOCIError(c, http.StatusNotFound, "NAME_UNKNOWN", "OCI 路径不存在")
	}
}

func (h *OCIHandler) getManifest(c *gin.Context, repo, image, reference string) {
	asset, rc, err := h.oci.ResolveManifest(c.Request.Context(), repo, image, reference)
	if err != nil {
		writeOCIError(c, ociStatus(err), ociCode(err), "manifest 不存在")
		return
	}
	defer func() { _ = rc.Close() }()
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.Header("Docker-Content-Digest", "sha256:"+asset.BlobHash)
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

func (h *OCIHandler) getBlob(c *gin.Context, repo, image, digest string) {
	asset, rc, err := h.oci.ResolveBlob(c.Request.Context(), repo, image, digest)
	if err != nil {
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 不存在")
		return
	}
	defer func() { _ = rc.Close() }()
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.Header("Docker-Content-Digest", digest)
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

func (h *OCIHandler) getTags(c *gin.Context, repo, image string) {
	tags, err := h.oci.Tags(c.Request.Context(), repo, image)
	if err != nil {
		writeOCIError(c, ociStatus(err), ociCode(err), "tag 列表不存在")
		return
	}
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.JSON(http.StatusOK, gin.H{"name": image, "tags": tags})
}

func (h *OCIHandler) putManifest(c *gin.Context, repo, image, reference string) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxOCIRequestManifestBytes+1))
	if err != nil || len(body) > maxOCIRequestManifestBytes {
		h.auditRejected(c, "oci.manifest.put", repo, ociManifestPolicyPath(image, reference), "write_failed")
		writeOCIError(c, http.StatusRequestEntityTooLarge, "MANIFEST_INVALID", "manifest 过大")
		return
	}
	settle, err := h.beginPublish(c, repo, ociManifestPolicyPath(image, reference), int64(len(body)))
	if err != nil {
		h.auditRejected(c, "oci.manifest.put", repo, ociManifestPolicyPath(image, reference), publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "manifest 发布被拒绝")
		return
	}
	defer func() { settle(false) }()
	result, err := h.oci.PutManifest(repo, image, reference, body, defaultContentType(c.GetHeader("Content-Type")))
	if err != nil {
		h.auditRejected(c, "oci.manifest.put", repo, ociManifestPolicyPath(image, reference), publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "manifest 发布失败")
		return
	}
	settle(true)
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.Header("Location", "/v2/"+repo+"/"+image+"/manifests/"+result.Digest)
	c.Header("Docker-Content-Digest", result.Digest)
	if h.audit != nil {
		h.audit(c, "oci.manifest.put", "asset", repo+"/"+image+"/"+reference, repo, "digest="+result.Digest, "ok")
	}
	c.Status(http.StatusCreated)
}

func (h *OCIHandler) completeUpload(c *gin.Context, repo, image, id string) {
	digest := c.Query("digest")
	if digest == "" {
		writeOCIError(c, http.StatusBadRequest, "DIGEST_INVALID", "缺少 digest")
		return
	}
	value, exists := h.sessions.Load(id)
	if !exists {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	session := value.(*ociUploadSession)
	if session.repo != repo || session.image != image {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	if session.userID != ociRequestUserID(c) {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	session.mu.Lock()
	var cleanup func()
	defer func() {
		session.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
	}()
	if session.closed {
		writeOCIError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "上传会话不存在")
		return
	}
	file, err := os.OpenFile(session.filePath, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "打开上传暂存失败")
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := h.copyUploadChunk(session, file, c.Request.Body); err != nil {
		if errors.Is(err, domain.ErrQuotaExceeded) {
			h.auditRejected(c, "oci.blob.put", repo, ociAuditPath(c.Param("rest")), "quota_exceeded")
			cleanup = h.closeUploadLocked(id, session, false)
			writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布被拒绝")
			return
		}
		writeOCIError(c, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "上传数据无效")
		return
	}
	path := ociBlobPolicyPath(digest)
	if err := h.validateUnresolvedPublish(c, repo, path); err != nil {
		h.auditRejected(c, "oci.blob.put", repo, path, publishRejectionDetail(err))
		cleanup = h.closeUploadLocked(id, session, false)
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布被拒绝")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "读取暂存失败")
		return
	}
	_, err = h.oci.PutBlob(repo, digest, file)
	if err != nil {
		h.auditRejected(c, "oci.blob.put", repo, path, publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布失败")
		return
	}
	closeErr := file.Close()
	cleanup = h.closeUploadLocked(id, session, true)
	if closeErr != nil {
		writeOCIError(c, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "关闭上传暂存失败")
		return
	}
	c.Header("Location", "/v2/"+repo+"/"+image+"/blobs/"+digest)
	c.Header("Docker-Content-Digest", digest)
	c.Status(http.StatusCreated)
}

func (h *OCIHandler) completeDirectUpload(c *gin.Context, repo, image, digest string) {
	path := ociBlobPolicyPath(digest)
	settle, limit, err := h.beginRawPublish(c, repo, path)
	if err != nil {
		h.auditRejected(c, "oci.blob.put", repo, path, publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布被拒绝")
		return
	}
	defer func() { settle(false, 0) }()
	body := io.Reader(c.Request.Body)
	if c.Request.ContentLength < 0 && limit > 0 {
		body = &quotaReader{r: c.Request.Body, left: limit, limit: limit}
	}
	asset, err := h.oci.PutBlob(repo, digest, body)
	if err != nil {
		h.auditRejected(c, "oci.blob.put", repo, path, publishRejectionDetail(err))
		writeOCIError(c, ociStatus(err), ociCode(err), "blob 发布失败")
		return
	}
	settle(true, asset.Size)
	c.Header("Location", "/v2/"+repo+"/"+image+"/blobs/"+digest)
	c.Header("Docker-Content-Digest", digest)
	c.Status(http.StatusCreated)
}

func ociAuditPath(raw string) string { return "oci/" + strings.Trim(raw, "/") }

func ociAction(kind string) string {
	if kind == "manifest" {
		return "oci.manifest.put"
	}
	return "oci.blob.put"
}

func ociBlobPolicyPath(digest string) string {
	return "oci/blobs/" + strings.TrimPrefix(digest, "sha256:")
}

func ociManifestPolicyPath(image, reference string) string {
	if strings.HasPrefix(reference, "sha256:") {
		return "oci/manifests/" + image + "/" + strings.TrimPrefix(reference, "sha256:")
	}
	return "oci/tags/" + image + "/" + reference
}

func ociRequestUserID(c *gin.Context) int64 {
	if p, ok := auth.PrincipalFrom(c); ok {
		return p.UserID
	}
	return 0
}

func (h *OCIHandler) closeUpload(id string, session *ociUploadSession) {
	session.mu.Lock()
	cleanup := h.closeUploadLocked(id, session, false)
	session.mu.Unlock()
	cleanup()
}

func (h *OCIHandler) closeUploadLocked(id string, session *ociUploadSession, success bool) func() {
	h.sessions.Delete(id)
	if session.closed {
		return func() {}
	}
	session.closed = true
	if session.timer != nil {
		session.timer.Stop()
	}
	filePath, settle, size := session.filePath, session.settle, session.size
	session.settle = nil
	return func() {
		_ = os.Remove(filePath)
		if settle != nil {
			settle(success, size)
		}
	}
}

func (h *OCIHandler) copyUploadChunk(session *ociUploadSession, file *os.File, body io.Reader) (int64, error) {
	reader := body
	if session.limit > 0 {
		remaining := session.limit - session.size
		reader = &quotaReader{r: body, left: remaining, limit: session.limit}
	}
	n, err := io.Copy(file, reader)
	session.size += n
	return n, err
}

func (h *OCIHandler) storeUpload(id string, session *ociUploadSession) {
	session.timer = time.AfterFunc(h.uploadTTL, func() { h.expireUpload(id, session) })
	h.sessions.Store(id, session)
}

func (h *OCIHandler) expireUpload(id string, session *ociUploadSession) {
	value, exists := h.sessions.Load(id)
	if !exists || value != session {
		return
	}
	h.closeUpload(id, session)
}

func (h *OCIHandler) cleanupExpiredUploads(now time.Time) {
	h.sessions.Range(func(key, value any) bool {
		id, session := key.(string), value.(*ociUploadSession)
		if !now.Before(session.createdAt.Add(h.uploadTTL)) {
			h.closeUpload(id, session)
		}
		return true
	})
}

func parseOCIPath(repo, raw string) (string, string, string, string, bool) {
	rest := strings.TrimPrefix(raw, "/")
	if rest == "" {
		return repo, "", "", "", false
	}
	if strings.HasSuffix(rest, "/blobs/uploads/") {
		return repo, strings.TrimSuffix(strings.TrimSuffix(rest, "/blobs/uploads/"), "/"), "upload", "", true
	}
	if idx := strings.Index(rest, "/blobs/uploads/"); idx >= 0 {
		image, id := rest[:idx], strings.TrimPrefix(rest[idx+len("/blobs/uploads/"):], "/")
		return repo, image, "upload", id, image != "" && id != ""
	}
	if idx := strings.Index(rest, "/manifests/"); idx >= 0 {
		image, reference := rest[:idx], strings.TrimPrefix(rest[idx+len("/manifests/"):], "/")
		return repo, image, "manifest", reference, image != "" && reference != ""
	}
	if idx := strings.Index(rest, "/blobs/"); idx >= 0 {
		image, digest := rest[:idx], strings.TrimPrefix(rest[idx+len("/blobs/"):], "/")
		return repo, image, "blob", digest, image != "" && digest != ""
	}
	if strings.HasSuffix(rest, "/tags/list") {
		return repo, strings.TrimSuffix(rest, "/tags/list"), "tags", "", true
	}
	return repo, "", "", "", false
}

func newUploadID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", os.Getpid())))
	}
	return hex.EncodeToString(b[:])
}

func defaultContentType(value string) string {
	if value == "" {
		return "application/vnd.oci.image.manifest.v1+json"
	}
	return value
}

func validOCIDigest(value string) (string, bool) {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return "", false
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	if _, err := hex.DecodeString(hexValue); err != nil {
		return "", false
	}
	return hexValue, true
}

func writeOCIError(c *gin.Context, status int, code, message string) {
	c.Header("Docker-Distribution-API-Version", "registry/2.0")
	c.JSON(status, gin.H{"errors": []gin.H{{"code": code, "message": message}}})
}

// writeOCIUnauthorized 以 Distribution v2 的错误信封和 Basic 质询拒绝未认证请求，
// 让 Docker 客户端在探测和首个 push 请求时都能按同一握手重试账号口令。
func writeOCIUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Basic realm="JianArtifact"`)
	writeOCIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

func ociStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrImmutableRelease), errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, domain.ErrQuotaExceeded):
		return http.StatusTooManyRequests
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrUpstreamTimeout):
		return http.StatusGatewayTimeout
	case errors.Is(err, domain.ErrUpstream):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func ociCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied), errors.Is(err, domain.ErrImmutableRelease), errors.Is(err, domain.ErrQuotaExceeded):
		return "DENIED"
	case errors.Is(err, domain.ErrNotFound):
		return "BLOB_UNKNOWN"
	case errors.Is(err, domain.ErrConflict):
		return "DENIED"
	case errors.Is(err, domain.ErrValidation):
		return "DIGEST_INVALID"
	default:
		return "UNKNOWN"
	}
}

func maxInt64(value, fallback int64) int64 {
	if value < fallback {
		return fallback
	}
	return value
}
