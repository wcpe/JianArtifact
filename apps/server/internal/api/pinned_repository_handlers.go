package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 置顶仓库端点的作用域约定（见迁移 0043）：
//   - 登录用户：自己的个人置顶（user_id = 当前用户），跨设备一致；
//   - 匿名 / 未登录：回退**全局置顶**（user_id IS NULL），由管理员维护。
//
// 读取侧统一经 pinnedResponse 组装响应，写入侧统一走覆盖式 Set（整体替换），
// 因此「置顶不再掉」：置顶存在服务端，换设备/清缓存都不丢。
//
// 可见性边界：置顶集合只回显**调用方可读**的仓库（管理员不受限）。否则普通用户可用
// 置顶端点枚举仓库 ID 反查私有仓库名——那条路径绕过了仓库列表的读权限过滤。
// 写入同样先校验可读（不可读视同不存在，返回 404），且**校验先于覆盖式写入**，
// 避免「请求含非法 ID」时先把用户既有置顶清空。

// maxPinnedRepositoryIds 限定单次覆盖式写入的仓库 ID 数量：任一登录用户（含管理员）传入
// 超大数组时，仓储层会在**单个写事务内**逐 ID 查询存在性再逐 ID 插入，把一次请求放大成
// 数万次查询并长时间持有 SQLite 写锁，故在入口按数量上限拒绝（400）。
const maxPinnedRepositoryIds = 200

