package api

// 用户组管理端点（FR-36）的 HTTP 层用例：守卫、状态码、审计动作名，
// 以及「删组 → 成员与 ACL 一并清理」这条最容易被漏掉的级联语义。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newUserGroupTestHandlers 装配用户组端点所需依赖：组服务 + 用户服务 + 仓库服务
// （仓库服务用于验证 ACL 清理，删组必须连带清掉以该组为主体的条目）。
func newUserGroupTestHandlers(t *testing.T) (*Handlers, *repository.AuditLogRepo) {
	t.Helper()
	db := openAPITestDB(t)
	userRepo := repository.NewUserRepo(db)
	aclRepo := repository.NewAclRepo(db)
	groupRepo := repository.NewUserGroupRepo(db)
	auditLogs := repository.NewAuditLogRepo(db)
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db), aclRepo, repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)), userRepo,
	)
	repoSvc.SetUserGroupRepo(groupRepo)
	return NewHandlers(Deps{
		Users:      domain.NewUserService(userRepo),
		Repos:      repoSvc,
		UserGroups: domain.NewUserGroupService(groupRepo, aclRepo, userRepo),
		AuditLogs:  auditLogs,
	}), auditLogs
}

// serveUserGroup 经生成的 ServerInterfaceWrapper 发起请求（测试口径即线上口径）。
func serveUserGroup(h *Handlers, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: h,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/user-groups", siw.ListUserGroups)
	router.POST("/api/v1/user-groups", siw.CreateUserGroup)
	router.GET("/api/v1/user-groups/:id", siw.GetUserGroup)
	router.PATCH("/api/v1/user-groups/:id", siw.UpdateUserGroup)
	router.DELETE("/api/v1/user-groups/:id", siw.DeleteUserGroup)
	router.GET("/api/v1/user-groups/:id/members", siw.ListUserGroupMembers)
	router.POST("/api/v1/user-groups/:id/members", siw.AddUserGroupMember)
	router.DELETE("/api/v1/user-groups/:id/members/:userId", siw.RemoveUserGroupMember)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// createdUserGroup 解析建组响应（取 id 供后续请求路径使用）。
type createdUserGroup struct {
	Id          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
}

// createGroup 建组并返回组 ID；失败直接终止用例。
func createGroup(t *testing.T, h *Handlers, name, description string) int64 {
	t.Helper()
	rec := serveUserGroup(h, adminPrincipal(), http.MethodPost, "/api/v1/user-groups",
		`{"name":"`+name+`","description":"`+description+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建组 %q 应 201，得 %d：%s", name, rec.Code, rec.Body.String())
	}
	var g createdUserGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("解析建组响应：%v（体：%s）", err, rec.Body.String())
	}
	if g.Id == 0 || g.Name != name {
		t.Fatalf("建组响应异常：%+v", g)
	}
	// createdAt 是契约 required 字段：前端按它排序展示，缺了就是契约漂移。
	if g.CreatedAt == "" {
		t.Fatalf("建组响应缺 createdAt：%+v", g)
	}
	return g.Id
}

// TestCreateUserGroupReturns201AndAudit 建组返回 201 + 完整组对象，并落一条 group.create 审计。
func TestCreateUserGroupReturns201AndAudit(t *testing.T) {
	h, auditLogs := newUserGroupTestHandlers(t)

	id := createGroup(t, h, "release-team", "发布窗口负责人")

	// 审计：动作名与实体类型必须落在既定常量上，否则审计看板认不出这是组操作。
	entries, err := auditLogs.List(repository.AuditFilter{Action: AuditActionGroupCreate, Limit: 10})
	if err != nil {
		t.Fatalf("读审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应落 1 条 group.create 审计，得 %d", len(entries))
	}
	if entries[0].EntityType != EntityTypeUserGroup {
		t.Fatalf("审计实体类型 = %q，期望 %q", entries[0].EntityType, EntityTypeUserGroup)
	}
	// entityKey 落在组名上（与复制变更日志同键，便于跨节点对齐）。
	if entries[0].EntityKey != "release-team" {
		t.Fatalf("审计 entityKey = %q，期望组名", entries[0].EntityKey)
	}
	if entries[0].Result != "ok" {
		t.Fatalf("审计 result = %q，期望 ok", entries[0].Result)
	}

	// 回读：GET 同一组应给出同一份数据。
	rec := serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读组应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var got createdUserGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析读组响应：%v", err)
	}
	if got.Id != id || got.Name != "release-team" || got.Description != "发布窗口负责人" {
		t.Fatalf("读组结果异常：%+v", got)
	}
}

