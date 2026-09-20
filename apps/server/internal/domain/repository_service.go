package domain

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// AnonymousUsername 是内置匿名主体的用户名（FR-66）：迁移脚本创建，
// 不可登录、不可删除 / 改密 / 停用；ACL 中作为普通主体承接匿名授权。
const AnonymousUsername = "anonymous"

// RepositoryService 处理仓库管理、ACL 与授权判定。
type RepositoryService struct {
	repos     *repository.RepoRepo
	acls      *repository.AclRepo
	assets    *repository.AssetRepo
	metadata  *repository.FormatMetadataRepo
	settings  *SettingService
	users     *repository.UserRepo
	mutator   *AssetMutationCoordinator
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
	enabled   formats.Set
}

// NewRepositoryService 构造 RepositoryService。settings 与 users 供匿名判定
// （全局开关 + anonymous 主体 ACL，FR-66）使用。
func NewRepositoryService(repos *repository.RepoRepo, acls *repository.AclRepo, assets *repository.AssetRepo, settings *SettingService, users *repository.UserRepo) *RepositoryService {
	return &RepositoryService{repos: repos, acls: acls, assets: assets, settings: settings, users: users, enabled: formats.Default()}
}

// SetEnabledFormats 注入启动期格式能力集合。集合仅在装配阶段设置，运行中不切换。
func (s *RepositoryService) SetEnabledFormats(enabled formats.Set) { s.enabled = enabled }

// EnabledFormats 返回当前进程启用的格式，供管理面展示。
func (s *RepositoryService) EnabledFormats() []string { return s.enabled.List() }

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
func (s *RepositoryService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *RepositoryService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

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
// 校验不过返回 ErrValidation；仓库名重复返回 ErrConflict。
func (s *RepositoryService) Create(name, format, typ, visibility, description string, cfg repository.RepositoryConfig) (*repository.Repository, error) {
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
	if _, err := s.repos.Create(name, format, typ, visibility, configJSON); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	if description != "" {
		if err := s.repos.UpdateDescription(name, description); err != nil {
			return nil, err
		}
	}
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, err
	}
	s.recordChange(EntityRepository, RepoKey(name), OpPut, RepoChangeData{
		Name: name, Format: repo.Format, Type: repo.Type,
		Visibility: repo.Visibility, Description: repo.Description, Config: repo.Config,
	})
	return repo, nil
}

// Update 更新仓库可见性、描述与/或结构化配置。visibility 为空表示不改；
// description 为 nil 表示不改（指向空串表示清空）；cfg 非 nil 时
// 按仓库当前 format/type 重新校验并覆盖写 config。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Update(name, visibility string, description *string, cfg *repository.RepositoryConfig) (*repository.Repository, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
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
	if description != nil {
		if err := s.repos.UpdateDescription(name, *description); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if visibility != "" {
		if err := s.repos.UpdateVisibility(name, visibility); err != nil {
			return nil, mapNotFound(err)
		}
	}
	updated, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	s.recordChange(EntityRepository, RepoKey(name), OpPut, RepoChangeData{
		Name: name, Format: updated.Format, Type: updated.Type,
		Visibility: updated.Visibility, Description: updated.Description, Config: updated.Config,
	})
	return updated, nil
}

// SetOnline 设置仓库 online/offline 状态（FR-113）。
// 直接落库、不写复制变更日志：online 是节点本地运维状态（M-2 硬约束），
// 不得经复制传播到对端覆盖其本地状态。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) SetOnline(name string, online bool) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if err := s.repos.SetOnline(name, online); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// validateConfig 按仓库类型校验结构化配置：
//   - hosted：remoteUrl、credentialRef 与 members 均须为空；
//   - proxy：remoteUrl 必填且为合法 http/https 绝对地址，members 与 immutableRelease 须为空或 false，credentialRef 可选且必须是环境变量引用名；
//   - group：members 必填（≥1），每个成员须存在、与本仓 format 一致且非自引用，remoteUrl、credentialRef 与 immutableRelease 须为空或 false。
//
// 违规返回 ErrValidation。
func (s *RepositoryService) validateConfig(name, format, typ string, cfg repository.RepositoryConfig) error {
	switch typ {
	case "hosted":
		if cfg.RemoteURL != "" || cfg.CredentialRef != "" || len(cfg.Members) > 0 {
			return ErrValidation
		}
	case "proxy":
		if len(cfg.Members) > 0 || cfg.RemoteURL == "" || cfg.ImmutableRelease {
			return ErrValidation
		}
		u, err := url.Parse(cfg.RemoteURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ErrValidation
		}
		if cfg.CredentialRef != "" && !validCredentialRef(cfg.CredentialRef) {
			return ErrValidation
		}
	case "group":
		if cfg.RemoteURL != "" || cfg.CredentialRef != "" || cfg.ImmutableRelease || len(cfg.Members) == 0 {
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
			return s.repos.DeleteTx(tx, name)
		})
		if err != nil {
			return err
		}
	} else if err := s.repos.Delete(name); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityRepository, RepoKey(name), OpDelete, TombstoneData{Deleted: true})
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

