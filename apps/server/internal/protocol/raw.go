// Package protocol 承载仓库格式的原生客户端协议适配（0.3.0 起：Raw hosted）。
//
// 分层（见 internal/doc.go）：protocol -> domain, auth。protocol 把 HTTP 语义
// （方法、路径、头、状态码）翻译为 domain 服务调用，不直接触碰持久化或 blob 存储。
// 这些端点不在 OpenAPI 契约内（面向 curl 等原生客户端），经 httpserver 的
// WithProtocolRoutes 注册在契约路由之后、静态 SPA 回退之前。
package protocol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// RawHandler 处理 Raw 格式仓库的发布 / 拉取 / 删除。
type RawHandler struct {
	assets         *domain.AssetService
	repoSvc        *domain.RepositoryService
	publish        *domain.PublishPolicyService
	quota          QuotaGuard         // FR-41：仓库级存储配额判定（nil 不判定）
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

// QuotaGuard 是仓库级存储配额判定（FR-41 §4.1）。协议层只需要两个能力：
// 准入判定（已知长度直接给结论，长度未知返回流式读取上限）与流式早拒上报。
// 由装配层注入 *domain.RepoQuotaService；接口在此声明是为了让协议单测可注入桩。
type QuotaGuard interface {
	// Admission 判定一次发布的准入并返回流式读取上限（0 表示不限）。
	Admission(repoName, path string, size int64) (int64, error)
	// RecordStreamRejection 上报一次由限额感知读取器在流中触发的早拒。
	RecordStreamRejection()
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

// SetQuotaGuard 注入仓库级存储配额判定（FR-41）；nil 表示不做判定，供兼容测试与只读部署使用。
func (h *RawHandler) SetQuotaGuard(g QuotaGuard) { h.quota = g }

// admissionFor 统一做仓库存储配额的准入判定（FR-41 §4.1 判定时序第 1/2 级）。
// 所有原生协议（raw/maven/npm/cargo/oci/pypi/nuget）的发布入口都经此一处调用，
// 避免逐协议各写一遍判定而漏掉某条绕过通道；越限错误经既有 ErrQuotaExceeded
// 映射落到 429 quota_exceeded，与发布额度的 429 口径一致。
func (h *RawHandler) admissionFor(repo, path string, size int64) (int64, error) {
	if h.quota == nil {
		return 0, nil
	}
	return h.quota.Admission(repo, path, size)
}

// minLimit 合并两个「字节上限」：0 表示不限；两者都非零取较小值。
func minLimit(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// recordQuotaRejection 在协议层观察到配额拒绝时上报一次（FR-41 §4.5 指标）。
// 预检与提交点的拒绝以 *domain.StorageQuotaError 形式返回、已由配额服务累加；
// 只在限额感知读取器于流中中断时（裸 ErrQuotaExceeded）才需要协议层补报，避免重复计数。
func (h *RawHandler) recordQuotaRejection(err error) {
	if h.quota == nil || !errors.Is(err, domain.ErrQuotaExceeded) {
		return
	}
	var counted *domain.StorageQuotaError
	if errors.As(err, &counted) {
		return
	}
	h.quota.RecordStreamRejection()
}

func (h *RawHandler) nativeOperationAudit(c *gin.Context, action, repo string) domain.AssetOperationAudit {
	if h.operationAudit == nil {
		return domain.AssetOperationAudit{}
	}
	return h.operationAudit(c, action, repo)
}

func (h *RawHandler) beginPublish(c *gin.Context, repo, path string, size int64) (func(bool), error) {
	// FR-41：仓库存储配额预检先于发布额度预留——越限时既不读请求体也不占额度。
	if _, err := h.admissionFor(repo, path, size); err != nil {
		return nil, err
	}
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

// CacheResultContextKey 是请求上下文里缓存来源标记的键，由 main 的协议指标中间件读取。
const CacheResultContextKey = "jianartifact.protocol.cache_result"

type repositorySnapshotContextKey struct{}

func withRepositorySnapshot(c *gin.Context, repo *repository.Repository) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), repositorySnapshotContextKey{}, repo))
}

func repositorySnapshot(c *gin.Context, repoName string) (*repository.Repository, error) {
	if repo, ok := c.Request.Context().Value(repositorySnapshotContextKey{}).(*repository.Repository); ok {
		if repo.Name == repoName {
			return repo, nil
		}
		for _, alias := range repo.Aliases {
			if alias == repoName {
				return repo, nil
			}
		}
	}
	return nil, nil
}

// withCacheOutcome 在当前请求上下文挂载缓存来源记录器，使后续 domain 解析调用能产出 hit/miss。
// 所有制品读取 handler 都应在解析前调用；hosted/group 不会写入，保持缺省（未知）。
func withCacheOutcome(c *gin.Context) {
	c.Request = c.Request.WithContext(domain.WithCacheOutcome(c.Request.Context()))
}

