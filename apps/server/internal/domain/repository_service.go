package domain

import (
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AnonymousUsername 是内置匿名主体的用户名（FR-66）：迁移脚本创建，
// 不可登录、不可删除 / 改密 / 停用；ACL 中作为普通主体承接匿名授权。
const AnonymousUsername = "anonymous"

// RepositoryService 处理仓库管理、ACL 与授权判定。
type RepositoryService struct {
	repos    *repository.RepoRepo
	acls     *repository.AclRepo
	assets   *repository.AssetRepo
	settings *SettingService
	users    *repository.UserRepo
}

// NewRepositoryService 构造 RepositoryService。settings 与 users 供匿名判定
// （全局开关 + anonymous 主体 ACL，FR-66）使用。
func NewRepositoryService(repos *repository.RepoRepo, acls *repository.AclRepo, assets *repository.AssetRepo, settings *SettingService, users *repository.UserRepo) *RepositoryService {
	return &RepositoryService{repos: repos, acls: acls, assets: assets, settings: settings, users: users}
}

// List 返回分页仓库与总数。
func (s *RepositoryService) List(limit, offset int) ([]repository.Repository, int, error) {
	total, err := s.repos.Count()
	if err != nil {
		return nil, 0, err
	}
	items, err := s.repos.List(limit, offset)
	return items, total, err
}

// RepoStats 是单仓库的制品统计（数量与总大小）。
type RepoStats struct {
	Count     int64
	TotalSize int64
}

// ListWithStats 返回分页仓库及其制品统计（数量、总大小），避免逐仓 N+1 查询。
// sortBy/order 可选：空串表示默认排序。
func (s *RepositoryService) ListWithStats(limit, offset int, sortBy ...string) ([]repository.Repository, map[int64]RepoStats, int, error) {
	total, err := s.repos.Count()
	if err != nil {
		return nil, nil, 0, err
	}
	var items []repository.Repository
	if len(sortBy) >= 2 && sortBy[0] != "" {
		items, err = s.repos.ListSorted(limit, offset, sortBy[0], sortBy[1])
	} else {
		items, err = s.repos.List(limit, offset)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	rawStats, err := s.assets.CountAndSizeByRepos(ids)
	if err != nil {
		return nil, nil, 0, err
	}
	stats := make(map[int64]RepoStats, len(rawStats))
	for id, rs := range rawStats {
		stats[id] = RepoStats{Count: rs.Count, TotalSize: rs.TotalSize}
	}
	return items, stats, total, nil
}

// Stats 返回单个仓库的制品统计（数量、总大小）。
func (s *RepositoryService) Stats(repoID int64) (RepoStats, error) {
	count, totalSize, err := s.assets.CountAndSizeByRepo(repoID)
	if err != nil {
		return RepoStats{}, err
	}
	return RepoStats{Count: count, TotalSize: totalSize}, nil
}

// Get 按名取仓库。
func (s *RepositoryService) Get(name string) (*repository.Repository, error) {
	r, err := s.repos.GetByName(name)
	return r, mapNotFound(err)
}

// ListAssets 返回仓库内制品的分页列表与总数；prefix 非空时按路径前缀过滤。
// 对于 group 仓库，聚合其所有 hosted/proxy 成员的制品。
// 仓库不存在返回 ErrNotFound。
func (s *RepositoryService) ListAssets(name, prefix string, limit, offset int) ([]repository.Asset, int, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, 0, mapNotFound(err)
	}
	if repo.Type == "group" {
		return s.listGroupAssets(repo, prefix, limit, offset)
	}
	total, err := s.assets.CountByRepo(repo.ID, prefix)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.assets.ListByRepo(repo.ID, prefix, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// listGroupAssets 聚合 group 仓库所有成员的制品列表。
func (s *RepositoryService) listGroupAssets(repo *repository.Repository, prefix string, limit, offset int) ([]repository.Asset, int, error) {
	memberIDs := s.collectMemberIDs(repo)
	if len(memberIDs) == 0 {
		return nil, 0, nil
	}
	total, err := s.assets.CountByRepos(memberIDs, prefix)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.assets.ListByRepos(memberIDs, prefix, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Usage 返回仓库及其客户端接入片段（据 format/type 与对外基址组装）。
// 仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Usage(name, baseURL string) (*repository.Repository, []UsageSnippet, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return repo, buildUsage(repo, baseURL), nil
}

// Create 创建仓库（默认可见性 private）。cfg 为结构化配置：
// proxy 必填合法 remoteUrl；group 必填 members（均存在且同 format、禁止自引用）。
// 校验不过返回 ErrValidation；仓库名重复返回 ErrConflict。
func (s *RepositoryService) Create(name, format, typ, visibility string, cfg repository.RepositoryConfig) (*repository.Repository, error) {
	if visibility == "" {
		visibility = "private"
	}
	if err := s.validateConfig(name, format, typ, cfg); err != nil {
		return nil, err
	}
	configJSON, err := repository.EncodeRepositoryConfig(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := s.repos.Create(name, format, typ, visibility, configJSON); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.repos.GetByName(name)
}

// Update 更新仓库可见性与/或结构化配置。visibility 为空表示不改；cfg 非 nil 时
// 按仓库当前 format/type 重新校验并覆盖写 config。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Update(name, visibility string, cfg *repository.RepositoryConfig) (*repository.Repository, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if cfg != nil {
		if err := s.validateConfig(name, repo.Format, repo.Type, *cfg); err != nil {
			return nil, err
		}
		configJSON, err := repository.EncodeRepositoryConfig(*cfg)
		if err != nil {
			return nil, err
		}
		if err := s.repos.UpdateConfig(name, configJSON); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if visibility != "" {
		if err := s.repos.UpdateVisibility(name, visibility); err != nil {
			return nil, mapNotFound(err)
		}
	}
	return s.repos.GetByName(name)
}

// validateConfig 按仓库类型校验结构化配置：
//   - hosted：remoteUrl 与 members 均须为空；
//   - proxy：remoteUrl 必填且为合法 http/https 绝对地址，members 须为空；
//   - group：members 必填（≥1），每个成员须存在、与本仓 format 一致且非自引用，remoteUrl 须为空。
//
// 违规返回 ErrValidation。
func (s *RepositoryService) validateConfig(name, format, typ string, cfg repository.RepositoryConfig) error {
	switch typ {
	case "hosted":
		if cfg.RemoteURL != "" || len(cfg.Members) > 0 {
			return ErrValidation
		}
	case "proxy":
		if len(cfg.Members) > 0 || cfg.RemoteURL == "" {
			return ErrValidation
		}
		u, err := url.Parse(cfg.RemoteURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return ErrValidation
		}
	case "group":
		if cfg.RemoteURL != "" || len(cfg.Members) == 0 {
			return ErrValidation
		}
		for _, m := range cfg.Members {
			if m == "" || m == name {
				return ErrValidation
			}
			member, err := s.repos.GetByName(m)
			if err != nil {
				return ErrValidation
			}
			if member.Format != format {
				return ErrValidation
			}
		}
	default:
		// 未知类型交由持久化层的 CHECK 约束拒绝（返回后映射为内部错误）。
	}
	return nil
}

// Delete 删除仓库。
func (s *RepositoryService) Delete(name string) error {
	return mapNotFound(s.repos.Delete(name))
}

// GetAcl 返回仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) GetAcl(name string) ([]repository.Acl, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return s.acls.ListByRepo(r.ID)
}

// SetAcl 覆盖写入仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) SetAcl(name string, entries []repository.Acl) ([]repository.Acl, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if err := s.acls.Replace(r.ID, entries); err != nil {
		return nil, err
	}
	return s.acls.ListByRepo(r.ID)
}

// CleanupEmptyMavenArtifacts 清理 Maven 仓库中没有 .jar 文件的 GAV 目录。
// 返回删除的资产数量。仓库不存在返回 ErrNotFound，非 Maven 仓库返回 ErrValidation。
func (s *RepositoryService) CleanupEmptyMavenArtifacts(name string) (int, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return 0, mapNotFound(err)
	}
	if repo.Format != "maven" {
		return 0, ErrValidation
	}

	// 拉取全部资产路径
	paths, err := s.assets.ListAllPaths(repo.ID)
	if err != nil {
		return 0, err
	}

	// 按版本目录（倒数第二层以上）分组，判断是否包含 .jar 文件
	type gavInfo struct {
		hasJar bool
		paths  []string
	}
	gavMap := make(map[string]*gavInfo)
	for _, p := range paths {
		segs := strings.Split(p, "/")
		if len(segs) < 4 {
			continue // 不是合法 GAV 结构
		}
		// 版本目录 = 倒数第二层以上的路径
		versionDir := strings.Join(segs[:len(segs)-1], "/")
		info, ok := gavMap[versionDir]
		if !ok {
			info = &gavInfo{}
			gavMap[versionDir] = info
		}
		info.paths = append(info.paths, p)
		if strings.HasSuffix(p, ".jar") {
			info.hasJar = true
		}
	}

	// 删除没有 jar 的版本目录下所有资产
	deleted := 0
	for _, info := range gavMap {
		if info.hasJar {
			continue
		}
		for _, p := range info.paths {
			if err := s.assets.DeleteByPath(repo.ID, p); err != nil {
				continue // 跳过单个删除失败
			}
			deleted++
		}
	}
	return deleted, nil
}

