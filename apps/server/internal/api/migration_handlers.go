package api

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// StartOfflineDirIndex 启动离线目录前置索引扫描（admin only）。
// body: { "path": "...", "mode": "full|update|rebuild" }
func (h *Handlers) StartOfflineDirIndex(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var req struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.migrations.StartOfflineIndexScan(req.Path, req.Mode); err != nil {
		writeDomainErr(c, err)
		return
	}
	meta, counts, err := h.migrations.OfflineIndexStatus(req.Path)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, offlineIndexJSON(meta, counts))
}

// GetOfflineDirIndex 查询离线目录索引状态（admin only）。query: path=
func (h *Handlers) GetOfflineDirIndex(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	path := c.Query("path")
	meta, counts, err := h.migrations.OfflineIndexStatus(path)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, offlineIndexJSON(meta, counts))
}

// CancelOfflineDirIndex 取消索引扫描（admin only）。
func (h *Handlers) CancelOfflineDirIndex(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.migrations.CancelOfflineIndexScan(req.Path); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func offlineIndexJSON(meta *repository.OfflineDirIndex, counts map[string]int64) gin.H {
	if meta == nil {
		return gin.H{"status": "idle"}
	}
	errMsg := ""
	if meta.ErrorMessage.Valid {
		errMsg = meta.ErrorMessage.String
	}
	started, finished := "", ""
	if meta.StartedAt.Valid {
		started = meta.StartedAt.String
	}
	if meta.FinishedAt.Valid {
		finished = meta.FinishedAt.String
	}
	repos := make([]gin.H, 0, len(counts))
	for name, n := range counts {
		repos = append(repos, gin.H{"name": name, "assets": n})
	}
	return gin.H{
		"path":         meta.RootPath,
		"status":       meta.Status,
		"mode":         meta.Mode,
		"totalEntries": meta.TotalEntries,
		"scannedProps": meta.ScannedProps,
		"repoCount":    meta.RepoCount,
		"message":      meta.Message,
		"errorMessage": errMsg,
		"startedAt":    started,
		"finishedAt":   finished,
		"updatedAt":    meta.UpdatedAt,
		"repositories": repos,
	}
}

// ListRemoteNexusRepositories 从在线 Nexus 拉取仓库索引（admin only）。
// 不创建迁移任务、不扫 blob；供离线目录迁移勾选 includeRepositories。
// 路由在 httpserver 额外注册（非 OpenAPI 生成，避免改契约生成链）。
func (h *Handlers) ListRemoteNexusRepositories(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var req struct {
		URL           string `json:"url"`
		CredentialRef string `json:"credentialRef"`
	}
	if !bindJSON(c, &req) {
		return
	}
	items, err := h.migrations.ListRemoteRepositories(c.Request.Context(), req.URL, req.CredentialRef)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	type item struct {
		Name   string `json:"name"`
		Format string `json:"format"`
		Type   string `json:"type"`
	}
	out := make([]item, 0, len(items))
	for _, r := range items {
		out = append(out, item{Name: r.Name, Format: r.Format, Type: r.Type})
	}
	c.JSON(http.StatusOK, gin.H{
		"items": out,
		"total": len(out),
	})
}

// ListMigrations 迁移任务列表（admin only）。
func (h *Handlers) ListMigrations(c *gin.Context, params ListMigrationsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	rows, total, err := h.migrations.List(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]MigrationTask, 0, len(rows))
	for i := range rows {
		items = append(items, toAPIMigrationTask(&rows[i]))
	}
	c.JSON(http.StatusOK, MigrationTaskList{Items: items, Total: total})
}

// CreateMigration 创建 planned 任务（admin only）。
func (h *Handlers) CreateMigration(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var req CreateMigrationRequest
	if !bindJSON(c, &req) {
		return
	}
	in := domain.MigrationCreateInput{
		SourceType: string(req.SourceType),
	}
	if req.SourceConfig != nil {
		in.SourceConfig = map[string]any(*req.SourceConfig)
	}
	if req.CredentialRef != nil {
		in.CredentialRef = *req.CredentialRef
	}
	if req.ConflictPolicy != nil {
		in.ConflictPolicy = string(*req.ConflictPolicy)
	}
	if req.Plan != nil {
		b, err := json.Marshal(req.Plan)
		if err != nil {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "plan 序列化失败")
			return
		}
		in.PlanJSON = string(b)
	}
	task, err := h.migrations.Create(in)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAPIMigrationTask(task))
}