// markCacheResult 读取本次请求的缓存来源，仅当请求命中代理仓库且来源可判定（hit/miss）时写入上下文键。
// hosted/group 一律保持缺省：group 的首个命中成员无法在此层可靠判定，宁可未知也不猜。
func (h *RawHandler) markCacheResult(c *gin.Context, repoName string) {
	outcome := domain.CacheOutcomeFromContext(c.Request.Context())
	if outcome == domain.CacheOutcomeUnknown {
		return
	}
	repo, err := repositorySnapshot(c, repoName)
	if err != nil || repo == nil || repo.Type != "proxy" {
		return
	}
	c.Set(CacheResultContextKey, string(outcome))
}

// Get 处理 GET/HEAD：鉴权 read → 流式回写内容，设置 Content-Type/Length/ETag。
// HEAD 不写 body。
func (h *RawHandler) Get(c *gin.Context) {
	repo := c.Param("repo")
	withCacheOutcome(c)
	defer h.markCacheResult(c, repo)
	if !h.authorize(c, repo, "read") {
		return
	}
	if h.tryBrowse(c) {
		return
	}
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	repoSnapshot, err := repositorySnapshot(c, repo)
	if err != nil || repoSnapshot == nil {
		if err != nil {
			writeAssetErr(c, err)
		} else {
			auth.WriteError(c, http.StatusNotFound, "not_found", "仓库不存在")
		}
		return
	}
	asset, rc, err := h.assets.ResolveWithRepo(c.Request.Context(), repoSnapshot, artPath)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	defer func() { _ = rc.Close() }()

	c.Header("Content-Type", asset.ContentType)
	c.Header("ETag", `"`+asset.BlobHash+`"`)
	// 本地 blob 是 *os.File（可寻址）：交给 http.ServeContent 协商 Range / If-Range /
	// If-None-Match，使 curl -C -、构建工具重试等断点续传拿到 206 与 Content-Range；
	// 不可寻址的内容流（如 proxy 回源）退化为一次性全量回写，与改动前行为一致。
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(c.Writer, c.Request, "", time.Time{}, rs)
		return
	}
	c.Header("Content-Length", strconv.FormatInt(asset.Size, 10))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// Put 处理 PUT：鉴权 publish → 流式入库；成功返回 201 与制品摘要。