// CanAccess 判定主体对仓库是否可执行动作（read/write/admin）。
// subjectID==0 表示匿名：受全局开关约束，开启时 public read 放行，
// 否则按内置 anonymous 主体的 ACL 判定（FR-66）。
// 已认证主体：public 仓库对 read 放行；其余按 ACL 判定。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) CanAccess(name string, subjectID int64, action string) (bool, error) {
	r, err := s.repos.GetByName(name)
	if errors.Is(err, repository.ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if subjectID == 0 {
		return s.canAccessAnonymous(r, action)
	}
	if action == "read" && r.Visibility == "public" {
		return true, nil
	}
	return s.acls.HasPermission(r.ID, subjectID, action)
}

// canAccessAnonymous 判定匿名请求对仓库的访问：全局开关关闭一律拒绝；
// 开启时 public read 放行，其余按 anonymous 主体的 ACL 判定。
func (s *RepositoryService) canAccessAnonymous(r *repository.Repository, action string) (bool, error) {
	enabled, err := s.settings.AnonymousAccessEnabled()
	if err != nil {
		return false, err
	}
	if !enabled {
		return false, nil
	}
	if action == "read" && r.Visibility == "public" {
		return true, nil
	}
	anonID, err := s.anonymousSubjectID()
	if err != nil || anonID == 0 {
		return false, err
	}
	return s.acls.HasPermission(r.ID, anonID, action)
}

