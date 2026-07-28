// Package protocol：npm registry 格式端点。见 doc.go 分层说明。
//
// npm 客户端以 registry 基址 `<server>/npm/<repo>/` 交互，端点形态与 Raw/Maven 的
// `/repository/...` 不同，故独立注册于 `/npm` 前缀下（见 RegisterNpmRoutes），
// 经单一 catch-all 段在 handler 内解析为三类请求：
//   - packument   GET  /npm/:repo/<pkg>              （<pkg> 支持 scoped @scope/name）
//   - tarball     GET  /npm/:repo/<pkg>/-/<file>
//   - publish     PUT  /npm/:repo/<pkg>              （体含 _attachments base64 tarball）
//
// 存储模型（复用内容寻址 blob，不引入 npm 专用表）：
//   - packument 文档整体作为一件 asset 存于路径 `<pkg>`；
//   - 每个 tarball 存于路径 `<pkg>/-/<file>`（与请求/上游 npm 布局一致）。
//
// 发布覆盖写 last-writer-wins（合并累积 versions/dist-tags）；proxy 回源经 Resolve
// 缓存后于服务端重写 dist.tarball 指向本仓；group 合并成员 packument（versions 并集、
// dist-tags 首成员优先），tarball 经 group 有序命中回落各成员。
package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// NpmHandler 处理 npm registry 格式仓库。鉴权与制品存取复用内嵌 RawHandler 的
// assets/repoSvc/authorize，GET/PUT 按 npm 端点形态自行解析与组装。
// store/tokens 供 registry 级端点使用：login 验口令并签发 API Token（FR-82）。
type NpmHandler struct {
	*RawHandler
	store  auth.Store
	tokens *domain.TokenService
}

// NewNpmHandler 构造 NpmHandler。
func NewNpmHandler(raw *RawHandler, store auth.Store, tokens *domain.TokenService) *NpmHandler {
	return &NpmHandler{RawHandler: raw, store: store, tokens: tokens}
}

// RegisterNpmRoutes 在 r 上挂载 npm registry 端点（前缀 /npm，不与 /api/v1、/repository 冲突）：
//
//	GET|HEAD /npm/:repo/*rest   （packument / tarball / registry 级只读端点）
//	PUT      /npm/:repo/*rest   （publish / login / dist-tag / unpublish 修订写）
//	POST     /npm/:repo/*rest   （audit 兜底）
//	DELETE   /npm/:repo/*rest   （dist-tag 删除 / unpublish 删除）
//
// rest 以 `-/` 开头的路径分派到 registry 级端点（见 npm_registry.go）。
// mw 通常为 authenticator.Optional()（支持 Basic + Bearer）。
func RegisterNpmRoutes(r gin.IRouter, h *NpmHandler, mw ...gin.HandlerFunc) {
	grp := r.Group("/npm", mw...)
	grp.GET("/:repo/*rest", h.Get)
	grp.HEAD("/:repo/*rest", h.Get)
	grp.PUT("/:repo/*rest", h.Put)
	grp.POST("/:repo/*rest", h.Post)
	grp.DELETE("/:repo/*rest", h.Delete)
}

// Get 处理 GET/HEAD：`-/` 开头走 registry 级端点；rest 含 `/-/` 视为 tarball，否则视为 packument。
func (h *NpmHandler) Get(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if strings.HasPrefix(rest, "-/") {
		h.registryGet(c, repoName, rest)
		return
	}
	if !h.authorize(c, repoName, "read") {
		return
	}
	if pkg, file, ok := splitTarball(rest); ok {
		h.serveTarball(c, repoName, pkg, file)
		return
	}
	h.servePackument(c, repoName, rest)
}

// Put 处理 PUT：`-/` 开头走 registry 级端点（login/dist-tag）；含 `/-rev/` 为
// unpublish 修订写（替换 packument）；否则为发布到 hosted 仓库。
func (h *NpmHandler) Put(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if strings.HasPrefix(rest, "-/") {
		h.registryPut(c, repoName, rest)
		return
	}
	if !h.authorize(c, repoName, "write") {
		return
	}
	if pkg, ok := splitRev(rest); ok {
		h.unpublishRevPut(c, repoName, pkg)
		return
	}
	if rest == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "包名不能为空")
		return
	}
	h.publish(c, repoName, rest)
}

