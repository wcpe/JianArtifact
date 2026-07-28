// npm registry 级端点（FR-82）：挂在 registry 基址 `/npm/:repo/` 下、路径以 `-/`
// 开头的标准端点——ping / whoami / login / dist-tag / search / audit 兜底，以及
// unpublish 系列（修订写与删除，路径不以 `-/` 开头但同属本文件职责）。
//
// 设计要点：
//   - login（PUT /-/user/org.couchdb.user:<name>）验证用户名+口令后签发真 API Token
//     （复用 TokenService，后台可见可吊销）；npm≥9 默认 web 登录流 POST /-/v1/login
//     得到 404 后自动回落本 legacy 流。
//   - unpublish 走 npm 客户端三步流：修订 PUT 替换写 packument → DELETE 单 tarball
//     → 整包 DELETE；均需 write 权限且限 hosted 仓库。
//   - audit 端点固定返回空报告（本系统无漏洞库），消除 install 时客户端告警。
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// npmInstallMediaType 是 abbreviated packument 的媒体类型（install 加速）。
const npmInstallMediaType = "application/vnd.npm.install-v1+json"

// registryGet 分派 GET/HEAD 的 registry 级端点。
func (h *NpmHandler) registryGet(c *gin.Context, repoName, rest string) {
	switch {
	case rest == "-/ping":
		c.JSON(http.StatusOK, gin.H{})
	case rest == "-/whoami":
		p, ok := auth.PrincipalFrom(c)
		if !ok {
			writeUnauthorized(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"username": p.Username})
	case rest == "-/v1/search":
		h.registrySearch(c, repoName)
	default:
		if pkg, tag, ok := splitDistTags(rest); ok && tag == "" {
			h.distTagsGet(c, repoName, pkg)
			return
		}
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
	}
}

// registryPut 分派 PUT 的 registry 级端点（login / dist-tag 写）。
func (h *NpmHandler) registryPut(c *gin.Context, repoName, rest string) {
	if strings.HasPrefix(rest, "-/user/org.couchdb.user:") {
		h.registryLogin(c)
		return
	}
	if pkg, tag, ok := splitDistTags(rest); ok && tag != "" {
		h.distTagPut(c, repoName, pkg, tag)
		return
	}
	auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
}

// registryPost 分派 POST 的 registry 级端点（audit 兜底）。
// /-/v1/login 不实现，走 default 404：npm≥9 会自动回落 legacy adduser 流。
func (h *NpmHandler) registryPost(c *gin.Context, repoName, rest string) {
	switch rest {
	case "-/npm/v1/security/advisories/bulk":
		if !h.authorize(c, repoName, "read") {
			return
		}
		c.JSON(http.StatusOK, gin.H{})
	case "-/npm/v1/security/audits/quick":
		if !h.authorize(c, repoName, "read") {
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"actions":    []any{},
			"advisories": gin.H{},
			"muted":      []any{},
			"metadata": gin.H{
				"vulnerabilities": gin.H{
					"info": 0, "low": 0, "moderate": 0, "high": 0, "critical": 0,
				},
				"dependencies":         0,
				"devDependencies":      0,
				"optionalDependencies": 0,
				"totalDependencies":    0,
			},
		})
	default:
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
	}
}

// registryDelete 分派 DELETE 的 registry 级端点（dist-tag 删除）。
func (h *NpmHandler) registryDelete(c *gin.Context, repoName, rest string) {
	if pkg, tag, ok := splitDistTags(rest); ok && tag != "" {
		h.distTagDelete(c, repoName, pkg, tag)
		return
	}
	auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
}

// registryLogin 处理 npm login/adduser（legacy 流）：验证用户名+口令后签发
// API Token 返回，npm 自动写入 .npmrc；口令错误 401（不带 Basic 质询）。
func (h *NpmHandler) registryLogin(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取请求体失败")
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if jerr := json.Unmarshal(body, &req); jerr != nil || req.Name == "" || req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "用户名与口令不能为空")
		return
	}
	p, err := h.store.PrincipalByPassword(req.Name, req.Password)
	if err != nil {
		auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "用户名或口令错误")
		return
	}
	plaintext, _, err := h.tokens.Create(p.UserID, "npm login "+req.Name+"@"+time.Now().Format("2006-01-02 15:04"))
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"ok":    true,
		"id":    "org.couchdb.user:" + req.Name,
		"token": plaintext,
	})
}

// splitDistTags 解析 dist-tags 路径：`-/package/<pkg>/dist-tags[/<tag>]`。
// pkg 可含 `/`（scoped 包 @scope/name），故以最后一个 `/dist-tags` 分隔。
func splitDistTags(rest string) (pkg, tag string, ok bool) {
	const prefix = "-/package/"
	if !strings.HasPrefix(rest, prefix) {
		return "", "", false
	}
	rest = rest[len(prefix):]
	idx := strings.LastIndex(rest, "/dist-tags")
	if idx <= 0 {
		return "", "", false
	}
	pkg = rest[:idx]
	tail := rest[idx+len("/dist-tags"):]
	switch {
	case tail == "":
		return pkg, "", true
	case strings.HasPrefix(tail, "/") && len(tail) > 1:
		return pkg, tail[1:], true
	default:
		return "", "", false
	}
}

