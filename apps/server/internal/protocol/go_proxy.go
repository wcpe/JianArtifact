package protocol

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// GoProxyHandler 提供只读 GOPROXY 端点。缓存与上游回源由 AssetService 统一处理。
type GoProxyHandler struct {
	*RawHandler
}

func NewGoProxyHandler(raw *RawHandler) *GoProxyHandler { return &GoProxyHandler{RawHandler: raw} }

// RegisterGoProxyRoutes 注册 /go/<repo>/<escaped-module>/@v/<endpoint>。
func RegisterGoProxyRoutes(r gin.IRouter, h *GoProxyHandler, mw ...gin.HandlerFunc) {
	g := r.Group("/go", mw...)
	g.GET("/:repo/*rest", h.Get)
	g.HEAD("/:repo/*rest", h.Get)
}

func (h *GoProxyHandler) Get(c *gin.Context) {
	repoName := c.Param("repo")
	if !h.authorize(c, repoName, "read") {
		return
	}
	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if repo.Format != "gomod" || repo.Type != "proxy" {
		auth.WriteError(c, http.StatusNotFound, "not_found", "Go modules 仓库不存在")
		return
	}
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if !validGoProxyPath(rest) {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "Go modules 代理路径非法")
		return
	}
	asset, rc, err := h.assets.Resolve(c.Request.Context(), repoName, rest)
	if err != nil {
		writeGoProxyError(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

func validGoProxyPath(rest string) bool {
	if rest == "" || strings.Contains(rest, "\\") || strings.Contains(rest, "//") || strings.Contains(rest, "..") {
		return false
	}
	idx := strings.LastIndex(rest, "/@v/")
	if idx <= 0 {
		return false
	}
	escapedModule := rest[:idx]
	endpoint := rest[idx+len("/@v/"):]
	if !validEscapedModule(escapedModule) {
		return false
	}
	switch endpoint {
	case "list", "latest":
		return true
	}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		if strings.HasSuffix(endpoint, ext) && validGoVersion(strings.TrimSuffix(endpoint, ext)) {
			return true
		}
	}
	return false
}

func validGoVersion(version string) bool {
	return version != "" && !strings.ContainsAny(version, "/\\") && !strings.Contains(version, "..") && !strings.ContainsAny(version, "?#")
}

// validEscapedModule 校验 Go module 大写转义的规范唯一表示。
func validEscapedModule(module string) bool {
	if module == "" || strings.HasPrefix(module, "/") || strings.HasSuffix(module, "/") {
		return false
	}
	for _, segment := range strings.Split(module, "/") {
		if segment == "" || strings.Contains(segment, "..") {
			return false
		}
		for i := 0; i < len(segment); i++ {
			if segment[i] == '!' {
				if i+1 >= len(segment) || segment[i+1] < 'a' || segment[i+1] > 'z' {
					return false
				}
				i++
				continue
			}
			if segment[i] >= 'A' && segment[i] <= 'Z' {
				return false
			}
		}
	}
	return true
}

func writeGoProxyError(c *gin.Context, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		auth.WriteError(c, http.StatusNotFound, "not_found", "模块或版本不存在")
		return
	}
	if errors.Is(err, domain.ErrUpstreamGone) {
		auth.WriteError(c, http.StatusGone, "gone", "模块或版本已下架")
		return
	}
	writeAssetErr(c, err)
}