//
// 动作从 write 收窄为 publish（FR-36）：「能写某个路径」与「能发布新版本」在细化后的
// 动作集里是两件事。向后兼容由 Publish 的蕴含关系保证——write 蕴含 publish，
// 故存量 write 授权照旧可以发布，而新授予的 publish 不再顺带拿到 write/read。
func (h *RawHandler) Put(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if artPath == "" {
		h.auditRejected(c, "asset.put", repo, artPath, "invalid_path")
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "制品路径不能为空")
		return
	}
	if !h.authorize(c, repo, repository.ActionPublish) {
		h.auditRejected(c, "asset.put", repo, artPath, "authorization_denied")
		return
	}
	settle, limit, err := h.beginRawPublish(c, repo, artPath)
	if err != nil {
		h.auditRejected(c, "asset.put", repo, artPath, publishRejectionDetail(err))
		h.recordQuotaRejection(err)
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
			h.recordQuotaRejection(err)
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

// beginRawPublish 为「路径已知」的发布做预检：已知 Content-Length 时在读取请求体之前
// 判定（越限直接返回，不读流）；长度未知时返回流式上限，由调用方包成限额感知读取器早拒。
func (h *RawHandler) beginRawPublish(c *gin.Context, repo, path string) (func(bool, int64), int64, error) {
	if c.Request.ContentLength >= 0 {
		settle, err := h.beginPublish(c, repo, path, c.Request.ContentLength)
		return func(ok bool, _ int64) { settle(ok) }, 0, err
	}
	// FR-41：长度未知——路径已知，配额给出抵扣覆盖写旧大小后的流式早拒上限。
	quotaLimit, err := h.admissionFor(repo, path, -1)
	if err != nil {
		return nil, 0, err
	}
	if h.publish == nil {
		return func(bool, int64) {}, quotaLimit, nil
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	id, limit, err := h.publish.BeginStreaming(userID, repo, path)
	if err != nil {
		return nil, 0, err
	}
	return func(ok bool, size int64) { _ = h.publish.SettleSize(id, ok, size) }, minLimit(limit, quotaLimit), nil
}

// beginUnresolvedPublish 为「路径尚未解析」的发布做预检（multipart / 上传会话建立时）。
// FR-41：路径未知时无法计算覆盖写抵扣，只能给出不会误拒合法写入的安全上限（整仓配额），
// 精确判定由路径解析后的 validateUnresolvedPublish 与提交点复检负责。
func (h *RawHandler) beginUnresolvedPublish(c *gin.Context, repo string) (func(bool, int64), int64, error) {
	quotaLimit, err := h.admissionFor(repo, "", -1)
	if err != nil {
		return nil, 0, err
	}
	if h.publish == nil {
		return func(bool, int64) {}, quotaLimit, nil
	}
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(c); ok {
		userID = p.UserID
	}
	id, limit, err := h.publish.BeginUnresolvedStreaming(userID, repo)
	if err != nil {
		return nil, 0, err
	}
	return func(ok bool, size int64) { _ = h.publish.SettleSize(id, ok, size) }, minLimit(limit, quotaLimit), nil
}

// validateUnresolvedPublish 在制品路径解析出来后补做准入判定与发布额度路径校验。
// FR-41：此时路径已知，可做精确判定（件数上限 + 字节上限 + 覆盖写抵扣），
// 判定发生在任何内容进入 blob 存储之前，越限即 429。
func (h *RawHandler) validateUnresolvedPublish(c *gin.Context, repo, path string) error {
	if _, err := h.admissionFor(repo, path, -1); err != nil {
		return err
	}
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
		auth.WriteError(c, http.StatusTooManyRequests, "quota_exceeded", quotaRejectionMessage(err))
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

// Delete 处理 DELETE：管理员，或对该仓库持有 delete 授权的主体（blob 内容保留）。
//
// 删除权从「仅全局管理员」放开到 ACL 的 delete 动作（FR-36）：仓库级删除授权此前
// 只能靠给账号加全局管理员，等于把实例级权限当仓库级权限发。delete 只蕴含自身，
// 故授予 delete 不会顺带拿到 read/write/publish；全局管理员分支原样保留。
func (h *RawHandler) Delete(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	operationID := domain.NewOperationID()
	if !h.requireDelete(c, repo, operationID) {
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

// requireDelete 要求删除制品的授权：全局管理员，或对该仓库持有 delete 动作者。
// 未认证 401、已认证越权 403；为 Raw 删除失败保留操作追踪标识。
func (h *RawHandler) requireDelete(c *gin.Context, repo string, operationID string) bool {
	principal, ok := auth.PrincipalFrom(c)
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="JianArtifact"`)
		writeRawDeleteErrorResponse(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效", operationID)
		return false
	}
	if principal.IsAdmin() {
		return true
	}
	// 主体（含组归属）展开一次，再按 delete 动作判定。
	subject, err := h.repoSvc.SubjectFor(principal.UserID)
	if err != nil {
		writeRawDeleteErrorResponse(c, http.StatusInternalServerError, "internal", "内部错误", operationID)
		return false
	}
	allowed, err := h.repoSvc.CanAccess(repo, subject, repository.ActionDelete)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeRawDeleteErrorResponse(c, http.StatusNotFound, "not_found", "仓库不存在", operationID)
			return false
		}
		writeRawDeleteErrorResponse(c, http.StatusInternalServerError, "internal", "内部错误", operationID)
		return false
	}
	if !allowed {
		writeRawDeleteErrorResponse(c, http.StatusForbidden, "forbidden", "无权删除该仓库制品", operationID)
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

// principalUserID 取出当前请求的用户 ID；未认证返回 0（匿名）。
//
// 单独抽出来是因为协议层各处都要「有主体才取 ID、否则当匿名」这个同样的三行，
// 而这正是最容易漏判的一处——漏一次就把未认证请求当成 ID 为 0 的有效主体传下去。
func principalUserID(c *gin.Context) int64 {
	if p, ok := auth.PrincipalFrom(c); ok {
		return p.UserID
	}
	return 0
}

// authorize 判定主体对仓库是否可执行动作。全局管理员放行；否则按 ACL（含 public read）判定。
// 无主体且无权限 → 401；有主体无权限 → 403；仓库不存在 → 404。返回 false 时已写出响应。
func (h *RawHandler) authorize(c *gin.Context, repo, action string) bool {
	repoSnapshot, lookupErr := repositorySnapshot(c, repo)
	if lookupErr != nil {
		writeAssetErr(c, lookupErr)
		return false
	}
	if repoSnapshot == nil {
		var err error
		repoSnapshot, err = h.repoSvc.Get(repo)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				auth.WriteError(c, http.StatusNotFound, "not_found", "仓库不存在")
			} else {
				writeAssetErr(c, err)
			}
			return false
		}
		withRepositorySnapshot(c, repoSnapshot)
	}
	principal, hasPrincipal := auth.PrincipalFrom(c)
	if hasPrincipal && principal.IsAdmin() {
		return true
	}
	// 主体（含其所属用户组）在本层展开一次：HasPermission 需要组 ID 集合，
	// 而协议层的十来个 authorize 调用点都只持有用户 ID。
	subject, err := h.repoSvc.SubjectFor(principalUserID(c))
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return false
	}
	ok, err := h.repoSvc.CanAccessResolved(repoSnapshot, subject, action)
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

// quotaRejectionMessage 给出配额超限的可读提示。仓库存储配额（FR-41）的
// 「当前占用 / 上限」文本由领域层构造，只含仓库名与数字，**不含文件系统路径**（docs/API.md:10）。
func quotaRejectionMessage(err error) string {
	var quotaErr *domain.StorageQuotaError
	if errors.As(err, &quotaErr) {
		return quotaErr.Error()
	}
	return "发布额度已用尽"
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