// distTagsGet 返回包的 dist-tags 映射（`npm dist-tag ls`）。
func (h *NpmHandler) distTagsGet(c *gin.Context, repoName, pkg string) {
	if !h.authorize(c, repoName, "read") {
		return
	}
	doc, ok := h.loadPackument(c, repoName, pkg)
	if !ok {
		return
	}
	tags := subMap(doc, "dist-tags")
	if tags == nil {
		tags = map[string]any{}
	}
	c.JSON(http.StatusOK, tags)
}

// distTagPut 新增/更新标签（`npm dist-tag add`）：body 为版本号 JSON 字符串，
// 版本必须已存在于 versions。
func (h *NpmHandler) distTagPut(c *gin.Context, repoName, pkg, tag string) {
	if !h.authorize(c, repoName, "write") {
		return
	}
	if !h.requireHosted(c, repoName) {
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取请求体失败")
		return
	}
	var version string
	if jerr := json.Unmarshal(body, &version); jerr != nil || version == "" {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "请求体须为版本号 JSON 字符串")
		return
	}
	doc, ok := h.loadPackument(c, repoName, pkg)
	if !ok {
		return
	}
	if _, exists := subMap(doc, "versions")[version]; !exists {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "版本不存在："+version)
		return
	}
	ensureMap(doc, "dist-tags")[tag] = version
	if !h.savePackument(c, repoName, pkg, doc) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

// distTagDelete 删除标签（`npm dist-tag rm`）；latest 拒删。
func (h *NpmHandler) distTagDelete(c *gin.Context, repoName, pkg, tag string) {
	if !h.authorize(c, repoName, "write") {
		return
	}
	if !h.requireHosted(c, repoName) {
		return
	}
	if tag == "latest" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "latest 标签不可删除")
		return
	}
	doc, ok := h.loadPackument(c, repoName, pkg)
	if !ok {
		return
	}
	tags := subMap(doc, "dist-tags")
	if _, exists := tags[tag]; !exists {
		auth.WriteError(c, http.StatusNotFound, "not_found", "标签不存在："+tag)
		return
	}
	delete(tags, tag)
	if !h.savePackument(c, repoName, pkg, doc) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// unpublishRevPut 处理 `npm unpublish <pkg>@<ver>` 的修订写：body 为剔除目标
// 版本后的完整 packument，替换写（非合并）；随后客户端会单独 DELETE tarball。
func (h *NpmHandler) unpublishRevPut(c *gin.Context, repoName, pkg string) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "读取请求体失败")
		return
	}
	var doc map[string]any
	if jerr := json.Unmarshal(body, &doc); jerr != nil {
		auth.WriteError(c, http.StatusBadRequest, "invalid_body", "请求体非合法 JSON")
		return
	}
	delete(doc, "_attachments")
	if !h.savePackument(c, repoName, pkg, doc) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

