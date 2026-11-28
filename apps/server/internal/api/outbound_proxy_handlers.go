package api

import (
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// ListOutboundProxies 查询全局出站代理池与本机 Mihomo 运行时能力（仅管理员）。
func (h *Handlers) ListOutboundProxies(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.outboundProxies == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "出站代理管理未启用")
		return
	}
	views, err := h.outboundProxies.List()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	caps := h.outboundProxies.Capabilities(c.Request.Context())
	items := make([]OutboundProxy, 0, len(views))
	for i := range views {
		items = append(items, toAPIOutboundProxy(&views[i]))
	}
	resp := OutboundProxyList{
		Items:           items,
		MihomoAvailable: caps.Available,
	}
	if caps.Version != "" {
		v := caps.Version
		resp.MihomoVersion = &v
	}
	if caps.BinaryPath != "" {
		b := caps.BinaryPath
		resp.MihomoBinary = &b
	}
	c.JSON(http.StatusOK, resp)
}

// CreateOutboundProxy 新增全局出站代理（仅管理员）。
func (h *Handlers) CreateOutboundProxy(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.outboundProxies == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "出站代理管理未启用")
		return
	}
	var req CreateOutboundProxyRequest
	if !bindJSON(c, &req) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	desc := ""
	if req.Description != nil {
		desc = *req.Description
	}
	view, err := h.outboundProxies.Create(domain.CreateOutboundProxyInput{
		Name:        req.Name,
		Kind:        string(req.Kind),
		Description: desc,
		Config:      req.Config,
		Enabled:     enabled,
	})
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, "outbound_proxy.create", "outbound_proxy", strconv.FormatInt(view.ID, 10), "",
		"name="+view.Name+" kind="+view.Kind+" preview="+view.EndpointPreview, "ok")
	c.JSON(http.StatusCreated, toAPIOutboundProxy(view))
}

// UpdateOutboundProxy 更新全局出站代理（仅管理员）。
func (h *Handlers) UpdateOutboundProxy(c *gin.Context, id OutboundProxyIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.outboundProxies == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "出站代理管理未启用")
		return
	}
	var req UpdateOutboundProxyRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == nil && req.Kind == nil && req.Description == nil && req.Config == nil && req.Enabled == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少可更新字段")
		return
	}
	in := domain.UpdateOutboundProxyInput{
		Name:        req.Name,
		Description: req.Description,
		Config:      req.Config,
		Enabled:     req.Enabled,
	}
	if req.Kind != nil {
		k := string(*req.Kind)
		in.Kind = &k
	}
	view, err := h.outboundProxies.Update(id, in)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, "outbound_proxy.update", "outbound_proxy", strconv.FormatInt(view.ID, 10), "",
		"name="+view.Name+" kind="+view.Kind+" enabled="+strconv.FormatBool(view.Enabled), "ok")
	c.JSON(http.StatusOK, toAPIOutboundProxy(view))
}

// DeleteOutboundProxy 删除全局出站代理（仅管理员；若仍有仓库引用则返回 409）。
func (h *Handlers) DeleteOutboundProxy(c *gin.Context, id OutboundProxyIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.outboundProxies == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "出站代理管理未启用")
		return
	}
	if err := h.outboundProxies.Delete(id); err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, "outbound_proxy.delete", "outbound_proxy", strconv.FormatInt(id, 10), "", "", "ok")
	c.Status(http.StatusNoContent)
}

// TestOutboundProxy 通过指定出站代理测试到公网目标地址的连通性（仅管理员）。
func (h *Handlers) TestOutboundProxy(c *gin.Context, id OutboundProxyIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.outboundProxies == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "出站代理管理未启用")
		return
	}
	var req TestOutboundProxyRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式非法")
			return
		}
	}
	target := ""
	if req.TargetUrl != nil {
		target = *req.TargetUrl
	}
	res, err := h.outboundProxies.TestConnectivity(c.Request.Context(), id, target, h.upstreamClient)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	out := TestOutboundProxyResult{
		Ok:        res.OK,
		LatencyMs: res.LatencyMs,
		Message:   res.Message,
	}
	if res.StatusCode > 0 {
		code := res.StatusCode
		out.StatusCode = &code
	}
	c.JSON(http.StatusOK, out)
}

func toAPIOutboundProxy(v *domain.OutboundProxyView) OutboundProxy {
	bound := append([]string{}, v.BoundRepositories...)
	return OutboundProxy{
		Id:                v.ID,
		Name:              v.Name,
		Kind:              OutboundProxyKind(v.Kind),
		Description:       v.Description,
		EndpointPreview:   v.EndpointPreview,
		Enabled:           v.Enabled,
		BoundRepositories: bound,
		CreatedAt:         v.CreatedAt,
		UpdatedAt:         v.UpdatedAt,
	}
}
