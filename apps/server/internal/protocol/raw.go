// Package protocol 承载仓库格式的原生客户端协议适配（0.3.0 起：Raw hosted）。
//
// 分层（见 internal/doc.go）：protocol -> domain, auth。protocol 把 HTTP 语义
// （方法、路径、头、状态码）翻译为 domain 服务调用，不直接触碰持久化或 blob 存储。
// 这些端点不在 OpenAPI 契约内（面向 curl 等原生客户端），经 httpserver 的
// WithProtocolRoutes 注册在契约路由之后、静态 SPA 回退之前。
package protocol

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// RawHandler 处理 Raw 格式仓库的发布 / 拉取 / 删除。
type RawHandler struct {
	assets         *domain.AssetService
	repoSvc        *domain.RepositoryService
	publish        *domain.PublishPolicyService
	audit          AuditFunc          // FR-38：审计记录回调（main 注入；nil 不记录）
	operationAudit OperationAuditFunc // 原子制品操作的同事务审计构造器
}

// quotaReader 在流式上传超过预留上限时立即终止读取，避免先落盘再拒绝。
type quotaReader struct {
	r     io.Reader
	left  int64
	limit int64
}

func (r *quotaReader) Read(p []byte) (int, error) {
	if r.limit > 0 && r.left <= 0 {
		var one [1]byte
		n, err := r.r.Read(one[:])
		if n > 0 || err == nil {
			return 0, domain.ErrQuotaExceeded
		}
		return 0, err
	}
	if r.limit > 0 && int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

// AuditFunc 是审计记录回调（FR-38）：写操作成功后记录一条审计日志。
// 由 main 组装时注入（闭包绑定 api.Handlers.AuditLog），避免 protocol 依赖 api 包。
type AuditFunc func(c *gin.Context, action, entityType, entityKey, repo, detail, result string)

// OperationAuditFunc 从原生协议请求提取操作主体及同事务审计回调。
type OperationAuditFunc func(c *gin.Context, action, repo string) domain.AssetOperationAudit

// SetAudit 注入审计记录回调（FR-38）；nil 表示不记录。
func (h *RawHandler) SetAudit(f AuditFunc) { h.audit = f }

// SetOperationAudit 注入原子制品操作审计构造器；nil 表示保持旧装配兼容。
func (h *RawHandler) SetOperationAudit(f OperationAuditFunc) { h.operationAudit = f }

// SetPublishPolicy 注入发布账号策略校验；为空时保持旧行为，供兼容测试和只读部署使用。
func (h *RawHandler) SetPublishPolicy(s *domain.PublishPolicyService) { h.publish = s }

func (h *RawHandler) nativeOperationAudit(c *gin.Context, action, repo string) domain.AssetOperationAudit {
	if h.operationAudit == nil {
		return domain.AssetOperationAudit{}
	}
	return h.operationAudit(c, action, repo)
}

func (h *RawHandler) beginPublish(c *gin.Context, repo, path string, size int64) (func(bool), error) {
	if h.publish == nil {
		return func(bool) {}, nil
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	id, err := h.publish.Begin(userID, repo, path, size)
	if err != nil {
		return nil, err
	}
	return func(success bool) { _ = h.publish.Settle(id, success) }, nil
}

func NewRawHandler(assets *domain.AssetService, repoSvc *domain.RepositoryService) *RawHandler {
	return &RawHandler{assets: assets, repoSvc: repoSvc}
}

// assetSummary 是 PUT 成功后返回的制品摘要（非契约类型）。
type assetSummary struct {
	Repository  string `json:"repository"`
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

// Get 处理 GET/HEAD：鉴权 read → 流式回写内容，设置 Content-Type/Length/ETag。
// HEAD 不写 body。
func (h *RawHandler) Get(c *gin.Context) {
	repo := c.Param("repo")
	if !h.authorize(c, repo, "read") {
		return
	}
	if h.tryBrowse(c) {
		return
	}
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	cacheCandidate, cacheHit := h.cacheCandidate(repo, artPath)
	asset, rc, err := h.assets.Resolve(c.Request.Context(), repo, artPath)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if cacheCandidate {
		if cacheHit {
			c.Set("jianartifact.protocol.cache_result", "hit")
		} else {
			c.Set("jianartifact.protocol.cache_result", "miss")
		}
	}
	defer func() { _ = rc.Close() }()

	c.Header("Content-Type", asset.ContentType)
	c.Header("Content-Length", strconv.FormatInt(asset.Size, 10))
	c.Header("ETag", `"`+asset.BlobHash+`"`)
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// cacheCandidate 只对单个 proxy 仓库标记可缓存读取；group 的首个命中成员不可在此层可靠判定，保持未知。
func (h *RawHandler) cacheCandidate(repoName, artifactPath string) (bool, bool) {
	repo, err := h.repoSvc.Get(repoName)
	if err != nil || repo.Type != "proxy" {
		return false, false
	}
	_, reader, err := h.assets.Get(repoName, artifactPath)
	if err != nil {
		return true, false
	}
	_ = reader.Close()
	return true, true
}

// Put 处理 PUT：鉴权 write → 流式入库；成功返回 201 与制品摘要。
func (h *RawHandler) Put(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if artPath == "" {
		h.auditRejected(c, "asset.put", repo, artPath, "invalid_path")
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "制品路径不能为空")
		return
	}
	if !h.authorize(c, repo, "write") {
		h.auditRejected(c, "asset.put", repo, artPath, "authorization_denied")
		return
	}
	settle, limit, err := h.beginRawPublish(c, repo, artPath)
	if err != nil {
		h.auditRejected(c, "asset.put", repo, artPath, publishRejectionDetail(err))
		writePublishErr(c, err)
		return
	}
	defer func() { settle(false, 0) }()
	body := io.Reader(c.Request.Body)
	if c.Request.ContentLength < 0 && limit > 0 {
		body = &quotaReader{r: c.Request.Body, left: limit, limit: limit}
	}
	asset, err := h.assets.Put(repo, artPath, body, c.GetHeader("Content-Type"))
	if err != nil {
		h.auditRejected(c, "asset.put", repo, artPath, publishRejectionDetail(err))
		if errors.Is(err, domain.ErrQuotaExceeded) {
			writePublishErr(c, err)
			return
		}
		writeAssetErr(c, err)
		return
	}
	if h.publish != nil {
		settle(true, asset.Size)
	}
	if h.audit != nil {
		h.audit(c, "asset.put", "asset", repo+"/"+asset.Path, repo,
			"size="+strconv.FormatInt(asset.Size, 10), "ok")
	}
	c.JSON(http.StatusCreated, assetSummary{
		Repository:  repo,
		Path:        asset.Path,
		Hash:        asset.BlobHash,
		Size:        asset.Size,
		ContentType: asset.ContentType,
	})
}

// auditRejected 记录可预期的协议拒绝；仅保存稳定的拒绝类别，不拼接错误文本或请求凭据。
func (h *RawHandler) auditRejected(c *gin.Context, action, repo, artifactPath, detail string) {
	if h.audit == nil {
		return
	}
	h.audit(c, action, "asset", repo+"/"+artifactPath, repo, detail, "rejected")
}

func (h *RawHandler) auditPublish(c *gin.Context, action, repo, artifactPath string, size int64) {
	if h.audit == nil {
		return
	}
	h.audit(c, action, "asset", repo+"/"+artifactPath, repo, "size="+strconv.FormatInt(size, 10), "ok")
}

func publishRejectionDetail(err error) string {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied):
		return "publish_path_denied"
	case errors.Is(err, domain.ErrImmutableRelease):
		return "immutable_release"
	case errors.Is(err, domain.ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	default:
		return "write_failed"
	}
}

func (h *RawHandler) beginRawPublish(c *gin.Context, repo, path string) (func(bool, int64), int64, error) {
	if h.publish == nil {
		return func(bool, int64) {}, 0, nil
	}
	if c.Request.ContentLength >= 0 {
		settle, err := h.beginPublish(c, repo, path, c.Request.ContentLength)
		return func(ok bool, _ int64) { settle(ok) }, 0, err
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	id, limit, err := h.publish.BeginStreaming(userID, repo, path)
	if err != nil {
		return nil, 0, err
	}
	return func(ok bool, size int64) { _ = h.publish.SettleSize(id, ok, size) }, limit, nil
}

func (h *RawHandler) beginUnresolvedPublish(c *gin.Context, repo string) (func(bool, int64), int64, error) {
	if h.publish == nil {
		return func(bool, int64) {}, 0, nil
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	id, limit, err := h.publish.BeginUnresolvedStreaming(userID, repo)
	if err != nil {
		return nil, 0, err
	}
	return func(ok bool, size int64) { _ = h.publish.SettleSize(id, ok, size) }, limit, nil
}

func (h *RawHandler) validateUnresolvedPublish(c *gin.Context, repo, path string) error {
	if h.publish == nil {
		return nil
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	return h.publish.ValidateUnresolvedPublish(userID, repo, path)
}

func writePublishErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied):
		auth.WriteError(c, http.StatusForbidden, "publish_path_denied", "发布路径不在允许前缀内")
	case errors.Is(err, domain.ErrImmutableRelease):
		auth.WriteError(c, http.StatusConflict, "immutable_release", "不可变 Release 不允许覆盖")
	case errors.Is(err, domain.ErrQuotaExceeded):
		auth.WriteError(c, http.StatusTooManyRequests, "quota_exceeded", "发布额度已用尽")
	case errors.Is(err, domain.ErrConflict):
		auth.WriteError(c, http.StatusConflict, "conflict", "该仓库不支持此操作")
	case errors.Is(err, domain.ErrLogicalDeleteRequired):
		auth.WriteError(c, http.StatusConflict, "logical_delete_required", "必须使用格式感知的逻辑删除目标")
	case errors.Is(err, domain.ErrOperationUnsupported):
		auth.WriteError(c, http.StatusConflict, "operation_unsupported", "该格式不支持此制品操作")
	case errors.Is(err, domain.ErrOperationLimit):
		auth.WriteError(c, http.StatusBadRequest, "operation_limit", "单次实际制品数不得超过 500")
	default:
		writeAssetErr(c, err)
	}
}

// Delete 处理 DELETE：仅全局管理员可删除制品元数据（blob 内容保留）。
func (h *RawHandler) Delete(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	operationID := domain.NewOperationID()
	if !h.requireAdmin(c, operationID) {
		h.auditRejected(c, "asset.delete", repo, artPath, "authorization_denied")
		return
	}
	result, err := h.assets.ApplyOperation(repo, domain.AssetOperation{
		Action:  domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: artPath}},
		Audit:   h.nativeOperationAudit(c, "asset.delete", repo),
	})
	if err != nil {
		h.auditRejected(c, "asset.delete", repo, artPath, publishRejectionDetail(err))
		if result != nil {
			operationID = result.OperationID
		}
		writeRawDeleteError(c, err, operationID)
		return
	}
	c.Status(http.StatusNoContent)
}