// TestCreateUserGroupDuplicateReturns409 重名建组返回 409（组名全局唯一）。
func TestCreateUserGroupDuplicateReturns409(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)
	createGroup(t, h, "release-team", "")

	rec := serveUserGroup(h, adminPrincipal(), http.MethodPost, "/api/v1/user-groups",
		`{"name":"release-team","description":"换个说明再建一次"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重名建组应 409，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestCreateUserGroupBlankNameReturns400 组名为空 / 空白返回 400。
func TestCreateUserGroupBlankNameReturns400(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)

	for _, body := range []string{`{"name":""}`, `{"name":"   "}`} {
		rec := serveUserGroup(h, adminPrincipal(), http.MethodPost, "/api/v1/user-groups", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("空组名建组 %s 应 400，得 %d：%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestUserGroupEndpointsRequireAdmin 用户组端点一律仅管理员：匿名 401、普通用户 403。
func TestUserGroupEndpointsRequireAdmin(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)

	// 先以管理员建一个组，供「越权读写已存在的组」用例使用。
	id := createGroup(t, h, "release-team", "")
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"列组", http.MethodGet, "/api/v1/user-groups", ""},
		{"建组", http.MethodPost, "/api/v1/user-groups", `{"name":"another"}`},
		{"读组", http.MethodGet, "/api/v1/user-groups/1", ""},
		{"改组", http.MethodPatch, "/api/v1/user-groups/1", `{"name":"renamed"}`},
		{"删组", http.MethodDelete, "/api/v1/user-groups/1", ""},
		{"列成员", http.MethodGet, "/api/v1/user-groups/1/members", ""},
		{"加成员", http.MethodPost, "/api/v1/user-groups/1/members", `{"userId":1}`},
		{"删成员", http.MethodDelete, "/api/v1/user-groups/1/members/1", ""},
	}
	for _, c := range cases {
		if rec := serveUserGroup(h, nil, c.method, c.path, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("匿名%s 应 401，得 %d：%s", c.name, rec.Code, rec.Body.String())
		}
		if rec := serveUserGroup(h, userPrincipal(), c.method, c.path, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("非管理员%s 应 403，得 %d：%s", c.name, rec.Code, rec.Body.String())
		}
	}
	// 越权期间不得产生副作用：组仍存在且未改名。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/1", ""); rec.Code != http.StatusOK {
		t.Fatalf("越权后组应仍在，得 %d", rec.Code)
	}
	var got createdUserGroup
	if err := json.Unmarshal(serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/1", "").Body.Bytes(), &got); err != nil {
		t.Fatalf("解析：%v", err)
	}
	if got.Name != "release-team" || got.Id != id {
		t.Fatalf("越权请求不应改名，得 %+v", got)
	}
}

// TestUserGroupMembersAddAndRemove 成员增删：加入 201、可列、移出 204，
// 且移出后成员列表为空；对不存在的组 / 用户返回 404。
func TestUserGroupMembersAddAndRemove(t *testing.T) {
	h, auditLogs := newUserGroupTestHandlers(t)
	groupID := createGroup(t, h, "release-team", "")

	// 建一个将被加入组的用户。
	user, err := h.users.Create("publisher", "hash-publisher", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	userID := user.ID

	// 加入：201 且回显用户名与加入时间（契约 required）。
	rec := serveUserGroup(h, adminPrincipal(), http.MethodPost,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members", `{"userId":`+strconv.FormatInt(userID, 10)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("加入成员应 201，得 %d：%s", rec.Code, rec.Body.String())
	}
	var member struct {
		UserId    int64  `json:"userId"`
		Username  string `json:"username"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &member); err != nil {
		t.Fatalf("解析加入响应：%v", err)
	}
	if member.UserId != userID || member.Username != "publisher" {
		t.Fatalf("加入响应异常：%+v", member)
	}

	// 列出成员：应含该用户。
	rec = serveUserGroup(h, adminPrincipal(), http.MethodGet,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列成员应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			UserId   int64  `json:"userId"`
			Username string `json:"username"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析成员列表：%v", err)
	}
	if len(list.Items) != 1 || list.Items[0].UserId != userID {
		t.Fatalf("成员列表异常：%+v", list.Items)
	}

	// 移出：204 且列表清空。
	rec = serveUserGroup(h, adminPrincipal(), http.MethodDelete,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members/"+strconv.FormatInt(userID, 10), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("移出成员应 204，得 %d：%s", rec.Code, rec.Body.String())
	}
	rec = serveUserGroup(h, adminPrincipal(), http.MethodGet,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析成员列表：%v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("移出后成员列表应为空，得 %+v", list.Items)
	}

	// 重复移出：404（关系已不存在）。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodDelete,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members/"+strconv.FormatInt(userID, 10), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("重复移出应 404，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 加入不存在的用户：404。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodPost,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members", `{"userId":999999}`); rec.Code != http.StatusNotFound {
		t.Fatalf("加入不存在的用户应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 对不存在的组列成员：404（不得退化成空列表）。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodGet,
		"/api/v1/user-groups/999999/members", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("对不存在的组列成员应 404，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 审计：成员增删各落一条，动作名即既定常量。
	for _, action := range []string{AuditActionGroupMemberAdd, AuditActionGroupMemberRmv} {
		entries, err := auditLogs.List(repository.AuditFilter{Action: action, Limit: 10})
		if err != nil {
			t.Fatalf("读审计 %s：%v", action, err)
		}
		if len(entries) != 1 {
			t.Fatalf("应落 1 条 %s 审计，得 %d", action, len(entries))
		}
		if entries[0].EntityType != EntityTypeUserGroup {
			t.Fatalf("%s 审计实体类型 = %q，期望 %q", action, entries[0].EntityType, EntityTypeUserGroup)
		}
	}
}

// TestDeleteUserGroupClearsMembersAndAcl 删组后：组成员关系与「以该组为主体的 ACL 条目」
// 一并消失，原成员立即失去经由该组获得的授权；审计落一条 group.delete。
func TestDeleteUserGroupClearsMembersAndAcl(t *testing.T) {
	h, auditLogs := newUserGroupTestHandlers(t)
	groupID := createGroup(t, h, "doomed-group", "")

	user, err := h.users.Create("member", "hash-member", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	userID := user.ID
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodPost,
		"/api/v1/user-groups/"+strconv.FormatInt(groupID, 10)+"/members", `{"userId":`+strconv.FormatInt(userID, 10)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("加入成员：%d %s", rec.Code, rec.Body.String())
	}
	// 建一个私有仓库，只给该组 admin 授权（没有任何用户自身条目）。
	if _, err := h.repos.Create("group-only", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := h.repos.SetAcl("group-only", []repository.Acl{{
		SubjectType:    repository.SubjectTypeGroup,
		SubjectGroupID: groupID,
		Action:         repository.ActionAdmin,
	}}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}
	subject := repository.Subject{UserID: userID, GroupIDs: []int64{groupID}}
	if ok, err := h.repos.CanAccess("group-only", subject, repository.ActionRead); err != nil || !ok {
		t.Fatalf("前置失败：组成员应可 read（ok=%v err=%v）", ok, err)
	}

	// 删组。
	rec := serveUserGroup(h, adminPrincipal(), http.MethodDelete, "/api/v1/user-groups/"+strconv.FormatInt(groupID, 10), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删组应 204，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 组没了：再 GET 应 404。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/"+strconv.FormatInt(groupID, 10), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删组后读取应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 以该组为主体的 ACL 条目被清理：该用户不再可访问（仅剩用户自身授权，本例没有）。
	if ok, err := h.repos.CanAccess("group-only", repository.Subject{UserID: userID}, repository.ActionRead); err != nil || ok {
		t.Fatalf("删组后不应仍可 read（ok=%v err=%v）", ok, err)
	}
	rows, err := h.repos.GetAcl("group-only")
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("删组后仓库 ACL 应为空，得 %+v", rows)
	}

	// 审计：删组落一条 group.delete，entityKey 为组名（删完就没得取，故必须先取再删）。
	entries, err := auditLogs.List(repository.AuditFilter{Action: AuditActionGroupDelete, Limit: 10})
	if err != nil {
		t.Fatalf("读审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应落 1 条 group.delete 审计，得 %d", len(entries))
	}
	if entries[0].EntityKey != "doomed-group" {
		t.Fatalf("删组审计 entityKey = %q，期望组名", entries[0].EntityKey)
	}
}

// TestUserGroupNotFoundReturns404 对不存在的组做读 / 改 / 删返回 404。
func TestUserGroupNotFoundReturns404(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)

	if rec := serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("读不存在的组应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodPatch, "/api/v1/user-groups/999999", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("改不存在的组应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodDelete, "/api/v1/user-groups/999999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的组应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateUserGroupRenamesAndConflicts 改组名成功返回新组；改成已存在的组名返回 409；
// 仅传描述时组名不变（空串表示不改该字段）。
func TestUpdateUserGroupRenamesAndConflicts(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)
	id := createGroup(t, h, "team-a", "说明 A")
	other := createGroup(t, h, "team-b", "说明 B")

	// 仅改描述：组名不变。
	rec := serveUserGroup(h, adminPrincipal(), http.MethodPatch, "/api/v1/user-groups/"+strconv.FormatInt(id, 10), `{"description":"新说明"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改组应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var got createdUserGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析：%v", err)
	}
	if got.Name != "team-a" || got.Description != "新说明" {
		t.Fatalf("仅改描述后结果异常：%+v", got)
	}

	// 改成已存在的组名：409。
	if rec := serveUserGroup(h, adminPrincipal(), http.MethodPatch, "/api/v1/user-groups/"+strconv.FormatInt(id, 10), `{"name":"team-b"}`); rec.Code != http.StatusConflict {
		t.Fatalf("改成已存在的组名应 409，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 冲突不得留下半改状态：组名仍是 team-a。
	if err := json.Unmarshal(serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups/"+strconv.FormatInt(id, 10), "").Body.Bytes(), &got); err != nil {
		t.Fatalf("解析：%v", err)
	}
	if got.Name != "team-a" {
		t.Fatalf("冲突后组名不应改变，得 %q", got.Name)
	}
	_ = other
}

// TestListUserGroupsPaginates 列表返回 total 为全量组数，分页窗口按 page / page_size 生效。
func TestListUserGroupsPaginates(t *testing.T) {
	h, _ := newUserGroupTestHandlers(t)
	for _, name := range []string{"group-a", "group-b", "group-c"} {
		createGroup(t, h, name, "")
	}

	rec := serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups?page=1&page_size=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列组应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []createdUserGroup `json:"items"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表：%v", err)
	}
	// total 报全量（3），窗口只回 2 条——否则前端分页控件算不出总页数。
	if list.Total != 3 || len(list.Items) != 2 {
		t.Fatalf("分页结果异常：total=%d items=%d", list.Total, len(list.Items))
	}
	// 第二页回剩下一组。
	if err := json.Unmarshal(serveUserGroup(h, adminPrincipal(), http.MethodGet, "/api/v1/user-groups?page=2&page_size=2", "").Body.Bytes(), &list); err != nil {
		t.Fatalf("解析第二页：%v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "group-c" {
		t.Fatalf("第二页异常：%+v", list.Items)
	}
}