// DiscoverMigrations 同步三来源发现并落库 planned（admin only）。
func (h *Handlers) DiscoverMigrations(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var req MigrationDiscoverRequest
	if !bindJSON(c, &req) {
		return
	}
	in := domain.MigrationDiscoverInput{
		SourceType: string(req.SourceType),
	}
	if req.SourceConfig != nil {
		in.SourceConfig = map[string]any(*req.SourceConfig)
	}
	if req.CredentialRef != nil {
		in.CredentialRef = *req.CredentialRef
	}
	if req.ConflictPolicy != nil {
		in.ConflictPolicy = string(*req.ConflictPolicy)
	}
	result, err := h.migrations.Discover(c.Request.Context(), in)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 将 domain plan 转为契约 MigrationPlan
	apiPlan := MigrationPlan{
		Repositories: make([]MigrationPlanRepository, 0, len(result.Plan.Repositories)),
		Warnings:     result.Plan.Warnings,
		Stats:        result.Plan.Stats,
	}
	if result.Plan.Estimated {
		est := true
		apiPlan.Estimated = &est
	}
	for _, r := range result.Plan.Repositories {
		item := MigrationPlanRepository{
			Name:   r.Name,
			Format: MigrationPlanRepositoryFormat(r.Format),
		}
		if r.Type != "" {
			typ := MigrationPlanRepositoryType(r.Type)
			item.Type = &typ
		}
		if r.EstimatedAssets > 0 {
			n := r.EstimatedAssets
			item.EstimatedAssets = &n
		}
		apiPlan.Repositories = append(apiPlan.Repositories, item)
	}
	if apiPlan.Warnings == nil {
		apiPlan.Warnings = []string{}
	}
	if apiPlan.Stats == nil {
		apiPlan.Stats = map[string]interface{}{}
	}
	c.JSON(http.StatusOK, MigrationDiscoverResponse{
		TaskId: result.Task.ID,
		Plan:   apiPlan,
	})
}

// GetMigration 任务详情（admin only）。
func (h *Handlers) GetMigration(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	task, err := h.migrations.Get(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationTask(task))
}

// StartMigration planned → running（admin only）。
// 可选 body.includeRepositories：启动前多选收窄 plan（在线/离线均适用）。
func (h *Handlers) StartMigration(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	var include []string
	// body 可选；空 body / 非法 JSON 均按不收窄处理（兼容旧客户端）
	var req StartMigrationRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err == nil && req.IncludeRepositories != nil {
			include = *req.IncludeRepositories
		}
	}
	task, err := h.migrations.Start(id, include)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationTask(task))
}

// ResumeMigration failed/cancelled → running（admin only）。
func (h *Handlers) ResumeMigration(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	task, err := h.migrations.Resume(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationTask(task))
}

// CancelMigration 取消任务（admin only）。
func (h *Handlers) CancelMigration(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	task, err := h.migrations.Cancel(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationTask(task))
}

// GetMigrationReport 返回报告（foundation：透传 report_json 或空 totals）。
func (h *Handlers) GetMigrationReport(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	task, err := h.migrations.Get(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationReport(task))
}

// FinalizeMigration 切换窗口增量（admin only，仅 completed）。
func (h *Handlers) FinalizeMigration(c *gin.Context, id MigrationIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.migrations == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "迁移服务未启用")
		return
	}
	task, err := h.migrations.Finalize(c.Request.Context(), id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIMigrationTask(task))
}

