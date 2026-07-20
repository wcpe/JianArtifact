package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ListRepositories 仓库列表（分页）。管理员可见全部；普通用户仅见可读（public 或经 ACL 授权）者。
func (h *Handlers) ListRepositories(c *gin.Context, params ListRepositoriesParams) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	rows, total, err := h.repos.List(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]Repository, 0, len(rows))
	for i := range rows {
		if !p.IsAdmin() {
			allowed, err := h.repos.CanAccess(rows[i].Name, p.UserID, "read")
			if err != nil || !allowed {
				continue
			}
		}
		items = append(items, toAPIRepository(&rows[i]))
	}
	if !p.IsAdmin() {
		total = len(items)
	}
	c.JSON(http.StatusOK, RepositoryList{Items: items, Total: total})
}

// CreateRepository 创建仓库，仅管理员。
func (h *Handlers) CreateRepository(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req CreateRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库名称不能为空")
		return
	}
	visibility := ""
	if req.Visibility != nil {
		visibility = string(*req.Visibility)
	}
	repo, err := h.repos.Create(req.Name, string(req.Format), string(req.Type), visibility)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAPIRepository(repo))
}

// UpdateRepository 更新仓库可见性，仅管理员。
func (h *Handlers) UpdateRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req UpdateRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Visibility == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少可更新字段")
		return
	}
	repo, err := h.repos.Update(name, string(*req.Visibility))
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIRepository(repo))
}

// DeleteRepository 删除仓库，仅管理员。
func (h *Handlers) DeleteRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if err := h.repos.Delete(name); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// GetRepositoryAcl 读取仓库 ACL：需全局管理员或对该仓库有 admin 授权。
func (h *Handlers) GetRepositoryAcl(c *gin.Context, name RepoNameParam) {
	if _, ok := h.requireRepoAdmin(c, name); !ok {
		return
	}
	rows, err := h.repos.GetAcl(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, AclList{Items: aclEntries(rows)})
}

// SetRepositoryAcl 覆盖写入仓库 ACL：需全局管理员或对该仓库有 admin 授权。
func (h *Handlers) SetRepositoryAcl(c *gin.Context, name RepoNameParam) {
	if _, ok := h.requireRepoAdmin(c, name); !ok {
		return
	}
	var req PutAclRequest
	if !bindJSON(c, &req) {
		return
	}
	entries := make([]repository.Acl, 0, len(req.Items))
	for _, e := range req.Items {
		entries = append(entries, repository.Acl{SubjectID: e.SubjectId, Action: string(e.Action)})
	}
	rows, err := h.repos.SetAcl(name, entries)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, AclList{Items: aclEntries(rows)})
}

// requireRepoAdmin 要求主体对仓库有管理权：全局管理员或该仓库 admin ACL。
func (h *Handlers) requireRepoAdmin(c *gin.Context, name RepoNameParam) (*auth.Principal, bool) {
	p, ok := requirePrincipal(c)
	if !ok {
		return nil, false
	}
	if p.IsAdmin() {
		return p, true
	}
	allowed, err := h.repos.CanAccess(name, p.UserID, "admin")
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
			return nil, false
		}
		writeDomainErr(c, err)
		return nil, false
	}
	if !allowed {
		auth.WriteError(c, http.StatusForbidden, "forbidden", "无权管理该仓库 ACL")
		return nil, false
	}
	return p, true
}

// aclEntries 把行模型批量转为契约 AclEntry。
func aclEntries(rows []repository.Acl) []AclEntry {
	items := make([]AclEntry, 0, len(rows))
	for _, a := range rows {
		items = append(items, toAPIAcl(a))
	}
	return items
}
