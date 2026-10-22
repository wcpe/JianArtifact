package domain

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// AnonymousUsername 是内置匿名主体的用户名：迁移脚本创建，
// 不可登录、不可删除 / 改密 / 停用；ACL 中作为普通主体承接匿名授权。
const AnonymousUsername = "anonymous"

// searchTimeoutDefault 是检索语句预算的兜底默认值。
// 生产装配经 SetSearchTimeout 注入 JIAN_SEARCH_TIMEOUT_SECONDS 的解析结果，
// 本常量只在未注入时兜底（测试构造与未接线的旧路径），必须与 config 的
// defaultSearchTimeout 保持同值——改一处必须同步改另一处。
const searchTimeoutDefault = 10 * time.Second

// RepositoryService 处理仓库管理、ACL 与授权判定。
type RepositoryService struct {
	repos     *repository.RepoRepo
	acls      *repository.AclRepo
	assets    *repository.AssetRepo
	metadata  *repository.FormatMetadataRepo
	settings  *SettingService
	users     *repository.UserRepo
	groups    *repository.UserGroupRepo // 展开主体的组归属（nil 退化为「仅用户自身」）
	mutator   *AssetMutationCoordinator
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
	enabled   formats.Set
	// searchTimeout 是一次搜索里**下发到 SQL 的检索语句**的预算上限（网页搜索 / npm 搜索 /
	// suggest / npm 的按成员检索）。每个搜索入口用它派生一次 deadline，同一次检索的多条语句
	// 共享同一个 deadline。它**不覆盖**可搜范围解析那一段查询（见 searchContext 的说明）。
	searchTimeout time.Duration
}

// NewRepositoryService 构造 RepositoryService。settings 与 users 供匿名判定
// （全局开关 + anonymous 主体 ACL）使用。
func NewRepositoryService(repos *repository.RepoRepo, acls *repository.AclRepo, assets *repository.AssetRepo, settings *SettingService, users *repository.UserRepo) *RepositoryService {
	return &RepositoryService{repos: repos, acls: acls, assets: assets, settings: settings, users: users, enabled: formats.Default(),
		searchTimeout: searchTimeoutDefault}
}

// SetSearchTimeout 注入检索语句的预算上限（覆盖范围见 searchContext）。
// 非正数不覆盖默认值：搜索是无界查询，关掉超时等于开放一个可以无限占用唯一数据库连接的
// 入口，因此本项**没有禁用语义**（口径同 autoBlockBase，而非 storageCleanupInterval 的 0 禁用）。
func (s *RepositoryService) SetSearchTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.searchTimeout = d
}

// SearchTimeout 返回当前生效的检索语句预算，供协议层/运维排查读取。
func (s *RepositoryService) SearchTimeout() time.Duration { return s.searchTimeout }

// SetEnabledFormats 注入启动期格式能力集合。集合仅在装配阶段设置，运行中不切换。
func (s *RepositoryService) SetEnabledFormats(enabled formats.Set) { s.enabled = enabled }

// EnabledFormats 返回当前进程启用的格式，供管理面展示。
func (s *RepositoryService) EnabledFormats() []string { return s.enabled.List() }