func toAPIMigrationTask(t *repository.MigrationTask) MigrationTask {
	out := MigrationTask{
		Id:             t.ID,
		Status:         MigrationTaskStatus(t.Status),
		SourceType:     MigrationSourceType(t.SourceType),
		ConflictPolicy: MigrationConflictPolicy(t.ConflictPolicy),
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
	if t.CredentialRef.Valid && t.CredentialRef.String != "" {
		ref := t.CredentialRef.String
		out.CredentialRef = &ref
	}
	if t.ErrorMessage.Valid && t.ErrorMessage.String != "" {
		msg := t.ErrorMessage.String
		out.ErrorMessage = &msg
	}
	if t.StartedAt.Valid && t.StartedAt.String != "" {
		s := t.StartedAt.String
		out.StartedAt = &s
	}
	if t.FinishedAt.Valid && t.FinishedAt.String != "" {
		s := t.FinishedAt.String
		out.FinishedAt = &s
	}
	if t.SourceConfig != "" && t.SourceConfig != "{}" {
		var cfg MigrationSourceConfig
		if err := json.Unmarshal([]byte(t.SourceConfig), &cfg); err == nil {
			out.SourceConfig = &cfg
		}
	}
	if t.PlanJSON != "" && t.PlanJSON != "{}" {
		var plan MigrationPlan
		if err := json.Unmarshal([]byte(t.PlanJSON), &plan); err == nil {
			// 保证切片非 nil 以满足 required
			if plan.Repositories == nil {
				plan.Repositories = []MigrationPlanRepository{}
			}
			if plan.Warnings == nil {
				plan.Warnings = []string{}
			}
			if plan.Stats == nil {
				plan.Stats = map[string]interface{}{}
			}
			out.Plan = &plan
		}
	}
	return out
}

func toAPIMigrationReport(t *repository.MigrationTask) MigrationReport {
	st := MigrationSourceType(t.SourceType)
	cp := MigrationConflictPolicy(t.ConflictPolicy)
	out := MigrationReport{
		TaskId:         t.ID,
		Status:         MigrationTaskStatus(t.Status),
		SourceType:     &st,
		ConflictPolicy: &cp,
		Totals: map[string]interface{}{
			"copied":  0,
			"skipped": 0,
			"failed":  0,
		},
	}
	if t.StartedAt.Valid {
		s := t.StartedAt.String
		out.StartedAt = &s
	}
	if t.FinishedAt.Valid {
		s := t.FinishedAt.String
		out.FinishedAt = &s
	}
	// Runner 写入的 report_json 为 {copied,skipped,failed,failures,phase,total}
	if t.ReportJSON != "" && t.ReportJSON != "{}" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(t.ReportJSON), &raw); err == nil {
			out.Raw = &raw
			if v, ok := raw["copied"]; ok {
				out.Totals["copied"] = v
			}
			if v, ok := raw["skipped"]; ok {
				out.Totals["skipped"] = v
			}
			if v, ok := raw["failed"]; ok {
				out.Totals["failed"] = v
			}
			if v, ok := raw["phase"]; ok {
				out.Totals["phase"] = v
			}
			if v, ok := raw["total"]; ok {
				out.Totals["total"] = v
			}
			for _, k := range []string{"found", "processed", "total", "percent", "phase", "message", "currentRepo"} {
				if v, ok := raw[k]; ok {
					out.Totals[k] = v
				}
			}
			if totals, ok := raw["totals"].(map[string]interface{}); ok {
				for k, v := range totals {
					out.Totals[k] = v
				}
			}
			if fails, ok := raw["failures"].([]interface{}); ok {
				list := make([]map[string]interface{}, 0, len(fails))
				for _, f := range fails {
					if m, ok := f.(map[string]interface{}); ok {
						list = append(list, m)
					}
				}
				out.Failures = &list
			}
			if delta, ok := raw["delta"].(map[string]interface{}); ok {
				// 放入 cutover.delta
				cutover := map[string]interface{}{
					"checklist": defaultCutoverChecklist(),
					"delta":     delta,
				}
				out.Cutover = &cutover
			}
		}
	}
	if out.Cutover == nil {
		cutover := map[string]interface{}{
			"checklist": defaultCutoverChecklist(),
			"delta":     nil,
		}
		out.Cutover = &cutover
	}
	return out
}

func defaultCutoverChecklist() []string {
	return []string{
		"将 CI / 客户端 registry 指向本 JianArtifact 实例",
		"将源 Nexus 置为只读（或断开写入）",
		"执行 finalize 增量补齐切换窗口新增制品",
		"抽样校验关键路径可下载且校验和一致",
		"确认备份策略覆盖 SQLite 与 blob 目录",
	}
}
