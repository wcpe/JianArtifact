package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
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
	// 单仓库与批量端点共用同一套「合并 + 校验」路径：publishPolicyRequest.patch 负责把
	// 「省略 / 显式空数组」翻译成补丁，domain.PublishPolicyPatch.Apply 负责合并，
	// Save 负责校验。规则只有一份，端点之间不会漂移。
	policy, err := h.publishPolicies.Save(req.patch().Apply(*current, userID), repoName)
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
	h.AuditLog(c, "publish_policy.update", "publish_policy", strconv.FormatInt(userID, 10)+"/"+repoName, repoName,
		"prefixes="+strconv.Itoa(len(policy.PathPrefixes)), "ok")
	c.JSON(http.StatusOK, toPublishPolicyResponse(user, repoName, policy, cfg.ImmutableRelease))
}

// patch 把请求体翻译成策略补丁。协议含义：字段缺省即 nil，保留目标仓库现有值；
// 显式提交（含空数组/零值）即覆盖。allowedPrefixes 为当前命名，pathPrefixes 为兼容别名，
// 二者同时出现时以 allowedPrefixes 为准；空数组表示清空路径限制。
func (r publishPolicyRequest) patch() domain.PublishPolicyPatch {
	p := domain.PublishPolicyPatch{
		MaxAssetsHour: r.MaxAssetsHour,
		MaxBytesDay:   r.MaxBytesDay,
		MaxFileBytes:  r.MaxFileBytes,
	}
	switch {
	case r.AllowedPrefixes != nil:
		p.PathPrefixes = &r.AllowedPrefixes
	case r.PathPrefixes != nil:
		p.PathPrefixes = &r.PathPrefixes
	}
	return p
}

// PutPublishPolicies 把同一份发布策略批量应用到多个 Hosted 仓库，并逐仓库返回结果。
//
// 为什么返回逐仓库结果而不是单个布尔：批量写入可能出现个别仓库失败，静默成功会掩盖
// 不一致；逐条结果让调用方（管理端 UI）能精确提示「哪个仓库失败、为什么」。
// 契约见 PUT /api/v1/users/{id}/publish-policies。
func (h *Handlers) PutPublishPolicies(c *gin.Context, _ UserIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	userID, ok := publishPolicyUserID(c)
	if !ok || h.publishPolicies == nil {
		return
	}
	var req PublishPoliciesBatchRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ImmutableRelease != nil {
		auth.WriteError(c, http.StatusBadRequest, "immutable_release_moved", "不可变 Release 请在仓库配置中更新")
		return
	}
	if len(req.Repositories) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "至少选择一个仓库")
		return
	}
	if _, err := h.users.Get(userID); err != nil {
		writeDomainErr(c, err)
		return
	}
	// 禁止 Web 登录是账号级开关（不属于「用户 × 仓库」策略），批量保存时只更新一次。
	// 顺序与单仓库端点一致（先账号开关、再仓库策略）；策略侧由服务层整体预校验保证
	// 「要么全写、要么全不写」，账号开关与其是不同资源，失败时不会被静默回滚。
	if req.WebLoginDisabled != nil {
		if _, err := h.users.Update(userID, "", "", req.WebLoginDisabled); err != nil {
			writeDomainErr(c, err)
			return
		}
	}
	results, err := h.publishPolicies.SaveMany(userID, batchPatch(req), req.Repositories)
	if err != nil {
		writePublishPolicyBatchErr(c, err)
		return
	}
	out := PublishPoliciesBatchResponse{Results: make([]PublishPolicyBatchResult, 0, len(results))}
	succeeded := 0
	for _, r := range results {
		item := PublishPolicyBatchResult{Repository: r.Repository, Ok: r.OK}
		if r.Error != "" {
			// 单独取址：不能复用循环变量，否则所有失败项会指向同一字符串。
			message := r.Error
			item.Error = &message
		}
		if r.OK {
			succeeded++
		}
		out.Results = append(out.Results, item)
	}
	// 逐仓库结果可能部分失败：全部成功才记 ok，否则记 partial（分类时归入失败），
	// 避免按 result=ok 过滤的审计视图把「部分失败」计为成功。
	auditResult := "ok"
	if succeeded < len(results) {
		auditResult = "partial"
	}
	h.AuditLog(c, "publish_policy.update", "publish_policy", strconv.FormatInt(userID, 10),
		strings.Join(req.Repositories, ","),
		fmt.Sprintf("repositories=%d succeeded=%d", len(results), succeeded), auditResult)
	c.JSON(http.StatusOK, out)
}

