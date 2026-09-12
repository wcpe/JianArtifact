package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// publishPolicyRequest 是发布账号管理接口的请求体。pathPrefixes 为兼容旧命名，
// allowedPrefixes 优先；省略字段保留原值，空数组表示清空路径限制。
type publishPolicyRequest struct {
	WebLoginDisabled *bool    `json:"webLoginDisabled"`
	AllowedPrefixes  []string `json:"allowedPrefixes"`
	PathPrefixes     []string `json:"pathPrefixes"`
	MaxAssetsHour    *int64   `json:"maxAssetsHour"`
	MaxBytesDay      *int64   `json:"maxBytesDay"`
	MaxFileBytes     *int64   `json:"maxFileBytes"`
	ImmutableRelease *bool    `json:"immutableRelease"`
}

// publishPolicyResponse 汇总账号、仓库与发布限制，避免管理端分别更新多个资源。
type publishPolicyResponse struct {
	UserID           int64    `json:"userId"`
	Username         string   `json:"username"`
	WebLoginDisabled bool     `json:"webLoginDisabled"`
	Repository       string   `json:"repository"`
	AllowedPrefixes  []string `json:"allowedPrefixes"`
	MaxAssetsHour    int64    `json:"maxAssetsHour"`
	MaxBytesDay      int64    `json:"maxBytesDay"`
	MaxFileBytes     int64    `json:"maxFileBytes"`
	ImmutableRelease bool     `json:"immutableRelease"`
}

// GetPublishPolicy 返回用户在指定 hosted 仓库的发布策略，仅全局管理员可见。
func (h *Handlers) GetPublishPolicy(c *gin.Context, _ UserIdParam, _ string) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	userID, repoName, ok := publishPolicyPath(c)
	if !ok || h.publishPolicies == nil {
		return
	}
	user, err := h.users.Get(userID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	policy, err := h.publishPolicies.Get(userID, repoName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	repo, err := h.repos.Get(repoName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toPublishPolicyResponse(user, repoName, policy, cfg.ImmutableRelease))
}

// PutPublishPolicy 原子更新账号 Web 登录开关、路径前缀与额度。
func (h *Handlers) PutPublishPolicy(c *gin.Context, _ UserIdParam, _ string) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	userID, repoName, ok := publishPolicyPath(c)
	if !ok || h.publishPolicies == nil {
		return
	}
	var req publishPolicyRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ImmutableRelease != nil {
		auth.WriteError(c, http.StatusBadRequest, "immutable_release_moved", "不可变 Release 请在仓库配置中更新")
		return
	}
	user, err := h.users.Get(userID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	current, err := h.publishPolicies.Get(userID, repoName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if req.WebLoginDisabled != nil {
		user, err = h.users.Update(userID, "", "", req.WebLoginDisabled)
		if err != nil {
			writeDomainErr(c, err)
			return
		}
	}
	prefixes := append([]string(nil), current.PathPrefixes...)
	if req.AllowedPrefixes != nil {
		prefixes = req.AllowedPrefixes
	} else if req.PathPrefixes != nil {
		prefixes = req.PathPrefixes
	}
	assetsHour := current.MaxAssetsHour
	bytesDay := current.MaxBytesDay
	fileBytes := current.MaxFileBytes
	if req.MaxAssetsHour != nil {
		assetsHour = *req.MaxAssetsHour
	}
	if req.MaxBytesDay != nil {
		bytesDay = *req.MaxBytesDay
	}
	if req.MaxFileBytes != nil {
		fileBytes = *req.MaxFileBytes
	}
	savedPolicy := repository.PublishPolicy{
		UserID: userID, PathPrefixes: prefixes,
		MaxAssetsHour: assetsHour, MaxBytesDay: bytesDay, MaxFileBytes: fileBytes,
	}
	if _, err := h.publishPolicies.Save(savedPolicy, repoName); err != nil {
		writeDomainErr(c, err)
		return
	}
	repo, err := h.repos.Get(repoName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	policy, err := h.publishPolicies.Get(userID, repoName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, "publish_policy.update", "publish_policy", strconv.FormatInt(userID, 10)+"/"+repoName, repoName,
		"prefixes="+strconv.Itoa(len(policy.PathPrefixes)), "ok")
	c.JSON(http.StatusOK, toPublishPolicyResponse(user, repoName, policy, cfg.ImmutableRelease))
}

func publishPolicyPath(c *gin.Context) (int64, string, bool) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户 ID 非法")
		return 0, "", false
	}
	repoName := strings.TrimSpace(c.Param("repo"))
	if repoName == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库名不能为空")
		return 0, "", false
	}
	return userID, repoName, true
}

func toPublishPolicyResponse(user *repository.User, repoName string, p *repository.PublishPolicy, immutable bool) publishPolicyResponse {
	return publishPolicyResponse{
		UserID: user.ID, Username: user.Username, WebLoginDisabled: user.WebLoginDisabled,
		Repository: repoName, AllowedPrefixes: append([]string(nil), p.PathPrefixes...),
		MaxAssetsHour: p.MaxAssetsHour, MaxBytesDay: p.MaxBytesDay, MaxFileBytes: p.MaxFileBytes,
		ImmutableRelease: immutable,
	}
}