// SetAcl 覆盖写入仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) SetAcl(name string, entries []repository.Acl) ([]repository.Acl, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
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

// ---- FR-30: Search API ----

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

// SearchOutput 全局搜索的完整返回：分页条目 + 精确总数 + 仓库聚合。
type SearchOutput struct {
	Items  []SearchResult
	Total  int
	Facets []SearchFacetResult
}

// SearchAssets 跨仓库搜索制品路径，按可读权限过滤。
// keyword 支持高级表达式（见 search_query.go）：-词 负筛选、repo:/-repo: 仓库
// 限定、format:/-format: 格式筛选、ext:/-ext: 扩展名筛选、"引号短语"。
// repoScope 非空时限定单仓库（浏览页内搜索，条件下推保证 total 精确）。
// subjectID==0 表示匿名：受全局开关约束，可搜范围 = public ∪ anonymous 主体
// 被授 read 的仓库（FR-66）；isAdmin 则搜全部。
// sort/order 控制结果排序，取值见 AssetRepo.SearchByFilter。
func (s *RepositoryService) SearchAssets(keyword, repoScope string, subjectID int64, isAdmin bool, sort, order string, limit, offset int) (*SearchOutput, error) {
	empty := &SearchOutput{Items: []SearchResult{}, Facets: []SearchFacetResult{}}
	parsed := parseSearchQuery(keyword)
	// 匿名请求：开关关闭直接返回空集（handler 另拦 401，此处兜底）。
	aclSubject := subjectID
	if subjectID == 0 && !isAdmin {
		enabled, err := s.settings.AnonymousAccessEnabled()
		if err != nil {
			return nil, err
		}
		if !enabled {
			return empty, nil
		}
		if aclSubject, err = s.anonymousSubjectID(); err != nil {
			return nil, err
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
			return nil, err
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
			return empty, nil
		}
	}
	// repo:/format: 表达式或单仓库限定：按仓库元数据收窄搜索范围。
	// facetIDs 是聚合统计用的范围：忽略 repo: 正向限定（保留 -repo:/format: 等），
	// 让用户点选某仓库钻取后，聚合条上仍能看到其他仓库的命中数以便切换。
	facetIDs := repoIDs
	if repoScope != "" || parsed.hasRepoMeta() {
		allRepos, err := s.repos.List(1000, 0)
		if err != nil {
			return nil, err
		}
		allowed := make(map[int64]bool, len(repoIDs))
		for _, id := range repoIDs {
			allowed[id] = true
		}
		narrow := func(withPositiveRepos bool) []int64 {
			var out []int64
			for _, r := range allRepos {
				if !isAdmin && !allowed[r.ID] {
					continue
				}
				if repoScope != "" && r.Name != repoScope {
					continue
				}
				if withPositiveRepos && len(parsed.repos) > 0 && !slices.Contains(parsed.repos, r.Name) {
					continue
				}
				if slices.Contains(parsed.notRepos, r.Name) {
					continue
				}
				if len(parsed.formats) > 0 && !slices.Contains(parsed.formats, r.Format) {
					continue
				}
				if slices.Contains(parsed.notFormats, r.Format) {
					continue
				}
				out = append(out, r.ID)
			}
			return out
		}
		repoIDs = narrow(true)
		facetIDs = narrow(false)
		if len(facetIDs) == 0 {
			return empty, nil
		}
	}
	var (
		assets []repository.Asset
		total  int
	)
	if len(repoIDs) > 0 || (isAdmin && repoScope == "" && !parsed.hasRepoMeta()) {
		var err error
		assets, total, err = s.assets.SearchByFilter(parsed.filter, repoIDs, sort, order, limit, offset)
		if err != nil {
			return nil, err
		}
	}
	// 同一路径条件的按仓库聚合（钻取导航用）
	rawFacets, err := s.assets.SearchFacetsByFilter(parsed.filter, facetIDs)
	if err != nil {
		return nil, err
	}
	// 构建 repoID -> name 映射（结果条目与 facets 共用）
	idNameMap := make(map[int64]string)
	repoName := func(id int64) string {
		name, ok := idNameMap[id]
		if !ok {
			if r, _ := s.repos.GetByID(id); r != nil {
				name = r.Name
			}
			idNameMap[id] = name
		}
		return name
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