// batchPatch 把批量请求体翻译成策略补丁，口径与单仓库端点一致。
func batchPatch(req PublishPoliciesBatchRequest) domain.PublishPolicyPatch {
	p := domain.PublishPolicyPatch{
		MaxAssetsHour: req.MaxAssetsHour,
		MaxBytesDay:   req.MaxBytesDay,
		MaxFileBytes:  req.MaxFileBytes,
	}
	switch {
	case req.AllowedPrefixes != nil:
		p.PathPrefixes = req.AllowedPrefixes
	case req.PathPrefixes != nil:
		p.PathPrefixes = req.PathPrefixes
	}
	return p
}

// writePublishPolicyBatchErr 输出批量保存的预校验失败：状态码沿用哨兵错误映射，但消息使用
// 批量错误自身的文案（点名具体仓库），这是本端点「失败可见」的关键；非批量错误交回通用映射。
func writePublishPolicyBatchErr(c *gin.Context, err error) {
	var batchErr *domain.PublishPolicyBatchError
	if !errors.As(err, &batchErr) {
		writeDomainErr(c, err)
		return
	}
	switch {
	case errors.Is(batchErr.Err, domain.ErrNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", batchErr.Error())
	case errors.Is(batchErr.Err, domain.ErrValidation):
		auth.WriteError(c, http.StatusBadRequest, "validation_error", batchErr.Error())
	default:
		writeDomainErr(c, err)
	}
}

// publishPolicyUserID 解析路径中的用户 ID。
func publishPolicyUserID(c *gin.Context) (int64, bool) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户 ID 非法")
		return 0, false
	}
	return userID, true
}

func publishPolicyPath(c *gin.Context) (int64, string, bool) {
	userID, ok := publishPolicyUserID(c)
	if !ok {
		return 0, "", false
	}
	repoName := strings.TrimSpace(c.Param("repo"))
	if repoName == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库名不能为空")
		return 0, "", false
	}
	return userID, repoName, true
}

// nonNilPrefixes 保证路径前缀切片非 nil，供响应构造使用。
//
// 不能用 append([]string(nil), prefixes...) 复制：append 在追加 0 个元素时原样返回目标切片，
// 而这里的目标恰好是 nil ——「空」会被悄悄变回 nil。契约把 allowedPrefixes 定为 required 数组，
// nil 会序列化成 null，既违约，也会让消费方（前端 value.allowedPrefixes.join(...)）直接抛
// TypeError: Cannot read properties of null (reading 'join')。
func nonNilPrefixes(prefixes []string) []string {
	if prefixes == nil {
		return []string{}
	}
	return prefixes
}

func toPublishPolicyResponse(user *repository.User, repoName string, p *repository.PublishPolicy, immutable bool) publishPolicyResponse {
	return publishPolicyResponse{
		UserID: user.ID, Username: user.Username, WebLoginDisabled: user.WebLoginDisabled,
		Repository: repoName, AllowedPrefixes: nonNilPrefixes(p.PathPrefixes),
		MaxAssetsHour: p.MaxAssetsHour, MaxBytesDay: p.MaxBytesDay, MaxFileBytes: p.MaxFileBytes,
		ImmutableRelease: immutable,
	}
}
