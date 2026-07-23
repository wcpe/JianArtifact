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
	assets  *domain.AssetService
	repoSvc *domain.RepositoryService
}

// NewRawHandler 构造 RawHandler。
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
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if !h.authorize(c, repo, "read") {
		return
	}
	asset, rc, err := h.assets.Resolve(c.Request.Context(), repo, artPath)
	if err != nil {
		writeAssetErr(c, err)
		return
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

// Put 处理 PUT：鉴权 write → 流式入库；成功返回 201 与制品摘要。
func (h *RawHandler) Put(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if artPath == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "制品路径不能为空")
		return
	}
	if !h.authorize(c, repo, "write") {
		return
	}
	asset, err := h.assets.Put(repo, artPath, c.Request.Body, c.GetHeader("Content-Type"))
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, assetSummary{
		Repository:  repo,
		Path:        asset.Path,
		Hash:        asset.BlobHash,
		Size:        asset.Size,
		ContentType: asset.ContentType,
	})
}

// Delete 处理 DELETE：鉴权 write → 删除制品元数据（blob 内容保留）。
func (h *RawHandler) Delete(c *gin.Context) {
	repo := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath"))
	if !h.authorize(c, repo, "write") {
		return
	}
	if err := h.assets.Delete(repo, artPath); err != nil {
		writeAssetErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
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
			auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
		}
		return false
	}
	return true
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

// cleanArtifactPath 归一化 gin 通配段 *artifactPath：去除前导斜杠。
func cleanArtifactPath(raw string) string {
	return strings.TrimPrefix(raw, "/")
}