// Post 处理 POST：仅 registry 级端点（audit 兜底）；其余 404（含 /-/v1/login，
// 让 npm≥9 的 web 登录流回落 legacy adduser）。
func (h *NpmHandler) Post(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if strings.HasPrefix(rest, "-/") {
		h.registryPost(c, repoName, rest)
		return
	}
	auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
}

// Delete 处理 DELETE：`-/` 开头为 dist-tag 删除；含 `/-rev/` 为 unpublish
// （tarball 或整包删除）；其余 404。
func (h *NpmHandler) Delete(c *gin.Context) {
	repoName := c.Param("repo")
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	if strings.HasPrefix(rest, "-/") {
		h.registryDelete(c, repoName, rest)
		return
	}
	target, ok := splitRev(rest)
	if !ok {
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	if !h.authorize(c, repoName, "write") {
		return
	}
	if !h.requireHosted(c, repoName) {
		return
	}
	if pkg, file, isTarball := splitTarball(target); isTarball {
		h.unpublishTarball(c, repoName, pkg, file)
		return
	}
	h.unpublishPackage(c, repoName, target)
}

// serveTarball 流式回写 tarball 字节（经 Resolve：hosted 本地 / proxy 回源 / group 有序命中）。
func (h *NpmHandler) serveTarball(c *gin.Context, repoName, pkg, file string) {
	tarPath := pkg + "/-/" + file
	asset, rc, err := h.assets.Resolve(c.Request.Context(), repoName, tarPath)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	writeArtifact(c, asset.ContentType, asset.Size, asset.BlobHash, rc)
}

// servePackument 组装并回写 packument：group 走成员合并；其余经 Resolve 取文档后
// 重写各版本 dist.tarball 指向本仓再返回。
func (h *NpmHandler) servePackument(c *gin.Context, repoName, pkg string) {
	if pkg == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_path", "包名不能为空")
		return
	}
	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	if repo.Type == "group" {
		h.serveGroupPackument(c, repo, pkg)
		return
	}

	_, rc, err := h.assets.Resolve(c.Request.Context(), repoName, pkg)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	data, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		// 解析失败原样透传，保证代理场景健壮性。
		writePackument(c, data, "application/json")
		return
	}
	rewritePackument(doc, requestBaseURL(c), repoName, pkg)
	h.writePackumentDoc(c, doc)
}

// writePackumentDoc 按 Accept 协商序列化并回写 packument：客户端要求
// install-v1 精简媒体类型时裁剪为 abbreviated 文档（FR-82）。
func (h *NpmHandler) writePackumentDoc(c *gin.Context, doc map[string]any) {
	contentType := "application/json"
	if wantsAbbreviated(c) {
		doc = abbreviatePackument(doc)
		contentType = npmInstallMediaType
	}
	out, err := json.Marshal(doc)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	writePackument(c, out, contentType)
}

// serveGroupPackument 合并 group 各成员 packument：versions 并集（首成员优先）、
// dist-tags 首成员优先，随后重写 tarball 指向 group 本仓；全成员皆无该包→404。
func (h *NpmHandler) serveGroupPackument(c *gin.Context, repo *repository.Repository, pkg string) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	merged := map[string]any{}
	found := false
	for _, member := range cfg.Members {
		_, rc, err := h.assets.Resolve(c.Request.Context(), member, pkg)
		if err != nil {
			continue
		}
		data, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(data, &doc) != nil {
			continue
		}
		mergePackumentFirstWins(merged, doc)
		found = true
	}
	if !found {
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	rewritePackument(merged, requestBaseURL(c), repo.Name, pkg)
	h.writePackumentDoc(c, merged)
}

// publish 解析发布体：落各 _attachments tarball，合并进已存 packument（last-writer-wins），
// 覆盖写 packument 文档。仓库非 hosted 时 AssetService.Put 返回 ErrConflict→409。
func (h *NpmHandler) publish(c *gin.Context, repoName, pkg string) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取发布体失败")
		return
	}
	var incoming map[string]any
	if err := json.Unmarshal(body, &incoming); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "发布体非合法 JSON")
		return
	}

	// 1) 落 tarball（base64 解码）。
	if atts, ok := incoming["_attachments"].(map[string]any); ok {
		for name, raw := range atts {
			att, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			enc, _ := att["data"].(string)
			if enc == "" {
				continue
			}
			blob, derr := base64.StdEncoding.DecodeString(enc)
			if derr != nil {
				auth.WriteError(c, http.StatusBadRequest, "invalid_body", "_attachments 非法 base64")
				return
			}
			if _, perr := h.assets.Put(repoName, pkg+"/-/"+name, bytes.NewReader(blob), "application/octet-stream"); perr != nil {
				writeAssetErr(c, perr)
				return
			}
		}
	}
	delete(incoming, "_attachments")

	// 2) 与已存 packument 合并（累积历史 versions），覆盖写。
	merged := incoming
	if _, rc, gerr := h.assets.Get(repoName, pkg); gerr == nil {
		existingBytes, _ := io.ReadAll(rc)
		_ = rc.Close()
		var existing map[string]any
		if json.Unmarshal(existingBytes, &existing) == nil {
			mergePackumentLastWins(existing, incoming)
			merged = existing
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	if _, err := h.assets.Put(repoName, pkg, bytes.NewReader(out), "application/json"); err != nil {
		writeAssetErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true, "id": pkg})
}