// SetChangeRecorder 注入复制变更日志记录器；nil 表示不记录。
func (s *RepositoryService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *RepositoryService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// SetUserGroupRepo 注入用户组仓储：授权判定时据此展开主体的组归属。
// 未注入（nil）时 CanAccess 只看用户自身 ACL，等价于接入用户组之前的行为。
func (s *RepositoryService) SetUserGroupRepo(groups *repository.UserGroupRepo) { s.groups = groups }

// SetMutationCoordinator 注入节点级资产生命周期协调器，供仓库删除和 Maven 清理复用。
func (s *RepositoryService) SetMutationCoordinator(c *AssetMutationCoordinator) { s.mutator = c }

// SetFormatMetadataRepo 注入格式索引仓储，供仓库级删除持久化回滚快照。
func (s *RepositoryService) SetFormatMetadataRepo(r *repository.FormatMetadataRepo) { s.metadata = r }

// recordChange 记录复制变更日志；记录失败不阻断业务写（对账兜底，见 ADR-0013）。
func (s *RepositoryService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
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
	// 别名一次 IN 查询批量填充，避免逐仓 N+1。
	aliasMap, err := s.repos.ListAliasesByRepos(ids)
	if err != nil {
		return nil, nil, 0, err
	}
	for i := range items {
		if aliases := aliasMap[items[i].ID]; len(aliases) > 0 {
			items[i].Aliases = aliases
		}
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

// Get 按名取仓库（名字可为别名，返回的是主名仓库并带出别名列表）。
func (s *RepositoryService) Get(name string) (*repository.Repository, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if err := s.fillAliases(r); err != nil {
		return nil, err
	}
	return r, nil
}

// ReposByNames 批量按名字解析仓库（含别名），返回以请求名为键的映射。
//
// 供「按成员聚合」的协议路径（npm group 搜索）一次完成成员解析：逐个 Get 会为每个成员发出
// 两条查询（主名 + 别名回填），成员一多就与搜索本身的开销相乘；这里固定两条。
func (s *RepositoryService) ReposByNames(names []string) (map[string]*repository.Repository, error) {
	return s.repos.ListByNames(names)
}

// fillAliases 读取仓库别名并挂到模型上（供 API 响应展示）。
func (s *RepositoryService) fillAliases(repo *repository.Repository) error {
	aliases, err := s.repos.ListAliases(repo.ID)
	if err != nil {
		return err
	}
	repo.Aliases = aliases
	return nil
}

// validateAliases 清洗并校验别名集合：去两端空白、去重、拒绝空别名与「等于自身主名」，
// 并拒绝与任何仓库主名或其他仓库别名冲突（别名与主名共享同一命名空间、全局唯一）。
// own 为当前仓库已有的别名集合（创建时为空），用于覆盖式更新时放行「自身既有别名」。
// 违规返回 ErrValidation；名称被占用返回 ErrConflict。返回去重后的别名（保持首次出现顺序）。
func (s *RepositoryService) validateAliases(ownName string, aliases []string, own map[string]bool) ([]string, error) {
	cleaned := make([]string, 0, len(aliases))
	seen := make(map[string]bool, len(aliases))
	for _, raw := range aliases {
		alias := strings.TrimSpace(raw)
		if alias == "" {
			return nil, fmt.Errorf("%w: 别名不能为空", ErrValidation)
		}
		if alias == ownName {
			return nil, fmt.Errorf("%w: 别名不能与仓库主名相同", ErrValidation)
		}
		if seen[alias] {
			continue
		}
		seen[alias] = true
		if !own[alias] {
			taken, err := s.repos.NameTaken(alias)
			if err != nil {
				return nil, err
			}
			if taken {
				return nil, fmt.Errorf("%w: 名称 %q 已被占用", ErrConflict, alias)
			}
		}
		cleaned = append(cleaned, alias)
	}
	return cleaned, nil
}

// aliasSet 把别名切片转为集合，供校验时放行「自身既有别名」。
func aliasSet(aliases []string) map[string]bool {
	out := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		out[a] = true
	}
	return out
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

// Usage 返回仓库及其客户端接入片段（据 format/type 与对外基址组装，文案按 lang 本地化）。
// 仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Usage(name, baseURL, lang string) (*repository.Repository, []UsageSnippet, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	if !s.enabled.Has(repo.Format) {
		return nil, nil, fmt.Errorf("%w: %s", ErrFormatDisabled, repo.Format)
	}
	return repo, buildUsage(repo, baseURL, lang), nil
}

// Create 创建仓库（默认可见性 private）。cfg 为结构化配置：
// proxy 必填合法 remoteUrl，可选 credentialRef（环境变量引用名）；
// group 必填 members（均存在且同 format、禁止自引用）。
// description 为仓库描述（可为空）。
// aliases 为可选的仓库别名（与主名共享命名空间、全局唯一）。
// 校验不过返回 ErrValidation；仓库名或别名被占用返回 ErrConflict。
func (s *RepositoryService) Create(name, format, typ, visibility, description string, cfg repository.RepositoryConfig, aliases ...string) (*repository.Repository, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if !slices.Contains(formats.Known, format) {
		return nil, fmt.Errorf("%w: 未知格式 %q", ErrValidation, format)
	}
	if !s.enabled.Has(format) {
		return nil, fmt.Errorf("%w: %s", ErrFormatDisabled, format)
	}
	if err := validateFormatType(format, typ); err != nil {
		return nil, err
	}
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
	// 别名在插入前校验，避免仓库已建而别名非法造成半成品。
	cleanedAliases, err := s.validateAliases(name, aliases, nil)
	if err != nil {
		return nil, err
	}
	repoID, err := s.repos.Create(name, format, typ, visibility, configJSON)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	// 仓库行已建但后续步骤失败时补偿删除：别名与主名的跨表唯一性没有 DB 兜底，只有服务层
	// 「先查后写」，并发窗口内会真的插进重复别名。若留下仓库行，调用方收到 ErrConflict 却有一个
	// 按主名可访问的仓库，重试只会得到「名称已被占用」。
	if err := s.finishCreate(name, description, repoID, cleanedAliases); err != nil {
		_ = s.repos.Delete(name)
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, err
	}
	if err := s.fillAliases(repo); err != nil {
		return nil, err
	}
	s.recordChange(EntityRepository, RepoKey(name), OpPut, RepoChangeData{
		Name: name, Format: repo.Format, Type: repo.Type,
		Visibility: repo.Visibility, Description: repo.Description, Config: repo.Config,
	})
	return repo, nil
}

// finishCreate 完成创建仓库后的收尾步骤（描述、别名）。任一步失败即返回错误，由调用方补偿删除。
func (s *RepositoryService) finishCreate(name, description string, repoID int64, aliases []string) error {
	if description != "" {
		if err := s.repos.UpdateDescription(name, description); err != nil {
			return err
		}
	}
	if len(aliases) > 0 {
		if err := s.repos.SetAliases(repoID, aliases); err != nil {
			return err
		}
	}
	return nil
}

// Update 更新仓库可见性、描述、结构化配置与/或别名。visibility 为空表示不改；
// description 为 nil 表示不改（指向空串表示清空）；cfg 非 nil 时
// 按仓库当前 format/type 重新校验并覆盖写 config；aliases 非 nil 时覆盖式替换别名集合。
// 仓库不存在返回 ErrNotFound；别名非法返回 ErrValidation、被占用返回 ErrConflict。
func (s *RepositoryService) Update(name, visibility string, description *string, cfg *repository.RepositoryConfig, aliases *[]string) (*repository.Repository, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if cfg != nil {
		if err := s.validateConfig(repo.Name, repo.Format, repo.Type, *cfg); err != nil {
			return nil, err
		}
		configJSON, err := repository.EncodeRepositoryConfig(*cfg)
		if err != nil {
			return nil, err
		}
		if err := s.repos.UpdateConfig(repo.Name, configJSON); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if description != nil {
		if err := s.repos.UpdateDescription(repo.Name, *description); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if visibility != "" {
		if err := s.repos.UpdateVisibility(repo.Name, visibility); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if aliases != nil {
		own, err := s.repos.ListAliases(repo.ID)
		if err != nil {
			return nil, err
		}
		cleaned, err := s.validateAliases(repo.Name, *aliases, aliasSet(own))
		if err != nil {
			return nil, err
		}
		if err := s.repos.SetAliases(repo.ID, cleaned); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrConflict
			}
			return nil, mapNotFound(err)
		}
	}
	updated, err := s.repos.GetByName(repo.Name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if err := s.fillAliases(updated); err != nil {
		return nil, err
	}
	s.recordChange(EntityRepository, RepoKey(repo.Name), OpPut, RepoChangeData{
		Name: repo.Name, Format: updated.Format, Type: updated.Type,
		Visibility: updated.Visibility, Description: updated.Description, Config: updated.Config,
	})
	return updated, nil
}

// Rename 重命名仓库（name -> newName）。newName 非空且未被任何主名/别名占用，
// 否则返回 ErrValidation / ErrConflict。重命名后自动把旧名登记为别名，
// 使旧名仍能经 GetByName 解析到同一仓库（别名与主名共享命名空间）。
//
// 变更记录说明：仓储层 Rename 在同一事务内更新主名并把旧名写入别名表，故旧名与主名
// 都指向同一仓库，无需为旧名单独发墓碑。复制通道以主名编址（RepoKey(name)），
// 重命名会使自然键从 repo:<旧名> 变为 repo:<新名>：此处按**新名**记录一次 put 变更
// （新键=新名）；旧名作为别名继续可解析，历史审计/计量保留旧名不回填（见 D-2a）。
func (s *RepositoryService) Rename(name, newName string) (*repository.Repository, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, fmt.Errorf("%w: 新名称不能为空", ErrValidation)
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if newName == repo.Name {
		return nil, fmt.Errorf("%w: 新名称与原名称相同", ErrValidation)
	}
	taken, err := s.repos.NameTaken(newName)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, fmt.Errorf("%w: 名称 %q 已被占用", ErrConflict, newName)
	}
	if err := s.repos.Rename(repo.Name, newName); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, mapNotFound(err)
	}
	renamed, err := s.repos.GetByName(newName)
	if err != nil {
		return nil, err
	}
	if err := s.fillAliases(renamed); err != nil {
		return nil, err
	}
	s.recordChange(EntityRepository, RepoKey(renamed.Name), OpPut, RepoChangeData{
		Name: renamed.Name, Format: renamed.Format, Type: renamed.Type,
		Visibility: renamed.Visibility, Description: renamed.Description, Config: renamed.Config,
	})
	return renamed, nil
}

// SetOnline 设置仓库 online/offline 状态。
// 直接落库、不写复制变更日志：online 是节点本地运维状态（M-2 硬约束），
// 不得经复制传播到对端覆盖其本地状态。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) SetOnline(name string, online bool) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	// 名字可为别名：解析到主名仓库后按主名落库（别名与主名等价，D-2b）。
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return mapNotFound(err)
	}
	if err := s.repos.SetOnline(repo.Name, online); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// validateConfig 校验仓库配置，按仓库类型校验结构化配置：