// anonymousSubjectID 解析内置 anonymous 用户 ID；用户缺失返回 0（视为无授权）。
func (s *RepositoryService) anonymousSubjectID() (int64, error) {
	u, err := s.users.GetByUsername(AnonymousUsername)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return u.ID, nil
}

// ---- FR-54: Tree API ----

// DirectoryEntry 目录懒加载返回结构。
type DirectoryEntry struct {
	Dirs  []string
	Files []repository.Asset
}

// ListDirectory 列出仓库指定前缀的当前层级目录与文件（不递归）。
func (s *RepositoryService) ListDirectory(name, prefix string) (*DirectoryEntry, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	var repoIDs []int64
	if repo.Type == "group" {
		repoIDs = s.collectMemberIDs(repo)
	} else {
		repoIDs = []int64{repo.ID}
	}
	if len(repoIDs) == 0 {
		return &DirectoryEntry{}, nil
	}
	dirs, files, err := s.assets.ListDirectoryEntries(repoIDs, prefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return &DirectoryEntry{Dirs: dirs, Files: files}, nil
}

// ---- FR-30: Search API ----

// SearchResult 全局搜索结果条目。
type SearchResult struct {
	RepoName string
	Asset    repository.Asset
}

// SearchAssets 跨仓库搜索制品路径，按可读权限过滤。
// subjectID==0 表示匿名：受全局开关约束，可搜范围 = public ∪ anonymous 主体
// 被授 read 的仓库（FR-66）；isAdmin 则搜全部。
func (s *RepositoryService) SearchAssets(keyword string, subjectID int64, isAdmin bool, limit, offset int) ([]SearchResult, int, error) {
	// 匿名请求：开关关闭直接返回空集（handler 另拦 401，此处兜底）。
	aclSubject := subjectID
	if subjectID == 0 && !isAdmin {
		enabled, err := s.settings.AnonymousAccessEnabled()
		if err != nil {
			return nil, 0, err
		}
		if !enabled {
			return nil, 0, nil
		}
		if aclSubject, err = s.anonymousSubjectID(); err != nil {
			return nil, 0, err
		}
	}
	// 收集可读仓库 ID
	var repoIDs []int64
	if isAdmin {
		// 管理员搜全部，传空 repoIDs
		repoIDs = nil
	} else {
		pubRepos, err := s.repos.ListPublic()
		if err != nil {
			return nil, 0, err
		}
		for _, r := range pubRepos {
			repoIDs = append(repoIDs, r.ID)
		}
		// 已登录主体或匿名映射到的 anonymous 主体：加上 ACL 授权的私有仓库
		if aclSubject > 0 {
			allRepos, _ := s.repos.List(1000, 0)
			pubSet := make(map[int64]bool)
			for _, r := range pubRepos {
				pubSet[r.ID] = true
			}
			for _, r := range allRepos {
				if pubSet[r.ID] {
					continue
				}
				if ok, _ := s.acls.HasPermission(r.ID, aclSubject, "read"); ok {
					repoIDs = append(repoIDs, r.ID)
				}
			}
		}
		if len(repoIDs) == 0 {
			return nil, 0, nil
		}
	}
	assets, total, err := s.assets.SearchByPath(keyword, repoIDs, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	// 构建 repoID -> name 映射
	idNameMap := make(map[int64]string)
	results := make([]SearchResult, 0, len(assets))
	for i := range assets {
		rName, ok := idNameMap[assets[i].RepositoryID]
		if !ok {
			r, _ := s.repos.GetByID(assets[i].RepositoryID)
			if r != nil {
				rName = r.Name
			}
			idNameMap[assets[i].RepositoryID] = rName
		}
		results = append(results, SearchResult{RepoName: rName, Asset: assets[i]})
	}
	return results, total, nil
}

// collectMemberIDs 收集 group 仓库的所有成员 ID（一层展开）。
func (s *RepositoryService) collectMemberIDs(repo *repository.Repository) []int64 {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil
	}
	var memberIDs []int64
	for _, mname := range cfg.Members {
		m, err := s.repos.GetByName(mname)
		if err != nil {
			continue
		}
		if m.Type == "group" {
			nestedCfg, err := m.DecodeConfig()
			if err == nil {
				for _, nn := range nestedCfg.Members {
					nm, err := s.repos.GetByName(nn)
					if err == nil && nm.Type != "group" {
						memberIDs = append(memberIDs, nm.ID)
					}
				}
			}
			continue
		}
		memberIDs = append(memberIDs, m.ID)
	}
	return memberIDs
}