// unpublishTarball 删除单个 tarball（unpublish 单版本第三步）。
func (h *NpmHandler) unpublishTarball(c *gin.Context, repoName, pkg, file string) {
	// file 尾部的 /-rev/<rev> 已由调用方剥离，此处 file 即 tarball 文件名。
	if err := h.assets.Delete(repoName, pkg+"/-/"+file); err != nil {
		writeAssetErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// unpublishPackage 整包删除（`npm unpublish <pkg> --force`）：删 packument 与全部 tarball。
func (h *NpmHandler) unpublishPackage(c *gin.Context, repoName, pkg string) {
	paths, err := h.assets.ListPathsByPrefix(repoName, pkg+"/-/")
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	for _, p := range paths {
		_ = h.assets.Delete(repoName, p)
	}
	if err := h.assets.Delete(repoName, pkg); err != nil {
		writeAssetErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// registrySearch 处理 `npm search`（GET /-/v1/search?text=&size=&from=）：
// 在本仓（group 则各成员）packument 中按路径子串检索，组装 npm 标准响应。
func (h *NpmHandler) registrySearch(c *gin.Context, repoName string) {
	if !h.authorize(c, repoName, "read") {
		return
	}
	text := strings.TrimSpace(c.Query("text"))
	size := clampAtoi(c.Query("size"), 20, 1, 250)
	from := clampAtoi(c.Query("from"), 0, 0, 10000)
	empty := gin.H{"objects": []any{}, "total": 0, "time": time.Now().UTC().Format(http.TimeFormat)}
	if text == "" {
		c.JSON(http.StatusOK, empty)
		return
	}
	repo, err := h.repoSvc.Get(repoName)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	members := []string{repoName}
	if repo.Type == "group" {
		cfg, cerr := repo.DecodeConfig()
		if cerr != nil {
			writeAssetErr(c, cerr)
			return
		}
		members = cfg.Members
	}
	// 排除 tarball 路径（含 /-/），仅命中 packument（路径即包名）。
	keyword := text + ` -"/-/"`
	objects := make([]any, 0, size)
	seen := map[string]bool{}
	total := 0
	for _, member := range members {
		// 仓库级 read 已由 authorize 判定，isAdmin=true 跳过二次 ACL；
		// repoScope 限定单仓库保证不越权。
		out, serr := h.repoSvc.SearchAssets(keyword, member, 0, true, "path", "asc", from+size, 0)
		if serr != nil {
			continue
		}
		for _, item := range out.Items {
			if seen[item.Asset.Path] {
				continue
			}
			seen[item.Asset.Path] = true
			obj := h.searchObject(member, item.Asset.Path)
			if obj == nil {
				continue
			}
			total++
			if total > from && len(objects) < size {
				objects = append(objects, obj)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"objects": objects,
		"total":   total,
		"time":    time.Now().UTC().Format(http.TimeFormat),
	})
}

// searchObject 读取 packument 提炼搜索条目；非 packument（解析失败/无 name）返回 nil。
func (h *NpmHandler) searchObject(repoName, pkg string) gin.H {
	_, rc, err := h.assets.Get(repoName, pkg)
	if err != nil {
		return nil
	}
	data, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	name, _ := doc["name"].(string)
	if name == "" {
		return nil
	}
	version, _ := subMap(doc, "dist-tags")["latest"].(string)
	description := ""
	date := ""
	if vm, ok := subMap(doc, "versions")[version].(map[string]any); ok {
		description, _ = vm["description"].(string)
	}
	if m, ok := subMap(doc, "time")["modified"].(string); ok {
		date = m
	}
	return gin.H{
		"package": gin.H{
			"name":        name,
			"version":     version,
			"description": description,
			"date":        date,
			"links":       gin.H{},
			"maintainers": []any{},
		},
		"score": gin.H{
			"final":  1,
			"detail": gin.H{"quality": 1, "popularity": 1, "maintenance": 1},
		},
		"searchScore": 1,
	}
}

// loadPackument 读取并解析本仓 packument；失败时已写出响应，返回 ok=false。
func (h *NpmHandler) loadPackument(c *gin.Context, repoName, pkg string) (map[string]any, bool) {
	_, rc, err := h.assets.Resolve(c.Request.Context(), repoName, pkg)
	if err != nil {
		writeAssetErr(c, err)
		return nil, false
	}
	data, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return nil, false
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "packument 文档非法")
		return nil, false
	}
	return doc, true
}

// savePackument 序列化并覆盖写 packument；失败时已写出响应，返回 false。
func (h *NpmHandler) savePackument(c *gin.Context, repoName, pkg string, doc map[string]any) bool {
	out, err := json.Marshal(doc)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return false
	}
	if _, err := h.assets.Put(repoName, pkg, bytes.NewReader(out), "application/json"); err != nil {
		writeAssetErr(c, err)
		return false
	}
	return true
}

// wantsAbbreviated 判断客户端是否要求 abbreviated packument（install 加速）。
func wantsAbbreviated(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept"), npmInstallMediaType)
}

// abbreviatedVersionFields 是 abbreviated packument 每个 version 保留的字段白名单
// （npm install 解析依赖所需的最小集合）。
var abbreviatedVersionFields = []string{
	"name", "version", "dependencies", "optionalDependencies", "devDependencies",
	"peerDependencies", "peerDependenciesMeta", "bundleDependencies", "bundledDependencies",
	"bin", "directories", "dist", "engines", "deprecated", "hasInstallScript",
	"os", "cpu", "funding", "_hasShrinkwrap", "acceptDependencies",
}

// abbreviatePackument 按白名单裁剪 packument：顶层保留 name/dist-tags/modified/versions，
// version 级仅保留 install 所需字段。
func abbreviatePackument(doc map[string]any) map[string]any {
	out := map[string]any{
		"name":      doc["name"],
		"dist-tags": doc["dist-tags"],
	}
	if m, ok := subMap(doc, "time")["modified"]; ok {
		out["modified"] = m
	}
	versions := map[string]any{}
	for v, raw := range subMap(doc, "versions") {
		vm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		slim := map[string]any{}
		for _, f := range abbreviatedVersionFields {
			if val, exists := vm[f]; exists {
				slim[f] = val
			}
		}
		versions[v] = slim
	}
	out["versions"] = versions
	return out
}

// clampAtoi 解析十进制参数并夹取到 [min, max]；解析失败返回 def。
func clampAtoi(s string, def, min, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