//   - hosted：remoteUrl、credentialRef 与 members 均须为空；
//   - proxy：remoteUrl 必填且为合法 http/https 绝对地址，members 与 immutableRelease 须为空或 false，credentialRef 可选且必须是环境变量引用名；
//   - group：members 必填（≥1），每个成员须存在、与本仓 format 一致且非自引用，remoteUrl、credentialRef 与 immutableRelease 须为空或 false。
//
// cacheRetentionDays（代理缓存保留）只对 proxy 有意义，hosted/group 必须为 0，
// proxy 上 0 = 关闭（默认）且不接受负数；存储配额 quotaBytes/quotaAssets 为仓库级上限，
// 0/缺省 = 不限、负数非法，**只有 hosted 接受非 0 配额**：group 不承载写入，
// proxy 的缓存在读取回源路径上落盘而该路径没有预检与早拒，两者都在这里拒绝非 0 值。
//
// 违规返回 ErrValidation。
func (s *RepositoryService) validateConfig(name, format, typ string, cfg repository.RepositoryConfig) error {
	if cfg.QuotaBytes < 0 || cfg.QuotaAssets < 0 {
		return fmt.Errorf("%w: 存储配额不能为负数", ErrValidation)
	}
	switch typ {
	case "hosted":
		if cfg.RemoteURL != "" || cfg.CredentialRef != "" || len(cfg.Members) > 0 || cfg.CacheRetentionDays != 0 {
			return ErrValidation
		}
	case "proxy":
		if len(cfg.Members) > 0 || cfg.RemoteURL == "" || cfg.ImmutableRelease || cfg.CacheRetentionDays < 0 {
			return ErrValidation
		}
		if cfg.QuotaBytes != 0 || cfg.QuotaAssets != 0 {
			// 代理缓存的写入发生在「读取回源」路径上：该路径既没有准入预检、也没有流式早拒，
			// 只能在整份上游内容落盘后的提交点才被判越限，配额强制实际只对 hosted 完整生效。
			// 故与 group 一样先拒绝非 0 配额，避免"设了但永不生效"；
			// 待后续增量把代理回源写入纳入配额强制后再放开。
			return fmt.Errorf("%w: proxy 仓库暂不支持存储配额", ErrValidation)
		}
		u, err := url.Parse(cfg.RemoteURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ErrValidation
		}
		if cfg.CredentialRef != "" && !validCredentialRef(cfg.CredentialRef) {
			return ErrValidation
		}
	case "group":
		if cfg.RemoteURL != "" || cfg.CredentialRef != "" || cfg.ImmutableRelease || len(cfg.Members) == 0 || cfg.CacheRetentionDays != 0 {
			return ErrValidation
		}
		if cfg.QuotaBytes != 0 || cfg.QuotaAssets != 0 {
			// group 只是成员的读取聚合视图，不承载写入：不接受非 0 配额（避免"设了但永不生效"）。
			return fmt.Errorf("%w: group 仓库不支持存储配额", ErrValidation)
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

func validateFormatType(format, typ string) error {
	if format == "gomod" && typ != "proxy" {
		return ErrValidation
	}
	return nil
}

// MigrationRepositorySpec 是迁移启动前的目标仓库拓扑描述。
// 它只包含已经过来源层白名单筛选的非敏感配置。
type MigrationRepositorySpec struct {
	Name   string
	Format string
	Type   string
	Config repository.RepositoryConfig
}

// ValidateMigrationPlan 在迁移任务进入 running 前校验格式能力和目标拓扑，避免异步任务
// 先写入一部分仓库后才发现格式禁用、代理缺上游或 group 成员无效。
func (s *RepositoryService) ValidateMigrationPlan(specs []MigrationRepositorySpec) error {
	byName := make(map[string]MigrationRepositorySpec, len(specs))
	for _, spec := range specs {
		format := strings.ToLower(strings.TrimSpace(spec.Format))
		if !slices.Contains(formats.Known, format) {
			return fmt.Errorf("%w: 未知格式 %q", ErrValidation, format)
		}
		if !s.enabled.Has(format) {
			return fmt.Errorf("%w: %s", ErrFormatDisabled, format)
		}
		if spec.Name == "" || spec.Type == "" {
			return ErrValidation
		}
		switch spec.Type {
		case "hosted", "proxy", "group":
		default:
			return ErrValidation
		}
		if err := validateFormatType(format, spec.Type); err != nil {
			return err
		}
		spec.Format = format
		if _, exists := byName[spec.Name]; exists {
			return ErrValidation
		}
		byName[spec.Name] = spec
	}
	for _, spec := range specs {
		if spec.Type != "group" {
			if err := s.validateConfig(spec.Name, strings.ToLower(strings.TrimSpace(spec.Format)), spec.Type, spec.Config); err != nil {
				return err
			}
			continue
		}
		if spec.Config.RemoteURL != "" || spec.Config.CredentialRef != "" || len(spec.Config.Members) == 0 {
			return ErrValidation
		}
		for _, memberName := range spec.Config.Members {
			if memberName == "" || memberName == spec.Name {
				return ErrValidation
			}
			member, ok := byName[memberName]
			if !ok {
				var err error
				memberRepo, err := s.repos.GetByName(memberName)
				if err != nil {
					return ErrValidation
				}
				member = MigrationRepositorySpec{Format: memberRepo.Format}
			}
			if member.Format != strings.ToLower(strings.TrimSpace(spec.Format)) {
				return ErrValidation
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return ErrValidation
		}
		item, ok := byName[name]
		if !ok || item.Type != "group" {
			return nil
		}
		visiting[name] = true
		for _, member := range item.Config.Members {
			if err := visit(member); err != nil {
				return err
			}
		}
		delete(visiting, name)
		visited[name] = true
		return nil
	}
	for name, spec := range byName {
		if spec.Type == "group" {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	for _, spec := range specs {
		existing, err := s.repos.GetByName(spec.Name)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if existing.Format != strings.ToLower(strings.TrimSpace(spec.Format)) || existing.Type != spec.Type {
			return ErrConflict
		}
		current, err := existing.DecodeConfig()
		if err != nil {
			return err
		}
		if current.RemoteURL != spec.Config.RemoteURL || current.CredentialRef != spec.Config.CredentialRef || !slices.Equal(current.Members, spec.Config.Members) {
			return ErrConflict
		}
	}
	return nil
}

// validCredentialRef 仅接受专用凭据命名空间的逻辑名称。
// 此处绝不读取环境变量，避免凭据值进入领域模型、数据库或管理面响应。
func validCredentialRef(ref string) bool {
	return upstream.IsCredentialRef(ref)
}

// Delete 删除仓库。
func (s *RepositoryService) Delete(name string) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return ErrConflict
	}
	assets, err := s.assets.ListByRepo(repo.ID, "", maxAssetOperationItems+1, 0)
	if err != nil {
		return err
	}
	if len(assets) > maxAssetOperationItems {
		return ErrOperationLimit
	}
	if len(assets) > 0 {
		if s.mutator == nil {
			return fmt.Errorf("%w: 资产事务引擎未就绪", ErrConflict)
		}
		acls, err := s.acls.ListByRepo(repo.ID)
		if err != nil {
			return err
		}
		var metadata []repository.FormatMetadata
		if s.metadata != nil {
			metadata, err = s.metadata.ListAll(repo.ID, repo.Format)
			if err != nil {
				return err
			}
		}
		operationID := NewOperationID()
		snapshot := repository.RepositoryDeleteJournal{Repository: *repo, ACL: acls, FormatMetadata: metadata}
		_, err = s.mutator.ApplyWithOperationIDAndHook(operationID, deleteMutationItems(assets), func(tx *sqlx.Tx) error {
			if err := repository.PutRepositoryDeleteJournal(tx, operationID, snapshot); err != nil {
				return err
			}
			return s.repos.DeleteTx(tx, repo.Name)
		})
		if err != nil {
			return err
		}
	} else if err := s.repos.Delete(repo.Name); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityRepository, RepoKey(repo.Name), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// GetAcl 返回仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) GetAcl(name string) ([]repository.Acl, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return s.acls.ListByRepo(r.ID)
}

// aclActions 是 ACL 可登记的全部动作（六档），与 acl.action 的 CHECK 一致。
// 之所以要在领域层再校一遍而不是只依赖 SQLite CHECK：CHECK 违规表现为驱动层的
// 裸 SQL 错误，且是在**整仓 ACL 已被 DELETE 之后**才在 INSERT 上炸出来，
// 事务回滚后用户看到的是「内部错误」而不是「动作非法」。
var aclActions = []string{
	repository.ActionRead, repository.ActionWrite, repository.ActionPublish,
	repository.ActionDelete, repository.ActionAclManage, repository.ActionAdmin,
}

// validateActions 校验待写入的动作是否全部合法（六档）；非法返回 ErrValidation。
func validateActions(entries []repository.Acl) error {
	for _, e := range entries {
		if !slices.Contains(aclActions, e.Action) {
			return fmt.Errorf("%w：未知 ACL 动作 %q", ErrValidation, e.Action)
		}
	}
	return nil
}

// SetAcl 覆盖写入仓库 ACL；仓库不存在返回 ErrNotFound。
// 用户主体条目 SubjectID 有值，用户组主体条目 SubjectGroupID 有值，二者由
// 表内 CHECK 强制互斥；非法动作返回 ErrValidation。
func (s *RepositoryService) SetAcl(name string, entries []repository.Acl) ([]repository.Acl, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if err := validateActions(entries); err != nil {
		return nil, err
	}
	if err := s.acls.Replace(r.ID, entries); err != nil {
		return nil, err
	}
	s.recordAclChange(name, entries)
	return s.acls.ListByRepo(r.ID)
}

// recordAclChange 记录仓库 ACL 整仓快照变更（以 username 编址，跨节点一致）。
func (s *RepositoryService) recordAclChange(repoName string, entries []repository.Acl) {
	if s.recorder == nil {
		return
	}
	data := AclChangeData{RepoName: repoName, Entries: make([]AclEntryData, 0, len(entries))}
	for _, e := range entries {
		u, err := s.users.GetByID(e.SubjectID)
		if err != nil {
			continue // 主体不存在（如已删除用户），跳过该条
		}
		data.Entries = append(data.Entries, AclEntryData{Username: u.Username, Action: e.Action})
	}
	if err := s.recorder.Record(EntityAcl, AclKey(repoName), OpPut, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", EntityAcl, AclKey(repoName), OpPut, err)
	}
}

// CleanupEmptyMavenArtifacts 清理 Maven 仓库中没有 .jar 文件的 GAV 目录。
// 返回删除的资产数量。仓库不存在返回 ErrNotFound，非 Maven 仓库返回 ErrValidation。
func (s *RepositoryService) CleanupEmptyMavenArtifacts(name string) (int, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return 0, err
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return 0, mapNotFound(err)
	}
	if repo.Format != "maven" {
		return 0, ErrValidation
	}
	if !s.enabled.Has(repo.Format) {
		return 0, fmt.Errorf("%w: %s", ErrFormatDisabled, repo.Format)
	}

	assets, err := s.assets.ListByRepo(repo.ID, "", maxAssetOperationItems+1, 0)
	if err != nil {
		return 0, err
	}
	if len(assets) > maxAssetOperationItems {
		return 0, ErrOperationLimit
	}

	// 按版本目录（倒数第二层以上）分组，判断是否包含 .jar 文件
	type gavInfo struct {
		hasJar bool
		assets []repository.Asset
	}
	gavMap := make(map[string]*gavInfo)
	for _, asset := range assets {
		segs := strings.Split(asset.Path, "/")
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
		info.assets = append(info.assets, asset)
		if strings.HasSuffix(asset.Path, ".jar") {
			info.hasJar = true
		}
	}

	var toDelete []repository.Asset
	for _, info := range gavMap {
		if info.hasJar {
			continue
		}
		toDelete = append(toDelete, info.assets...)
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	if s.mutator == nil {
		return 0, fmt.Errorf("%w: 资产事务引擎未就绪", ErrConflict)
	}
	if err := s.mutator.Apply(deleteMutationItems(toDelete)); err != nil {
		return 0, err
	}
	return len(toDelete), nil
}

// CanAccess 判定主体对仓库是否可执行动作。
//
// subject 是**已展开组归属**的主体（见 SubjectFor）。UserID==0 即匿名（可用
// repository.AnonymousSubject() 构造）：受全局开关约束，开启时 public read 放行，
// 否则按内置 anonymous 主体的 ACL 判定。匿名不属于任何组，组集合恒为空。
// 已认证主体：public 仓库对 read 放行；其余按 ACL 判定，命中「用户自身」或
// 「其所属任一用户组」的 ACL 之一即放行。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) CanAccess(name string, subject repository.Subject, action string) (bool, error) {
	r, err := s.repos.GetByName(name)
	if errors.Is(err, repository.ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return s.CanAccessResolved(r, subject, action)
}

// CanAccessResolved 使用已解析仓库快照完成授权，避免协议分派后再次按名称查库。
func (s *RepositoryService) CanAccessResolved(r *repository.Repository, subject repository.Subject, action string) (bool, error) {
	if r == nil {
		return false, ErrNotFound
	}
	if subject.IsAnonymous() {
		return s.canAccessAnonymous(r, action)
	}
	if action == repository.ActionRead && r.Visibility == "public" {
		return true, nil
	}
	return s.acls.HasPermission(r.ID, subject, action)
}

// ReadableRepoIDs 一次性求出 repos 中主体**可读**（ActionRead）的仓库 ID 集合。
//
// 这是 CanAccessResolved(..., ActionRead) 的批量版本，用于「先取一批仓库行、再逐行过滤可见性」
// 的列表端点：逐行调用会按仓库各发一条 COUNT 查询，N 个仓库就是 N 条，而列表端点（尤其是
// 无需认证的公开列表）是外部可反复触发的放大路径。本方法的语句数恒为常数，与仓库数无关。
//
// 语义与逐条调用 CanAccessResolved 严格等价，判定顺序也照抄它：
//   - 匿名主体：先看匿名访问全局开关，关闭即**全部**拒绝（公开仓库也不例外，与
//     canAccessAnonymous 的判定顺序一致），开启再把主体映射到内置 anonymous 用户；
//   - 公开仓库（visibility=public）对 read 直接放行，不查 acl；
//   - 其余仓库按 ACL 判定，口径为「用户自身 ∪ 用户所属任一用户组」，动作蕴含关系由
//     satisfyingGrants 展开。
//
// ACL 判定复用仓储层批量方法 RepoIDsWithPermission（用户分支、组分支各一条 SQL），
// 因此不存在第二套权限口径——若在这里另写一条 SQL，就会在「只授 write 能否读」这类
// 蕴含关系上与逐仓库路径悄悄分叉，那是权限语义分叉，后果比性能问题严重得多。
//
// 返回的 map 只包含可读仓库，未出现的 ID 一律视为不可读；repos 为空时不发任何查询。
// 管理员不受本方法约束（管理面由调用方分支处理），此处不判断管理员身份。
//
// 与 readableRepoIDs 的分工（刻意不合并）：后者服务于搜索 / suggest，需要的是「有序的
// 可搜范围」——公开仓库在前、被授 read 的私有仓库在后，顺序会进入后续 SQL 与结果展示，
// 且它以 ListPublic 为公开集来源（自己的 allRepos 可能被取数上限截断）、以 subjectID 入参
// 内部再展开组归属。本方法只回答「手上这批行里哪些可读」，故直接吃调用方已展开的主体与
// 行快照，不额外查仓库表、不重复展开组。两者的 ACL 判定仍收敛在同一处。
func (s *RepositoryService) ReadableRepoIDs(repos []repository.Repository, subject repository.Subject) (map[int64]bool, error) {
	readable := make(map[int64]bool, len(repos))
	if len(repos) == 0 {
		return readable, nil
	}
	// 匿名主体先过全局开关，且必须在「公开仓库放行」之前：开关关闭时匿名连公开仓库
	// 都不可读，判定顺序反了就会变成一个「关掉开关仍能列出公开仓库」的越权口子。
	aclSubject := subject
	aclDisabled := false
	if subject.IsAnonymous() {
		enabled, err := s.settings.AnonymousAccessEnabled()
		if err != nil {
			return nil, err
		}
		if !enabled {
			return readable, nil
		}
		anonID, err := s.anonymousSubjectID()
		if err != nil {
			return nil, err
		}
		if anonID == 0 {
			// 内置 anonymous 用户缺失：ACL 分支无从判定，退化为「仅公开仓库可读」，
			// 与 canAccessAnonymous 在该情形下的结论一致。
			aclDisabled = true
		} else {
			aclSubject = repository.UserSubject(anonID)
		}
	}
	// 公开仓库对 read 放行；私有仓库收集起来留待一次批量 ACL 判定。
	privateIDs := make([]int64, 0, len(repos))
	for i := range repos {
		if repos[i].Visibility == "public" {
			readable[repos[i].ID] = true
			continue
		}
		privateIDs = append(privateIDs, repos[i].ID)
	}
	if aclDisabled || len(privateIDs) == 0 {
		return readable, nil
	}
	granted, err := s.acls.RepoIDsWithPermission(privateIDs, aclSubject, repository.ActionRead)
	if err != nil {
		return nil, err
	}
	for _, id := range privateIDs {
		if granted[id] {
			readable[id] = true
		}
	}
	return readable, nil
}

// SubjectFor 把「用户 ID」组装为鉴权主体：补上该用户所属的用户组 ID 集合。
//
// 这是全系统构造组感知主体的**唯一入口**：授权判定需要组集合，但各调用点（管理面守卫、
// 协议层 authorize、仓库列表/搜索过滤）拿到的都只是用户 ID，若让它们各自去查组，
// 既重复又容易漏掉某个端点——漏掉的那个端点正好是提权漏洞。故由本方法集中展开，
// 再由调用点把结果透传给 CanAccess / CanAccessResolved。
//
// 用户 ID 为 0（匿名）或组仓储未接线时直接返回无组归属的主体：前者本就不属任何组，
// 后者退化为「仅按用户自身 ACL 判定」，即之前既有的行为。
// 组查询失败返回 error：查不到组就放行会让组授权的拒绝侧开口子，宁可让请求失败。
func (s *RepositoryService) SubjectFor(userID int64) (repository.Subject, error) {
	if userID == 0 || s.groups == nil {
		return repository.UserSubject(userID), nil
	}
	groupIDs, err := s.groups.ListGroupsOfUser(userID)
	if err != nil {
		return repository.Subject{}, err
	}
	return repository.Subject{UserID: userID, GroupIDs: groupIDs}, nil
}

// canAccessAnonymous 判定匿名请求对仓库的访问：全局开关关闭一律拒绝；
// 开启时 public read 放行，其余按 anonymous 主体的 ACL 判定。
// 匿名不属任何组，故主体恒为「仅 anonymous 用户自身」。
func (s *RepositoryService) canAccessAnonymous(r *repository.Repository, action string) (bool, error) {
	enabled, err := s.settings.AnonymousAccessEnabled()
	if err != nil {
		return false, err
	}
	if !enabled {
		return false, nil
	}
	if action == repository.ActionRead && r.Visibility == "public" {
		return true, nil
	}
	anonID, err := s.anonymousSubjectID()
	if err != nil || anonID == 0 {
		return false, err
	}
	return s.acls.HasPermission(r.ID, repository.UserSubject(anonID), action)
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

// ---- Tree API ----

// DirectoryEntry 目录懒加载返回结构：当前层级的子目录（全量）与直接文件。
// FileTotal / FileBytes 是直接文件的总数与体积合计；Files 可能只是其中一页
// （fileLimit <= 0 时取全部）。DirStats 与 Dirs 同序，给出每个子目录子树内的
// 制品总数与最近更新时间（UTC "YYYY-MM-DD HH:MM:SS"）。
type DirectoryEntry struct {
	Dirs      []string
	DirStats  []repository.DirStat
	Files     []repository.Asset
	FileTotal int
	FileBytes int64
}

// ListDirectory 列出仓库指定前缀的当前层级目录与文件（不递归，文件取全部）。
func (s *RepositoryService) ListDirectory(name, prefix string) (*DirectoryEntry, error) {
	return s.ListDirectoryPage(name, prefix, 0, 0)
}

// ListDirectoryPage 列出仓库指定前缀的当前层级子目录（全量）与直接文件（分页）及文件总数。
// 子目录不参与分页：目录项规模只与「同层目录数」相关，而对取数做条数截断会让同层的
// 其它子目录整片消失（列表既不完整也不正确），故只对直接文件分页。
func (s *RepositoryService) ListDirectoryPage(name, prefix string, fileLimit, fileOffset int) (*DirectoryEntry, error) {
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
	children, err := s.assets.ListDirectChildren(repoIDs, prefix, fileLimit, fileOffset)
	if err != nil {
		return nil, err
	}
	dirs := children.Dirs
	dirStats := children.DirStats
	sort.Strings(dirs)
	sort.Slice(dirStats, func(i, j int) bool { return dirStats[i].Path < dirStats[j].Path })
	return &DirectoryEntry{
		Dirs:      dirs,
		DirStats:  dirStats,
		Files:     children.Files,
		FileTotal: children.FileTotal,
		FileBytes: children.FileBytes,
	}, nil
}

// ---- Search API ----

// SearchResult 全局搜索结果条目。
type SearchResult struct {
	RepoName string
	Asset    repository.Asset
}

// SearchFacetResult 搜索结果按仓库聚合的计数（含仓库名，按命中数降序）。
type SearchFacetResult struct {
	RepoName string
	Count    int
}

// 搜索结果的分组维度：`group_by=file` 为既有文件级视图（缺省），
// `group_by=package` 在文件级视图之外额外给出包级聚合。缺省值必须保持 file——仓库内搜索
// 依赖 `path asc` 的文件级结果建树，包级视图只在结果页显式请求。
const (
	SearchGroupByFile    = "file"
	SearchGroupByPackage = "package"
)

// SearchOutput 全局搜索的完整返回：分页条目 + 精确总数 + 仓库聚合。
type SearchOutput struct {
	Items  []SearchResult
	Total  int
	Facets []SearchFacetResult

	// 以下字段只在 `group_by=package` 时填充。file 模式下保持零值，
	// 因此既有响应形状（items / total / facets）逐字段不变。
	Packages     []SearchPackage
	PackageTotal int
	Aggregation  SearchAggregation
}

// SearchAggregation 描述一次包级聚合的扫描情况（响应 aggregation）。
type SearchAggregation struct {
	Scanned   int  // 实际扫描的候选行数（受 maxAggregateRows 预算约束）
	Truncated bool // 是否因预算不足被截断（此时各计数为下界，total 仍是精确值）
}

// searchScope 是一次搜索解析出的「可搜范围」：表达式、主查询仓库集合、facet 统计范围
// 与仓库全量表。SearchAssets 与 SearchAggregate 共用它，避免 ACL 与表达式收窄逻辑出现
// 两份实现（两份必然漂移，而漂移的后果是越权或漏搜）。
type searchScope struct {
	parsed   parsedSearch
	repoIDs  []int64                 // 主查询范围；nil 表示不限（管理员搜全部）
	facetIDs []int64                 // facet 统计范围（忽略 repo: 正向限定，供钻取切换）
	allRepos []repository.Repository // 仓库全量表，供名称/格式映射复用
	hasQuery bool                    // 是否需要对 asset 表发起主查询
}

// readableRepoIDs 计算一次请求的**可读仓库集合**（规格 §3.5 / §3.7 衔接点 2）。
//
// 返回：
//   - repoIDs —— 可读仓库 ID；**nil 表示不限**（管理员搜全部）。顺序为「public 在前、
//     被授 read 的私有仓库在后（按 allRepos 顺序）」，与引入本函数之前逐仓库判定时一致。
//   - readable —— false 表示本次请求必然为空集（匿名被关闭，或非管理员没有任何可读仓库）。
//     调用方应返回空结果而不是报错：suggest 若对此返回 403，就把「这里存在一个你看不到的
//     仓库」当信号泄漏出去了（规格 §3.8）。
//   - err —— 真实故障（设置读取 / 主体展开 / ACL 查询失败），原样上抛。
//
// **本函数是「允许仓库集合」判定的唯一实现**：搜索主链路（resolveSearchScope）与 suggest
// 都调用它，不新增第三套权限逻辑。ACL 判定本身复用仓储层的批量方法 RepoIDsWithPermission
// （固定两条 SQL，与逐仓库 HasPermission 结果一致）。
//
// allRepos/allErr 由调用方传入：仓库全量列举是同一请求内多处共用的产物（表达式收窄、仓库名
// 映射、suggest 的 format 映射），此处不再重复查询。
//
// allErr 非空时**上抛**：以「只搜公开仓库」继续会让已授权私有仓库的用户看到结果数量莫名下降，
// 而链路上没有任何信号。搜索主链路已有 writeDomainErr 兜底（管理员可见原始错误），
// 保守失败比静默降级正确。
func (s *RepositoryService) readableRepoIDs(subjectID int64, isAdmin bool, allRepos []repository.Repository, allErr error) ([]int64, bool, error) {
	if isAdmin {
		// 管理员搜全部：nil 表示不限。
		return nil, true, nil
	}
	// 匿名请求：开关关闭直接返回空集（handler 另拦 401，此处兜底）。
	aclSubject := subjectID
	if subjectID == 0 {
		enabled, err := s.settings.AnonymousAccessEnabled()
		if err != nil {
			return nil, false, err
		}
		if !enabled {
			return nil, false, nil
		}
		if aclSubject, err = s.anonymousSubjectID(); err != nil {
			return nil, false, err
		}
	}
	pubRepos, err := s.repos.ListPublic()
	if err != nil {
		return nil, false, err
	}
	repoIDs := make([]int64, 0, len(pubRepos))
	for _, r := range pubRepos {
		repoIDs = append(repoIDs, r.ID)
	}
	// 已登录主体或匿名映射到的 anonymous 主体：加上 ACL 授权的私有仓库。
	// 主体按用户 ID 展开组归属，使「组被授 read」的仓库也进入可搜范围。
	// 判定是**一次批量查询**（用户分支 + 组分支各一条 SQL），不随仓库数增长。
	//
	// 全量列举失败时不降级：没有 allRepos 就无法区分「私有仓库」与「公开仓库」，
	// 只能退回「仅公开」，那会让已授权私有仓库的用户看到结果数量下降却毫无信号。
	if aclSubject > 0 {
		if allErr != nil {
			return nil, false, allErr
		}
		pubSet := make(map[int64]bool, len(pubRepos))
		for _, r := range pubRepos {
			pubSet[r.ID] = true
		}
		privateIDs := make([]int64, 0, len(allRepos))
		for _, r := range allRepos {
			if pubSet[r.ID] {
				continue
			}
			privateIDs = append(privateIDs, r.ID)
		}
		subject, err := s.SubjectFor(aclSubject)
		if err != nil {
			return nil, false, err
		}
		granted, err := s.acls.RepoIDsWithPermission(privateIDs, subject, repository.ActionRead)
		if err != nil {
			return nil, false, err
		}
		// 按 allRepos 的顺序追加，保持「公开在前、私有在后」的既有可搜范围顺序。
		for _, r := range allRepos {
			if pubSet[r.ID] || !granted[r.ID] {
				continue
			}
			repoIDs = append(repoIDs, r.ID)
		}
	}
	if len(repoIDs) == 0 {
		return nil, false, nil
	}
	return repoIDs, true, nil
}

// resolveSearchScope 解析表达式并计算可搜范围。
// 返回 ok=false 表示本次搜索必然为空集（匿名被关闭 / 没有任何可读仓库），调用方直接返回空。
func (s *RepositoryService) resolveSearchScope(keyword, repoScope string, subjectID int64, isAdmin bool) (*searchScope, bool, error) {
	parsed := parseSearchQuery(keyword)
	// 索引是否可用：每次搜索判定一次（重建会把 completed_at 打回 NULL，因此不缓存），
	// 结果下推给仓储层决定走 FTS 候选集还是原 LIKE 路径。
	indexReady, err := s.assets.SearchIndexReady()
	if err != nil {
		return nil, false, err
	}
	parsed.filter.UseFullTextIndex = indexReady
	// allRepos 是本请求对仓库表的**唯一**一次全量列举：ACL 候选、表达式收窄与仓库名映射都
	// 复用它，避免同一请求内向仓库表发多次「取全量」，也避免名字映射按结果条目逐个回查。
	allRepos, allErr := s.repos.List(1000, 0)
	// 可读仓库集合：匿名开关、public ∪ 被授 read 的私有仓库、批量 ACL 判定都收敛在
	// readableRepoIDs 里，搜索与 suggest 共用同一份实现（规格 §3.7 衔接点 2）。
	repoIDs, readable, err := s.readableRepoIDs(subjectID, isAdmin, allRepos, allErr)
	if err != nil {
		return nil, false, err
	}
	if !readable {
		return nil, false, nil
	}
	// repo:/format: 表达式或单仓库限定：按仓库元数据收窄搜索范围。
	// facetIDs 是聚合统计用的范围：忽略 repo: 正向限定（保留 -repo:/format: 等），
	// 让用户点选某仓库钻取后，聚合条上仍能看到其他仓库的命中数以便切换。
	facetIDs := repoIDs
	if repoScope != "" || parsed.hasRepoMeta() {
		// 非管理员的全量列举失败已在 readableRepoIDs 里上抛；这里兜住**管理员**：
		// 管理员不走批量 ACL 分支，不会触发那次上抛，而收窄仍然需要 allRepos。
		if allErr != nil {
			return nil, false, allErr
		}
		// allowed 为 nil 表示不限（管理员搜全部），与既有 isAdmin 分支等价。
		var allowed map[int64]bool
		if !isAdmin {
			allowed = make(map[int64]bool, len(repoIDs))
			for _, id := range repoIDs {
				allowed[id] = true
			}
		}
		repoIDs = filterReposByExpr(allRepos, allowed, repoScope, parsed, true)
		facetIDs = filterReposByExpr(allRepos, allowed, repoScope, parsed, false)
		if len(facetIDs) == 0 {
			return nil, false, nil
		}
	}
	hasQuery := len(repoIDs) > 0 || (isAdmin && repoScope == "" && !parsed.hasRepoMeta())
	return &searchScope{
		parsed:   parsed,
		repoIDs:  repoIDs,
		facetIDs: facetIDs,
		allRepos: allRepos,
		hasQuery: hasQuery,
	}, true, nil
}

// SearchAssets 跨仓库搜索制品路径，按可读权限过滤。
// keyword 支持高级表达式（见 search_query.go）：-词 负筛选、repo:/-repo: 仓库
// 限定、format:/-format: 格式筛选、ext:/-ext: 扩展名筛选、version:/-version: 版本段
// 严格匹配、"引号短语"。
// repoScope 非空时限定单仓库（浏览页内搜索，条件下推保证 total 精确）。
// subjectID==0 表示匿名：受全局开关约束，可搜范围 = public ∪ anonymous 主体
// 被授 read 的仓库；isAdmin 则搜全部。
// 已认证主体的可搜范围含「其所属用户组被授 read」的仓库。
// sort/order 控制结果排序，取值见 AssetRepo.SearchByFilter。
//
// ctx 是搜索链路的超时来源：本方法在入口按 searchTimeout 派生一次 deadline，它**只覆盖
// asset 检索语句**（SearchAggregateByFilter 一次下发的计数 / 分页 / facet 聚合三条查询），
// 命中 DeadlineExceeded 归一为 ErrSearchTimeout。可搜范围解析那一段（SearchIndexReady、
// 仓库全量列举、主体展开、批量 ACL 判定）目前不接收 ctx，因此**不受这份预算约束**——那些是
// 窄表上的小查询，排查「为什么慢」时不要按「整条链路都在预算内」的假设去找。
//
// 客户端断开（context.Canceled）**不**归一为超时，原因原样返回，见 normalizeSearchErr。
//
// 三个调用方（/api/v1/search、npm registrySearch、suggest）共享同一套语义——放在 handler 层
// 会让每个调用方各写一遍，放在仓储层则拿不到「一次搜索的总预算」。
//
// 本方法**只做文件级搜索**：包级聚合在新增的 SearchAggregate 里，两者的签名与
// 响应形状互不影响，因此 npm registrySearch 直接调用本方法时行为一字不变（规格 §3.8）。
func (s *RepositoryService) SearchAssets(ctx context.Context, keyword, repoScope string, subjectID int64, isAdmin bool, sort, order string, limit, offset int) (*SearchOutput, error) {
	ctx, cancel := s.searchContext(ctx)
	defer cancel()

	empty := &SearchOutput{Items: []SearchResult{}, Facets: []SearchFacetResult{}}
	scope, ok, err := s.resolveSearchScope(keyword, repoScope, subjectID, isAdmin)
	if err != nil {
		return nil, err
	}
	if !ok {
		return empty, nil
	}
	parsed, repoIDs, facetIDs, allRepos := scope.parsed, scope.repoIDs, scope.facetIDs, scope.allRepos
	// 计数 / 分页 / facet 聚合一并下推到**同一个只读快照**（SearchAggregateByFilter）：
	// 逐条调用时每个方法各开一份快照，total、items 与 facets 可能来自三个不同版本的数据。
	// 无主查询条件（hasQuery=false）时跳过计数与分页，只做 facet 统计——与逐条调用时一致。
	res, err := s.assets.SearchAggregateByFilter(ctx, repository.SearchAggregateQuery{
		Filter:        parsed.filter,
		RepoIDs:       repoIDs,
		FacetIDs:      facetIDs,
		Sort:          sort,
		Order:         order,
		Limit:         limit,
		Offset:        offset,
		SkipAssetRows: !scope.hasQuery,
	})
	if err != nil {
		return nil, normalizeSearchErr(ctx, err)
	}
	assets, total, rawFacets := res.Items, res.Total, res.Facets
	// 构建 repoID -> name 映射（结果条目与 facets 共用）：复用上面的全量仓库列表一次遍历，
	// 未命中才回落单条查询（防御「两次查询之间仓库被删」的窗口；返回值口径不变，查不到为空串）。
	idNameMap := make(map[int64]string, len(allRepos))
	for _, r := range allRepos {
		idNameMap[r.ID] = r.Name
	}
	repoName := func(id int64) string {
		if name, ok := idNameMap[id]; ok {
			return name
		}
		if r, _ := s.repos.GetByID(id); r != nil {
			idNameMap[id] = r.Name
			return r.Name
		}
		idNameMap[id] = ""
		return ""
	}
	out := &SearchOutput{
		Items:  make([]SearchResult, 0, len(assets)),
		Total:  total,
		Facets: make([]SearchFacetResult, 0, len(rawFacets)),
	}
	for i := range assets {
		out.Items = append(out.Items, SearchResult{RepoName: repoName(assets[i].RepositoryID), Asset: assets[i]})
	}
	for _, f := range rawFacets {
		out.Facets = append(out.Facets, SearchFacetResult{RepoName: repoName(f.RepositoryID), Count: f.Count})
	}
	return out, nil
}

// SearchAggregate 在文件级搜索结果之外追加包级聚合（见 §3.7）。
//
// groupBy 取 SearchGroupByFile（缺省）或 SearchGroupByPackage。file 模式直接委托给
// SearchAssets——**行为、签名与下发的 SQL 语句数完全不变**，npm registrySearch 因此不受本 FR
// 影响（规格 §3.8 理由 1）。
//
// package 模式在 SearchAssets 的全部既有产物（items / total / facets，语义与分页都不变）
// 之外多扫一次候选行（预算 maxAggregateRows），在 Go 侧聚合成
// packages / packageTotal / aggregation。`total` 仍来自 COUNT(*)，不因聚合改写。
//
// 这四条查询（计数、分页、facet 聚合、聚合扫描）**共用同一个只读快照**
// （AssetRepo.SearchAggregateByFilter）。逐条调用各仓储方法时每个方法各开一份快照，
// 共用 WHERE 只保证条件相同、不保证数据版本相同：COUNT 之后命中的资产若被并发删除，
// 响应就可能是 total=1 而 packages=[] 且 truncated=false——搜索页只渲染 packages，
// 于是用户看到「无结果」，而响应自身的前后字段互相矛盾。
func (s *RepositoryService) SearchAggregate(ctx context.Context, keyword, repoScope string, subjectID int64, isAdmin bool, sort, order, groupBy string, limit, offset int) (*SearchOutput, error) {
	if groupBy != SearchGroupByPackage {
		return s.SearchAssets(ctx, keyword, repoScope, subjectID, isAdmin, sort, order, limit, offset)
	}
	ctx, cancel := s.searchContext(ctx)
	defer cancel()

	empty := &SearchOutput{Items: []SearchResult{}, Facets: []SearchFacetResult{}, Packages: []SearchPackage{}}
	scope, ok, err := s.resolveSearchScope(keyword, repoScope, subjectID, isAdmin)
	if err != nil {
		return nil, err
	}
	if !ok {
		return empty, nil
	}
	parsed, repoIDs, facetIDs, allRepos := scope.parsed, scope.repoIDs, scope.facetIDs, scope.allRepos
	// 一次快照内跑完四条查询（详见函数注释与 SearchAggregateQuery）。
	// 聚合扫描与分页/计数共用同一段 WHERE，因此聚合看到的就是「命中的全部文件」的同一版本。
	res, err := s.assets.SearchAggregateByFilter(ctx, repository.SearchAggregateQuery{
		Filter:        parsed.filter,
		RepoIDs:       repoIDs,
		FacetIDs:      facetIDs,
		Sort:          sort,
		Order:         order,
		Limit:         limit,
		Offset:        offset,
		MaxRows:       maxAggregateRows,
		SkipAssetRows: !scope.hasQuery,
	})
	if err != nil {
		return nil, normalizeSearchErr(ctx, err)
	}
	assets, total, rawFacets := res.Items, res.Total, res.Facets
	rows, truncated := res.Rows, res.Truncated

	// 仓库元数据映射：复用同一次全量列举，未命中的才回查单条（防御「两次查询之间仓库被删」）。
	byID := make(map[int64]repository.Repository, len(allRepos))
	for _, r := range allRepos {
		byID[r.ID] = r
	}
	repoOf := func(id int64) repository.Repository {
		if r, ok := byID[id]; ok {
			return r
		}
		r, _ := s.repos.GetByID(id)
		if r == nil {
			// 仓库已不存在：名称为空、格式为空 → ParseCoordinates 一律降级为无坐标条目，
			// 条目本身仍在结果里（不因仓库消失而丢行）。
			byID[id] = repository.Repository{ID: id}
			return byID[id]
		}
		byID[id] = *r
		return *r
	}

	sources := make([]PackageSource, 0, len(rows))
	for i := range rows {
		repo := repoOf(rows[i].RepositoryID)
		sources = append(sources, PackageSource{RepoName: repo.Name, Format: repo.Format, Asset: rows[i]})
	}
	// 精度判定只看**普通关键词**（IncludeTerms）：version:/ext:/repo: 等限定词不是相关度信号。
	packages := BuildPackageGroups(sources, parsed.filter.IncludeTerms)
	SortSearchPackages(packages)

	out := &SearchOutput{
		Items:        make([]SearchResult, 0, len(assets)),
		Total:        total,
		Facets:       make([]SearchFacetResult, 0, len(rawFacets)),
		Packages:     packages,
		PackageTotal: len(packages),
		Aggregation:  SearchAggregation{Scanned: len(rows), Truncated: truncated},
	}
	repoName := func(id int64) string { return repoOf(id).Name }
	for i := range assets {
		out.Items = append(out.Items, SearchResult{RepoName: repoName(assets[i].RepositoryID), Asset: assets[i]})
	}
	for _, f := range rawFacets {
		out.Facets = append(out.Facets, SearchFacetResult{RepoName: repoName(f.RepositoryID), Count: f.Count})
	}
	return out, nil
}

// SearchAssetsPerRepo 在给定仓库集合内**按仓库分别**取前 perRepoLimit 条命中（各自 path 升序）。
//
// 供 npm registry 搜索使用：它原本对 group 的每个成员各调用一次完整 SearchAssets——成员越多
// 越慢（每个成员都会跑 COUNT + 分页 SELECT + facet 聚合，还要再算一遍可搜范围与仓库名映射），
// 且每次都把 `from+size` 行拉回内存再去重。这里用**一条语句**完成「每个成员各取前 N 条」，
// 成本与成员数解耦，返回的行集与「逐成员各搜一次」逐条一致，去重与 total 口径仍由调用方在
// Go 侧按成员顺序完成（语义不变）。
//
// 不做**主体级** ACL 判定：repoIDs 由调用方决定并承担授权责任。npm group 搜索的口径是
// 「对 group 可读即视为可读其成员」，调用方只校验了 group 自身的 read 权限，成员仓库自身的
// ACL **没有**被检查——这是既有产品口径，不是本方法的疏漏；若将来要收紧，需在调用方逐个成员
// 做授权判定，本方法只负责按给定 id 集合检索。
func (s *RepositoryService) SearchAssetsPerRepo(ctx context.Context, keyword string, repoIDs []int64, perRepoLimit int) ([]repository.Asset, error) {
	if len(repoIDs) == 0 || perRepoLimit <= 0 {
		return nil, nil
	}
	ctx, cancel := s.searchContext(ctx)
	defer cancel()

	parsed := parseSearchQuery(keyword)
	indexReady, err := s.assets.SearchIndexReady()
	if err != nil {
		return nil, err
	}
	parsed.filter.UseFullTextIndex = indexReady
	// repo:/format: 仍按仓库元数据收窄（与逐仓库搜索时的收窄口径共用同一个函数）。
	if parsed.hasRepoMeta() {
		allRepos, err := s.repos.List(1000, 0)
		if err != nil {
			return nil, err
		}
		allowed := make(map[int64]bool, len(repoIDs))
		for _, id := range repoIDs {
			allowed[id] = true
		}
		repoIDs = filterReposByExpr(allRepos, allowed, "", parsed, true)
	}
	assets, err := s.assets.SearchPerRepoByFilter(ctx, parsed.filter, repoIDs, perRepoLimit)
	if err != nil {
		return nil, normalizeSearchErr(ctx, err)
	}
	return assets, nil
}

// searchContext 为一次搜索派生带预算的 ctx。
//
// 这份预算**只覆盖下发到 SQL 的 asset 检索语句**（COUNT / 分页 SELECT / facet 聚合 /
// suggest 的候选扫描 / npm 的按成员检索），它们共享同一个 deadline，因此用户感知到的
// 「检索语句花了多久」受它约束。
//
// 它**不覆盖**可搜范围的解析：SearchIndexReady、仓库全量列举、SubjectFor、
// RepoIDsWithPermission、ListPublic 都不接收 ctx，慢在这里不会触发 ErrSearchTimeout。
// 这些是窄表上的小查询，现状可接受；但排查「一次搜索为什么慢」时必须知道预算没有覆盖它们。
// 后续若要让预算覆盖整条链路，正确做法是给这些仓储方法补 ctx 版本（SelectContext 系列），
// 而不是靠这里调大上限。ctx 为空（未接线的旧调用）时退化为 context.Background()，
// 仍受同一个上限保护。
func (s *RepositoryService) searchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, s.searchTimeout)
}

// normalizeSearchErr 把搜索链路的**预算耗尽**归一为 ErrSearchTimeout。
//
// 只归一 DeadlineExceeded：上层传进来的是请求上下文，客户端断开（context.Canceled）是高频
// 事件，若也记成「超过搜索超时」，超时这个运维信号会长期虚高，把真正的预算不足埋掉。取消
// 原样返回，由 API 层按「请求已被取消」处理（客户端已经走了，既不需要 504 也不需要告警）。
//
// 驱动在 ctx 超时后返回的错误文案不统一（context deadline exceeded 或被包装过的版本），
// 因此同时用 errors.Is 与 ctx 自身状态两条路径判定；非超时错误原样返回，不掩饰真实故障。
func normalizeSearchErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		(ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return fmt.Errorf("%w（%v）", ErrSearchTimeout, err)
	}
	return err
}

// filterReposByExpr 按 repo:/format: 表达式与单仓库限定收窄仓库集合。
//
// allowed 为 nil 表示不限（管理员搜全部）；withPositiveRepos 为 false 时忽略 repo: 正向限定，
// 供 facet 统计保留「其它仓库的命中数」以便钻取切换。本函数是收窄口径的**唯一**实现，
// 搜索主链路与 npm 的按成员搜索都复用它，避免两处判定漂移。
func filterReposByExpr(all []repository.Repository, allowed map[int64]bool, repoScope string, p parsedSearch, withPositiveRepos bool) []int64 {
	var out []int64
	for _, r := range all {
		if allowed != nil && !allowed[r.ID] {
			continue
		}
		if repoScope != "" && r.Name != repoScope {
			continue
		}
		if withPositiveRepos && len(p.repos) > 0 && !slices.Contains(p.repos, r.Name) {
			continue
		}
		if slices.Contains(p.notRepos, r.Name) {
			continue
		}
		if len(p.formats) > 0 && !slices.Contains(p.formats, r.Format) {
			continue
		}
		if slices.Contains(p.notFormats, r.Format) {
			continue
		}
		out = append(out, r.ID)
	}
	return out
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