// requireAdmin 要求全局管理员权限，并为 Raw 删除失败保留操作追踪标识。
func (h *RawHandler) requireAdmin(c *gin.Context, operationID string) bool {
	principal, ok := auth.PrincipalFrom(c)
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="JianArtifact"`)
		writeRawDeleteErrorResponse(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效", operationID)
		return false
	}
	if !principal.IsAdmin() {
		writeRawDeleteErrorResponse(c, http.StatusForbidden, "forbidden", "仅管理员可删除制品", operationID)
		return false
	}
	return true
}

func writeRawDeleteError(c *gin.Context, err error, operationID string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeRawDeleteErrorResponse(c, http.StatusNotFound, "not_found", "资源不存在", operationID)
	case errors.Is(err, domain.ErrConflict):
		writeRawDeleteErrorResponse(c, http.StatusConflict, "conflict", "该仓库不支持此操作", operationID)
	case errors.Is(err, domain.ErrUpstreamTimeout):
		writeRawDeleteErrorResponse(c, http.StatusGatewayTimeout, "upstream_timeout", "上游回源超时", operationID)
	case errors.Is(err, domain.ErrUpstream):
		writeRawDeleteErrorResponse(c, http.StatusBadGateway, "upstream_error", "上游回源失败", operationID)
	default:
		writeRawDeleteErrorResponse(c, http.StatusInternalServerError, "internal", "内部错误", operationID)
	}
}

func writeRawDeleteErrorResponse(c *gin.Context, status int, code, message, operationID string) {
	c.JSON(status, gin.H{
		"error":       gin.H{"code": code, "message": message},
		"operationId": operationID,
	})
}

// authorize 判定主体对仓库是否可执行动作。全局管理员放行；否则按 ACL（含 public read）判定。
// 无主体且无权限 → 401；有主体无权限 → 403；仓库不存在 → 404。返回 false 时已写出响应。
func (h *RawHandler) authorize(c *gin.Context, repo, action string) bool {
	principal, hasPrincipal := auth.PrincipalFrom(c)
	if hasPrincipal && principal.IsAdmin() {
		return true
	}
	var subjectID int64
	if hasPrincipal {
		subjectID = principal.UserID
	}
	ok, err := h.repoSvc.CanAccess(repo, subjectID, action)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "仓库不存在")
			return false
		}
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return false
	}
	if !ok {
		if hasPrincipal {
			auth.WriteError(c, http.StatusForbidden, "forbidden", "无权访问该仓库")
		} else {
			writeUnauthorized(c)
		}
		return false
	}
	return true
}

// writeUnauthorized 写出协议端点的 401，并携带 WWW-Authenticate: Basic 质询头：
// Maven/Gradle 等客户端默认非抢占式认证，收不到 Basic 质询就不会带凭据重试。
// 仅限 /repository/* 协议端点使用；/api/* 不得携带该头，否则浏览器会弹原生登录框。
func writeUnauthorized(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/v2/") {
		writeOCIUnauthorized(c)
		return
	}
	c.Header("WWW-Authenticate", `Basic realm="JianArtifact"`)
	auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
}

// writeAssetErr 把领域错误映射为协议层 HTTP 状态：不存在 404、非 raw-hosted / 写只读仓库 409、
// 回源上游失败 502、回源超时 504、其余 500。
func writeAssetErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, domain.ErrConflict):
		auth.WriteError(c, http.StatusConflict, "conflict", "该仓库不支持此操作")
	case errors.Is(err, domain.ErrUpstreamTimeout):
		auth.WriteError(c, http.StatusGatewayTimeout, "upstream_timeout", "上游回源超时")
	case errors.Is(err, domain.ErrUpstream):
		auth.WriteError(c, http.StatusBadGateway, "upstream_error", "上游回源失败")
	default:
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
	}
}

// cleanArtifactPath 归一化 gin 通配段 *artifactPath：去除全部前导斜杠并折叠连续斜杠（// → /）。
// 客户端拼接 baseUrl + "/" + path 可能产生双斜杠（如 /repository/maven-public//io/...），
// gin 的 * 通配符会保留其（如 "//io/..."），折叠并去前导斜杠后与库内存储路径一致，避免 404。
func cleanArtifactPath(raw string) string {
	s := strings.TrimLeft(raw, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}
