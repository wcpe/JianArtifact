package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 用户组管理端点（FR-36）：组的增删改查与成员维护。
//
// 全部端点仅管理员可用——管理面不做仓库级细粒度授权（规格 §2 明确把
// `acl_manage` 接到管理端点列为范围外），沿用既有 requireAdmin 守卫。
//
// 组本身不产生权限：真正生效的是把组作为主体写进仓库 ACL。因此本组端点只维护
// 「组 + 成员」这一授权载体本身，判定逻辑仍在 RepositoryService 单点。

// ListUserGroups 用户组列表（分页），仅管理员。
func (h *Handlers) ListUserGroups(c *gin.Context, params ListUserGroupsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	rows, err := h.userGroups.List()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 组规模按「管理面一次列全」设计（企业场景典型为个位数 ~ 数十个），
	// 分页窗口在此切片即可，total 仍报全量，与既有 UserList 的分页口径一致。
	total := len(rows)
	limit, offset := pageOffset(params.Page, params.PageSize)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	items := make([]UserGroup, 0, end-offset)
	for i := offset; i < end; i++ {
		items = append(items, toAPIUserGroup(&rows[i]))
	}
	c.JSON(http.StatusOK, UserGroupList{Items: items, Total: total})
}

// CreateUserGroup 创建用户组，仅管理员；组名为空返回 400，重名返回 409。
func (h *Handlers) CreateUserGroup(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req CreateUserGroupRequest
	if !bindJSON(c, &req) {
		return
	}
	desc := ""
	if req.Description != nil {
		desc = *req.Description
	}
	g, err := h.userGroups.Create(req.Name, desc)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, AuditActionGroupCreate, EntityTypeUserGroup, g.Name, "", "id="+strconv.FormatInt(g.ID, 10), "ok")
	c.JSON(http.StatusCreated, toAPIUserGroup(g))
}

// GetUserGroup 读取单个用户组，仅管理员；不存在返回 404。
func (h *Handlers) GetUserGroup(c *gin.Context, id UserGroupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	g, err := h.userGroups.Get(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIUserGroup(g))
}

// UpdateUserGroup 更新组名 / 说明，仅管理员；重名返回 409。
// 字段缺省（null）表示不改（与组服务 COALESCE(NULLIF(?, ”), 列) 的口径一致）。
func (h *Handlers) UpdateUserGroup(c *gin.Context, id UserGroupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req UpdateUserGroupRequest
	if !bindJSON(c, &req) {
		return
	}
	name, desc := "", ""
	if req.Name != nil {
		name = *req.Name
	}
	if req.Description != nil {
		desc = *req.Description
	}
	g, err := h.userGroups.Update(id, name, desc)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, AuditActionGroupUpdate, EntityTypeUserGroup, g.Name, "", "id="+strconv.FormatInt(g.ID, 10), "ok")
	c.JSON(http.StatusOK, toAPIUserGroup(g))
}

// DeleteUserGroup 删除用户组，仅管理员。
// 连带清理成员关系与「以该组为主体的 ACL 条目」——不清会留下指向已消失组的悬空授权，
// 它在列表接口里既显示不出组名、也无法回收，是管理员无法解释的僵尸授权。
func (h *Handlers) DeleteUserGroup(c *gin.Context, id UserGroupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	// 先取组名再删：删完就没得取了，而审计的 entityKey 要落在组名上（与复制变更日志同键）。
	g, err := h.userGroups.Get(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if err := h.userGroups.Delete(id); err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, AuditActionGroupDelete, EntityTypeUserGroup, g.Name, "", "id="+strconv.FormatInt(g.ID, 10), "ok")
	c.Status(http.StatusNoContent)
}

// ListUserGroupMembers 列出组成员，仅管理员；组不存在返回 404。
func (h *Handlers) ListUserGroupMembers(c *gin.Context, id UserGroupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	// 组不存在必须报 404：否则「查不存在的组的成员」会给出一个空列表，
	// 调用方无法区分「组不存在」与「组存在但没有成员」。
	rows, err := h.userGroups.ListMemberRows(id)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]UserGroupMember, 0, len(rows))
	for i := range rows {
		items = append(items, UserGroupMember{UserId: rows[i].UserID, Username: rows[i].Username, CreatedAt: rows[i].CreatedAt})
	}
	c.JSON(http.StatusOK, UserGroupMemberList{Items: items})
}

// AddUserGroupMember 把用户加入组，仅管理员；组或用户不存在返回 404。
// 重复加入是幂等的（不报错），因此该端点可安全重试。
func (h *Handlers) AddUserGroupMember(c *gin.Context, id UserGroupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req AddUserGroupMemberRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.userGroups.AddMember(id, req.UserId); err != nil {
		writeDomainErr(c, err)
		return
	}
	// 用户名与加入时间仅用于响应回显：取不到就留空，不因此让「已加入」这个既成事实失败。
	username, joinedAt := "", ""
	if u, err := h.users.Get(req.UserId); err == nil {
		username = u.Username
	}
	if at, err := h.userGroups.MemberJoinedAt(id, req.UserId); err == nil {
		joinedAt = at
	}
	h.AuditLog(c, AuditActionGroupMemberAdd, EntityTypeUserGroup, strconv.FormatInt(id, 10), "",
		"userId="+strconv.FormatInt(req.UserId, 10)+" username="+username, "ok")
	c.JSON(http.StatusCreated, UserGroupMember{UserId: req.UserId, Username: username, CreatedAt: joinedAt})
}

// RemoveUserGroupMember 把用户移出组，仅管理员；关系不存在返回 404。
// 移出后该用户立即失去**经由该组**获得的授权，其自身单独被授予的授权不受影响。
func (h *Handlers) RemoveUserGroupMember(c *gin.Context, id UserGroupIdParam, userId UserGroupMemberUserIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if err := h.userGroups.RemoveMember(id, userId); err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, AuditActionGroupMemberRmv, EntityTypeUserGroup, strconv.FormatInt(id, 10), "",
		"userId="+strconv.FormatInt(userId, 10), "ok")
	c.Status(http.StatusNoContent)
}

// toAPIUserGroup 把行模型转为契约 UserGroup。
func toAPIUserGroup(g *repository.UserGroup) UserGroup {
	return UserGroup{Id: g.ID, Name: g.Name, Description: g.Description, CreatedAt: g.CreatedAt}
}
