package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// CargoHandler 适配 Cargo sparse registry 的 config、索引、下载和发布端点。
type CargoHandler struct {
	*RawHandler
	cargo     *domain.CargoService
	publicURL string
}

// NewCargoHandler 构造 Cargo 协议处理器。
func NewCargoHandler(raw *RawHandler, cargo *domain.CargoService, publicURL string) *CargoHandler {
	return &CargoHandler{RawHandler: raw, cargo: cargo, publicURL: strings.TrimRight(publicURL, "/")}
}

// RegisterCargoRoutes 注册 Cargo sparse 协议端点。
func RegisterCargoRoutes(r gin.IRouter, h *CargoHandler, mw ...gin.HandlerFunc) {
	grp := r.Group("/cargo", mw...)
	grp.GET("/:repo/*rest", h.Get)
	grp.HEAD("/:repo/*rest", h.Get)
	grp.PUT("/:repo/*rest", h.Put)
	grp.DELETE("/:repo/*rest", h.Delete)
}

// Get 处理 config.json、sparse 索引和 crate 下载。
func (h *CargoHandler) Get(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if !h.authorize(c, repoName, "read") {
		return
	}
	if rest == "config.json" {
		h.config(c, repoName)
		return
	}
	if crate, version, ok := parseCargoDownload(rest); ok {
		h.download(c, repoName, crate, version)
		return
	}
	crate := cargoNameFromIndexPath(rest)
	if crate == "" {
		writeCargoError(c, http.StatusNotFound, "not_found", "索引不存在")
		return
	}
	data, err := h.cargo.Index(c.Request.Context(), repoName, crate)
	if err != nil {
		writeCargoError(c, cargoStatus(err), cargoCode(err), "索引不存在")
		return
	}
	c.Header("Content-Type", "application/json")
	c.Header("Content-Length", stringInt64(int64(len(data))))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

// Put 处理 Cargo publish 与 unyank。
func (h *CargoHandler) Put(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if !h.authorize(c, repoName, "write") {
		h.auditRejected(c, cargoAction(rest), repoName, cargoAuditPath(rest), "authorization_denied")
		return
	}
	if rest == "api/v1/crates/new" {
		result, err := h.cargo.PublishWithGuard(c.Request.Context(), repoName, c.Request.Body, func(path string, size int64) (func(bool), error) {
			return h.beginPublish(c, repoName, path, size)
		})
		if err != nil {
			h.auditRejected(c, "cargo.publish", repoName, cargoAuditPath(rest), publishRejectionDetail(err))
			writeCargoError(c, cargoStatus(err), cargoCode(err), "发布失败")
			return
		}
		h.auditPublish(c, "cargo.publish", repoName, result.Path, result.Size)
		c.JSON(http.StatusOK, gin.H{"ok": true, "name": result.Name, "vers": result.Version, "cksum": result.Checksum})
		return
	}
	if crate, version, ok := parseCargoYank(rest, "unyank"); ok {
		if err := h.cargo.SetYanked(repoName, crate, version, false); err != nil {
			h.auditRejected(c, "cargo.unyank", repoName, cargoAuditPath(rest), publishRejectionDetail(err))
			writeCargoError(c, cargoStatus(err), cargoCode(err), "取消 yanked 失败")
			return
		}
		h.auditPublish(c, "cargo.unyank", repoName, cargoAuditPath(rest), 0)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	writeCargoError(c, http.StatusNotFound, "not_found", "资源不存在")
}

// Delete 处理 Cargo yank。
func (h *CargoHandler) Delete(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if !h.authorize(c, repoName, "write") {
		h.auditRejected(c, "cargo.yank", repoName, cargoAuditPath(rest), "authorization_denied")
		return
	}
	crate, version, ok := parseCargoYank(rest, "yank")
	if !ok {
		writeCargoError(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	if err := h.cargo.SetYanked(repoName, crate, version, true); err != nil {
		h.auditRejected(c, "cargo.yank", repoName, cargoAuditPath(rest), publishRejectionDetail(err))
		writeCargoError(c, cargoStatus(err), cargoCode(err), "设置 yanked 失败")
		return
	}
	h.auditPublish(c, "cargo.yank", repoName, cargoAuditPath(rest), 0)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func cargoAuditPath(rest string) string {
	return "cargo/" + strings.Trim(rest, "/")
}

func cargoAction(rest string) string {
	switch {
	case strings.HasSuffix(rest, "/yank"):
		return "cargo.yank"
	case strings.HasSuffix(rest, "/unyank"):
		return "cargo.unyank"
	default:
		return "cargo.publish"
	}
}

func (h *CargoHandler) config(c *gin.Context, repoName string) {
	base := h.publicURL
	if base == "" {
		base = requestBaseURL(c)
	}
	public := false
	if repo, err := h.repoSvc.Get(repoName); err == nil {
		public = repo.Visibility == "public"
	}
	doc, err := h.cargo.Config(repoName, base, public)
	if err != nil {
		writeCargoError(c, cargoStatus(err), cargoCode(err), "配置不存在")
		return
	}
	c.Header("Content-Type", "application/json")
	c.JSON(http.StatusOK, doc)
}

func (h *CargoHandler) download(c *gin.Context, repoName, crate, version string) {
	asset, rc, err := h.cargo.Download(c.Request.Context(), repoName, crate, version)
	if err != nil {
		writeCargoError(c, cargoStatus(err), cargoCode(err), "crate 不存在")
		return
	}
	defer func() { _ = rc.Close() }()
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

func parseCargoDownload(rest string) (string, string, bool) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 6 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "crates" || parts[5] != "download" {
		return "", "", false
	}
	return parts[3], parts[4], parts[3] != "" && parts[4] != ""
}

func parseCargoYank(rest, action string) (string, string, bool) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 6 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "crates" || parts[5] != action {
		return "", "", false
	}
	return parts[3], parts[4], parts[3] != "" && parts[4] != ""
}

func cargoNameFromIndexPath(rest string) string {
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	var name string
	switch {
	case len(parts) == 2 && parts[0] == "1" && len(parts[1]) == 1:
		name = parts[1]
	case len(parts) == 2 && parts[0] == "2" && len(parts[1]) == 2:
		name = parts[1]
	case len(parts) == 3 && parts[0] == "3" && len(parts[2]) == 3 && parts[1] == parts[2][:1]:
		name = parts[2]
	case len(parts) == 3 && len(parts[2]) >= 4 && parts[0] == parts[2][:2] && parts[1] == parts[2][2:4]:
		name = parts[2]
	default:
		return ""
	}
	if cargoIndexPath(name) != rest {
		return ""
	}
	return name
}

func cargoIndexPath(name string) string {
	name = strings.ToLower(name)
	switch len(name) {
	case 1:
		return "1/" + name
	case 2:
		return "2/" + name
	case 3:
		return "3/" + name[:1] + "/" + name
	default:
		return name[:2] + "/" + name[2:4] + "/" + name
	}
}

func mergeCargoIndex(existing, incoming []byte) ([]string, error) {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, source := range [][]byte{existing, incoming} {
		for _, line := range bytes.Split(source, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var item struct {
				Vers string `json:"vers"`
			}
			if json.Unmarshal(line, &item) != nil || item.Vers == "" {
				return nil, errors.New("cargo 索引记录非法")
			}
			if _, ok := seen[item.Vers]; ok {
				continue
			}
			seen[item.Vers] = struct{}{}
			result = append(result, string(line))
		}
	}
	return result, nil
}

func setCargoYanked(lines []string, version string, yanked bool) ([]string, bool, error) {
	result := append([]string(nil), lines...)
	changed := false
	for i, line := range result {
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, false, err
		}
		if item["vers"] != version {
			continue
		}
		item["yanked"] = yanked
		data, err := json.Marshal(item)
		if err != nil {
			return nil, false, err
		}
		result[i] = string(data)
		changed = true
	}
	return result, changed, nil
}

func containsYanked(line string) bool {
	var item map[string]any
	return json.Unmarshal([]byte(line), &item) == nil && item["yanked"] == true
}

func writeCargoError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"errors": []gin.H{{"detail": message, "code": code}}})
}

func cargoStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrImmutableRelease), errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, domain.ErrQuotaExceeded):
		return http.StatusTooManyRequests
	case errors.Is(err, domain.ErrUpstreamTimeout):
		return http.StatusGatewayTimeout
	case errors.Is(err, domain.ErrUpstream):
		return http.StatusBadGateway
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func cargoCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrPublishPathDenied):
		return "publish_path_denied"
	case errors.Is(err, domain.ErrImmutableRelease):
		return "immutable_release"
	case errors.Is(err, domain.ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, domain.ErrUpstreamTimeout):
		return "upstream_timeout"
	case errors.Is(err, domain.ErrUpstream):
		return "upstream_error"
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrValidation):
		return "invalid_request"
	default:
		return "internal"
	}
}

func requestBaseURL(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + c.Request.Host
}

func stringInt64(value int64) string {
	if value < 0 {
		return "0"
	}
	return strconv.FormatInt(value, 10)
}