// splitTarball 从 rest 解析 tarball 请求：以首个 `/-/` 分隔为 pkg 与文件名。
func splitTarball(rest string) (pkg, file string, ok bool) {
	idx := strings.Index(rest, "/-/")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+len("/-/"):], true
}

// splitRev 从 rest 解析 unpublish 修订路径：以尾部 `/-rev/` 分隔，返回其前段。
func splitRev(rest string) (target string, ok bool) {
	idx := strings.LastIndex(rest, "/-rev/")
	if idx < 0 {
		return "", false
	}
	return rest[:idx], true
}

// requireHosted 校验仓库为 hosted，否则写 409（与 publish 冲突语义一致）。
func (h *NpmHandler) requireHosted(c *gin.Context, repoName string) bool {
	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return false
	}
	if repo.Type != "hosted" {
		auth.WriteError(c, http.StatusConflict, "conflict", "仅 hosted 仓库支持该操作")
		return false
	}
	return true
}

// requestBaseURL 据请求还原对外基址（scheme://host），供 dist.tarball 重写。
func requestBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + c.Request.Host
}

// rewritePackument 把 doc 内每个 version 的 dist.tarball 重写为
// `<base>/npm/<repo>/<pkg>/-/<原文件名>`，使客户端经本仓拉取 tarball。
func rewritePackument(doc map[string]any, base, repoName, pkg string) {
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		return
	}
	for _, ver := range versions {
		vm, ok := ver.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := vm["dist"].(map[string]any)
		if !ok {
			continue
		}
		if t, ok := dist["tarball"].(string); ok && t != "" {
			dist["tarball"] = base + "/npm/" + repoName + "/" + pkg + "/-/" + path.Base(t)
		}
	}
}

// mergePackumentLastWins 把 src 覆盖并入 dst：versions/dist-tags/time 逐键覆盖，name 以 src 为准。
func mergePackumentLastWins(dst, src map[string]any) {
	if n, ok := src["name"]; ok {
		dst["name"] = n
	}
	overlayMap(ensureMap(dst, "versions"), subMap(src, "versions"), true)
	overlayMap(ensureMap(dst, "dist-tags"), subMap(src, "dist-tags"), true)
	overlayMap(ensureMap(dst, "time"), subMap(src, "time"), true)
}

// mergePackumentFirstWins 把 src 并入 dst，仅补 dst 尚无的键（首成员优先），用于 group 合并。
func mergePackumentFirstWins(dst, src map[string]any) {
	if _, ok := dst["name"]; !ok {
		if n, ok := src["name"]; ok {
			dst["name"] = n
		}
	}
	overlayMap(ensureMap(dst, "versions"), subMap(src, "versions"), false)
	overlayMap(ensureMap(dst, "dist-tags"), subMap(src, "dist-tags"), false)
	overlayMap(ensureMap(dst, "time"), subMap(src, "time"), false)
}

// overlayMap 把 src 并入 dst：overwrite 为真则逐键覆盖，否则仅补缺键。
func overlayMap(dst, src map[string]any, overwrite bool) {
	for k, v := range src {
		if !overwrite {
			if _, exists := dst[k]; exists {
				continue
			}
		}
		dst[k] = v
	}
}

// ensureMap 返回 m[key] 的 map（不存在则新建并写回）。
func ensureMap(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

// subMap 返回 m[key] 的 map（不存在或类型不符返回 nil，不写回）。
func subMap(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

// writePackument 以 contentType 回写 packument 字节；HEAD 不写 body。
func writePackument(c *gin.Context, data []byte, contentType string) {
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.Itoa(len(data)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, contentType, data)
}