// GetMyPinnedRepositories 读取当前用户的置顶仓库：登录用户读自己的，匿名回退全局置顶。
func (h *Handlers) GetMyPinnedRepositories(c *gin.Context) {
	p, authed := auth.PrincipalFrom(c)
	subjectID, isAdmin := int64(0), false
	if authed {
		subjectID, isAdmin = p.UserID, p.IsAdmin()
	}
	resp, err := h.pinnedResponse(pinnedScope(c), subjectID, isAdmin)
	if err != nil {
		writePinnedErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// PutMyPinnedRepositories 覆盖式写入当前用户的置顶仓库，仅登录用户（匿名 401）。
// 匿名置顶没有归属主体、会污染他人视图，故必须登录；未登录用户由全局置顶兜底。
func (h *Handlers) PutMyPinnedRepositories(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	var req PutPinnedRepositoriesRequest
	if !bindJSON(c, &req) {
		return
	}
	if h.pinned == nil {
		pinnedUnavailable(c)
		return
	}
	if len(req.RepositoryIds) > maxPinnedRepositoryIds {
		auth.WriteError(c, http.StatusBadRequest, "too_many_repositories", "置顶仓库数量超过上限")
		return
	}
	if err := h.requirePinnable(req.RepositoryIds, p.UserID, p.IsAdmin()); err != nil {
		writePinnedErr(c, err)
		return
	}
	if err := h.pinned.Set(&p.UserID, req.RepositoryIds); err != nil {
		writePinnedErr(c, err)
		return
	}
	resp, err := h.pinnedResponse(&p.UserID, p.UserID, p.IsAdmin())
	if err != nil {
		writePinnedErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetGlobalPinnedRepositories 读取全局置顶仓库，仅管理员。
func (h *Handlers) GetGlobalPinnedRepositories(c *gin.Context) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	// 管理员视图不按可读性过滤：需要能看到并管理「私有仓库」的全局置顶。
	resp, err := h.pinnedResponse(nil, p.UserID, true)
	if err != nil {
		writePinnedErr(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// PutGlobalPinnedRepositories 覆盖式写入全局置顶仓库，仅管理员。
// 全局置顶对所有用户（含匿名）可见，属实例级配置，故写入记一条审计。
func (h *Handlers) PutGlobalPinnedRepositories(c *gin.Context) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	var req PutPinnedRepositoriesRequest
	if !bindJSON(c, &req) {
		return
	}
	if h.pinned == nil {
		pinnedUnavailable(c)
		return
	}
	if len(req.RepositoryIds) > maxPinnedRepositoryIds {
		auth.WriteError(c, http.StatusBadRequest, "too_many_repositories", "置顶仓库数量超过上限")
		return
	}
	if err := h.requirePinnable(req.RepositoryIds, p.UserID, true); err != nil {
		writePinnedErr(c, err)
		return
	}
	if err := h.pinned.Set(nil, req.RepositoryIds); err != nil {
		writePinnedErr(c, err)
		return
	}
	resp, err := h.pinnedResponse(nil, p.UserID, true)
	if err != nil {
		writePinnedErr(c, err)
		return
	}
	h.AuditLog(c, "setting.set", "setting", "pinned-repositories", "", "全局置顶更新：n="+strconv.Itoa(len(resp.RepositoryIds)), "ok")
	c.JSON(http.StatusOK, resp)
}

// pinnedScope 返回请求主体的置顶作用域：登录用户为其 UserID，匿名/未登录为 nil（全局）。
func pinnedScope(c *gin.Context) *int64 {
	p, ok := auth.PrincipalFrom(c)
	if !ok {
		return nil
	}
	id := p.UserID
	return &id
}

// pinnedResponse 组装某作用域的置顶响应：ID 保留持久化顺序，名字按 ID 实时解析
// （仓库重命名后自动跟随），两者一一对应。仅回显调用方可读（isAdmin 不受限）的仓库。
// 置顶仓储未接线（测试/降级）时返回空集合。
func (h *Handlers) pinnedResponse(scope *int64, subjectID int64, isAdmin bool) (PinnedRepositoriesResponse, error) {
	out := PinnedRepositoriesResponse{RepositoryIds: []int64{}, Names: []string{}}
	if h.pinned == nil {
		return out, nil
	}
	ids, err := h.pinned.List(scope)
	if err != nil {
		return out, err
	}
	names, err := h.pinned.NamesByIDs(ids)
	if err != nil {
		return out, err
	}
	for _, id := range ids {
		name, ok := names[id]
		if !ok {
			// 仓库已删除（外键级联下不应出现）：跳过而不是返回空名。
			continue
		}
		if !isAdmin && !h.readable(name, subjectID) {
			continue
		}
		out.RepositoryIds = append(out.RepositoryIds, id)
		out.Names = append(out.Names, name)
	}
	return out, nil
}

// requirePinnable 校验待写入的置顶 ID 是否可被该主体置顶：未知仓库或不可读者一律返回
// repository.ErrNotFound（视同不存在，不泄漏私有仓库的存在性）。
func (h *Handlers) requirePinnable(ids []int64, subjectID int64, isAdmin bool) error {
	if len(ids) == 0 || h.pinned == nil {
		return nil
	}
	names, err := h.pinned.NamesByIDs(ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		name, ok := names[id]
		if !ok {
			return repository.ErrNotFound
		}
		if isAdmin {
			continue
		}
		if !h.readable(name, subjectID) {
			return repository.ErrNotFound
		}
	}
	return nil
}

// readable 判定主体对仓库是否有读权限（repos 未接线时放行，保持测试/降级装配的旧语义）。
func (h *Handlers) readable(name string, subjectID int64) bool {
	if h.repos == nil {
		return true
	}
	allowed, err := h.repos.CanAccess(name, subjectID, "read")
	return err == nil && allowed
}

// globalPinnedNames 返回**匿名可读**的全局置顶仓库名（按置顶顺序），供公开列表携带，
// 使公开页也能展示置顶。管理员把私有仓库设为全局置顶时不对匿名暴露其名。
func (h *Handlers) globalPinnedNames() ([]string, error) {
	names := []string{}
	if h.pinned == nil {
		return names, nil
	}
	ids, err := h.pinned.ListGlobal()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return names, nil
	}
	byID, err := h.pinned.NamesByIDs(ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		name, ok := byID[id]
		if !ok {
			continue
		}
		// 匿名可读：全局开关关闭或仓库非 public 时都读不到（与公开列表同一判定）。
		if !h.readable(name, 0) {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// pinnedUnavailable 置顶仓储未接线时的写响应：明确失败，避免让用户误以为置顶已生效。
func pinnedUnavailable(c *gin.Context) {
	auth.WriteError(c, http.StatusConflict, "conflict", "置顶存储未就绪")
}

// writePinnedErr 映射置顶相关错误：仓库不存在（仓储层 ErrNotFound，含不可读视同不存在）
// → 404，其余走统一映射。仓储层的 ErrNotFound 与 domain.ErrNotFound 是两个包各自的哨兵
// （分层：domain 依赖 repository，反向转译在 API 边界完成）。
func writePinnedErr(c *gin.Context, err error) {
	if errors.Is(err, repository.ErrNotFound) {
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	writeDomainErr(c, err)
}
