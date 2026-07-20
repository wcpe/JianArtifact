package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// ListUsers 用户列表（分页），仅管理员可见。
func (h *Handlers) ListUsers(c *gin.Context, params ListUsersParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	rows, total, err := h.users.List(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]User, 0, len(rows))
	for i := range rows {
		items = append(items, toAPIUser(&rows[i]))
	}
	c.JSON(http.StatusOK, UserList{Items: items, Total: total})
}

// CreateUser 创建用户，仅管理员。
func (h *Handlers) CreateUser(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req CreateUserRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户名与口令不能为空")
		return
	}
	role := ""
	if req.Role != nil {
		role = string(*req.Role)
	}
	user, err := h.users.Create(req.Username, req.Password, role)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAPIUser(user))
}

// UpdateUser 更新用户角色 / 状态，仅管理员。
func (h *Handlers) UpdateUser(c *gin.Context, id UserIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req UpdateUserRequest
	if !bindJSON(c, &req) {
		return
	}
	role, status := "", ""
	if req.Role != nil {
		role = string(*req.Role)
	}
	if req.Status != nil {
		status = string(*req.Status)
	}
	user, err := h.users.Update(id, role, status)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIUser(user))
}

// DeleteUser 删除用户，仅管理员。
func (h *Handlers) DeleteUser(c *gin.Context, id UserIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if err := h.users.Delete(id); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ChangePassword 修改 / 重置用户口令：管理员可改任意用户，普通用户仅可改自己。
func (h *Handlers) ChangePassword(c *gin.Context, id UserIdParam) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	if !p.IsAdmin() && p.UserID != id {
		auth.WriteError(c, http.StatusForbidden, "forbidden", "只能修改自己的口令")
		return
	}
	var req PasswordChangeRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "口令不能为空")
		return
	}
	if err := h.users.ChangePassword(id, req.Password); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
