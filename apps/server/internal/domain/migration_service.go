package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/offindex"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// MigrationService 迁移任务状态机：创建 planned、discover 落库、start/cancel/resume 守卫。
// 异步执行由 Runner 钩子接入（execute 增量）。
type MigrationService struct {
	tasks         *repository.MigrationTaskRepo
	runner        MigrationRunner // 可选；nil 时 Start/Resume 仅更新状态
	sourceFactory MigrationSourceFactory
	// 离线目录索引（可选）：就绪时 discover 走索引，避免反复扫 blob
	offlineIndex     *repository.OfflineIndexRepo
	offlineScan      OfflineIndexScanner
	writeGate        BusinessWriteGate
	credentialSealer *credential.Sealer
}

// SourceAuth 是 online REST 的仅写入来源认证。
type SourceAuth = credential.SourceAuth

// MigrationSourceFactory 按来源类型构造发现器。
type MigrationSourceFactory func(sourceType string) (discover.Source, error)

// OfflineIndexScanner 前置扫描接口。
type OfflineIndexScanner interface {
	StartScan(root string, mode string) error
	Cancel(root string)
	Status(root string) (*repository.OfflineDirIndex, error)
}

// MigrationRunner 异步执行钩子；nil 时 Start/Resume 仅更新状态。
type MigrationRunner interface {
	// StartAsync 在任务已进入 running 后启动后台搬运；不得阻塞调用方。
	StartAsync(taskID int64)
	// Cancel 协作取消正在运行的任务（可选；无实现则仅改 DB 状态）。
	Cancel(taskID int64)
}

// MigrationFinalizerRunner 可选：Runner 同时支持 finalize。
type MigrationFinalizerRunner interface {
	MigrationRunner
	Finalize(ctx context.Context, taskID int64) error
}

// MigrationPreflightRunner 在任务进入 running 前验证目标拓扑，必须不产生写入。
type MigrationPreflightRunner interface {
	Preflight(task *repository.MigrationTask) error
}

// NewMigrationService 构造 MigrationService。
func NewMigrationService(tasks *repository.MigrationTaskRepo, runner MigrationRunner, factories ...MigrationSourceFactory) *MigrationService {
	factory := MigrationSourceFactory(discover.NewSource)
	if len(factories) > 0 && factories[0] != nil {
		factory = factories[0]
	}
	return &MigrationService{tasks: tasks, runner: runner, sourceFactory: factory}
}

// SetOfflineIndex 注入离线目录索引与扫描器。
func (s *MigrationService) SetOfflineIndex(idx *repository.OfflineIndexRepo, scan OfflineIndexScanner) {
	s.offlineIndex = idx
	s.offlineScan = scan
}

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *MigrationService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// SetCredentialSealer 注入 online REST 任务认证的 AES-256-GCM 密封器。
func (s *MigrationService) SetCredentialSealer(sealer *credential.Sealer) {
	s.credentialSealer = sealer
}

// StartOfflineIndexScan 启动离线目录前置索引（full/update/rebuild）。
func (s *MigrationService) StartOfflineIndexScan(root, mode string) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if s.offlineScan == nil {
		return fmt.Errorf("%w: 离线索引服务未启用", ErrValidation)
	}
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("%w: path 不能为空", ErrValidation)
	}
	if err := s.offlineScan.StartScan(root, mode); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

// CancelOfflineIndexScan 取消扫描。
func (s *MigrationService) CancelOfflineIndexScan(root string) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if s.offlineScan == nil {
		return fmt.Errorf("%w: 离线索引服务未启用", ErrValidation)
	}
	s.offlineScan.Cancel(root)
	return nil
}

// OfflineIndexStatus 查询索引状态；repos 为索引内仓计数摘要。
func (s *MigrationService) OfflineIndexStatus(root string) (*repository.OfflineDirIndex, map[string]int64, error) {
	if s.offlineScan == nil || s.offlineIndex == nil {
		return nil, nil, fmt.Errorf("%w: 离线索引服务未启用", ErrValidation)
	}
	if strings.TrimSpace(root) == "" {
		return nil, nil, fmt.Errorf("%w: path 不能为空", ErrValidation)
	}
	meta, err := s.offlineScan.Status(root)
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]int64{}
	if meta.Status == repository.OfflineIndexReady {
		abs := meta.RootPath
		if c, err := s.offlineIndex.CountByRepo(abs); err == nil {
			counts = c
		}
	}
	return meta, counts, nil
}

// MigrationCreateInput 创建 planned 任务的入参。
type MigrationCreateInput struct {
	SourceType     string
	SourceConfig   map[string]any // 序列化为 JSON；无密钥
	CredentialRef  string         // 可空；在线源建议填写
	SourceAuth     *SourceAuth    // 可空；仅 online_rest 的直接来源认证
	ConflictPolicy string         // skip|overwrite|fail，默认 skip
	PlanJSON       string         // 可空，默认 "{}"
	Initiator      MigrationInitiator
}

// MigrationInitiator 是创建或发现迁移任务的认证身份快照。
// 只保存审计所需的用户名、稳定用户 ID 与认证来源，不包含令牌或来源凭据。
type MigrationInitiator struct {
	Username   string
	UserID     int64
	AuthSource string
}

// Create 创建状态为 planned 的任务；不自动 running。
// 未知 credentialRef（环境变量不存在）→ ErrValidation。
func (s *MigrationService) Create(in MigrationCreateInput) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	if err := validateSourceType(in.SourceType); err != nil {
		return nil, err
	}
	policy, err := normalizeConflictPolicy(in.ConflictPolicy)
	if err != nil {
		return nil, err
	}
	if err := validateDirectSourceAuth(in.SourceType, in.CredentialRef, in.SourceAuth); err != nil {
		return nil, err
	}
	if err := s.checkCredentialRef(in.CredentialRef, in.SourceType); err != nil {
		return nil, err
	}
	safeConfig, err := discover.SanitizeSourceConfig(in.SourceType, in.SourceConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	safeConfig, err = discover.PersistedSourceConfig(in.SourceType, safeConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	cfgJSON, err := encodeSourceConfig(safeConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	authType, authCiphertext, err := s.encryptSourceAuth(in.SourceAuth, cfgJSON)
	if err != nil {
		return nil, err
	}
	plan := in.PlanJSON
	if plan == "" {
		plan = "{}"
	}
	if !json.Valid([]byte(plan)) {
		return nil, fmt.Errorf("%w: plan 须为合法 JSON", ErrValidation)
	}
	plan, err = filterPlanJSON(plan, parseIncludeRepos(in.SourceConfig["includeRepositories"]))
	if err != nil {
		return nil, err
	}
	id, err := s.tasks.Create(repository.MigrationTaskCreate{
		Status:               repository.MigrationStatusPlanned,
		SourceType:           in.SourceType,
		SourceConfig:         cfgJSON,
		CredentialRef:        in.CredentialRef,
		SourceAuthType:       authType,
		SourceAuthCiphertext: authCiphertext,
		ConflictPolicy:       policy,
		PlanJSON:             plan,
		InitiatorUsername:    strings.TrimSpace(in.Initiator.Username),
		InitiatorUserID:      migrationInitiatorUserID(in.Initiator.UserID),
		InitiatorAuthSource:  strings.TrimSpace(in.Initiator.AuthSource),
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func validateDirectSourceAuth(sourceType, credentialRef string, auth *SourceAuth) error {
	if auth == nil {
		return nil
	}
	if sourceType != repository.MigrationSourceOnlineREST || strings.TrimSpace(credentialRef) != "" {
		return fmt.Errorf("%w: sourceAuth 仅能用于不带 credentialRef 的 online_rest", ErrValidation)
	}
	if err := auth.Validate(); err != nil {
		return fmt.Errorf("%w: sourceAuth", ErrValidation)
	}
	return nil
}

func (s *MigrationService) encryptSourceAuth(auth *SourceAuth, sourceConfig string) (string, []byte, error) {
	if auth == nil {
		return "", nil, nil
	}
	if auth.Type == credential.TypeAnonymous {
		return auth.Type, nil, nil
	}
	if s.credentialSealer == nil {
		return "", nil, fmt.Errorf("%w: 迁移凭据加密未配置", ErrValidation)
	}
	ciphertext, err := s.credentialSealer.Seal(*auth, migrationCredentialAAD(repository.MigrationSourceOnlineREST, sourceConfig))
	if err != nil {
		return "", nil, fmt.Errorf("%w: 迁移凭据加密失败", ErrValidation)
	}
	return auth.Type, ciphertext, nil
}

// MigrationCredentialAAD 返回绑定任务安全来源配置的认证附加数据。
func MigrationCredentialAAD(task *repository.MigrationTask) []byte {
	if task == nil {
		return nil
	}
	return migrationCredentialAAD(task.SourceType, task.SourceConfig)
}

func migrationCredentialAAD(sourceType, sourceConfig string) []byte {
	return []byte(sourceType + ":" + sourceConfig)
}

func migrationInitiatorUserID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id > 0}
}

func filterPlanJSON(raw string, include []string) (string, error) {
	if len(include) == 0 {
		return raw, nil
	}
	var plan discover.Plan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return "", fmt.Errorf("%w: plan 解析失败", ErrValidation)
	}
	if err := filterPlanRepositories(&plan, include); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func filterPlanRepositories(plan *discover.Plan, include []string) error {
	allow := map[string]bool{}
	for _, n := range include {
		if n != "" {
			allow[n] = true
		}
	}
	filtered := make([]discover.PlanRepository, 0, len(allow))
	for _, r := range plan.Repositories {
		if allow[r.Name] {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("%w: includeRepositories 未匹配 plan 中任何仓库", ErrValidation)
	}
	plan.Repositories = filtered
	if plan.Stats == nil {
		plan.Stats = map[string]any{}
	}
	plan.Stats["repositoryCount"] = len(filtered)
	return nil
}

// Get 按 ID 取任务。
func (s *MigrationService) Get(id int64) (*repository.MigrationTask, error) {
	t, err := s.tasks.GetByID(id)
	return t, mapNotFound(err)
}

// List 分页列表。
func (s *MigrationService) List(limit, offset int) ([]repository.MigrationTask, int, error) {
	total, err := s.tasks.Count()
	if err != nil {
		return nil, 0, err
	}
	items, err := s.tasks.List(limit, offset)
	return items, total, err
}

// Start 将 planned → running 并触发 Runner（若有）。其它状态 → ErrConflict。
// include 非空时先收窄 plan 再启动（支持预览后多选仓库）。
func (s *MigrationService) Start(id int64, include []string) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusPlanned {
		return nil, fmt.Errorf("%w: 仅 planned 可 start，当前 %s", ErrConflict, t.Status)
	}
	if len(include) > 0 {
		if _, err := s.ApplyIncludeFilter(id, include); err != nil {
			return nil, err
		}
		t, err = s.Get(id)
		if err != nil {
			return nil, err
		}
	}
	if preflight, ok := s.runner.(MigrationPreflightRunner); ok {
		if err := preflight.Preflight(t); err != nil {
			return nil, err
		}
	}
	if err := s.tasks.UpdateStatus(id, repository.MigrationStatusRunning, nil, true, false); err != nil {
		return nil, mapNotFound(err)
	}
	if s.runner != nil {
		s.runner.StartAsync(id)
	}
	return s.Get(id)
}

// Cancel 取消任务：planned 直接 cancelled；running 协作取消（foundation 直接标 cancelled，execute 再接协作式）。
// completed/failed/cancelled → ErrConflict。
func (s *MigrationService) Cancel(id int64) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	switch t.Status {
	case repository.MigrationStatusPlanned:
		msg := "用户取消"
		if err := s.tasks.UpdateStatus(id, repository.MigrationStatusCancelled, &msg, false, true); err != nil {
			return nil, mapNotFound(err)
		}
		return s.Get(id)
	case repository.MigrationStatusRunning:
		if s.runner != nil {
			s.runner.Cancel(id)
		}
		// 协作取消：Runner 会写 cancelled；若无 runner 则直接标 cancelled
		if s.runner == nil {
			msg := "用户取消"
			if err := s.tasks.UpdateStatus(id, repository.MigrationStatusCancelled, &msg, false, true); err != nil {
				return nil, mapNotFound(err)
			}
		}
		// 稍等 Runner 落态；MVP 再读一次，若仍 running 则强制标 cancelled
		got, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		if got.Status == repository.MigrationStatusRunning {
			msg := "用户取消"
			_ = s.tasks.UpdateStatus(id, repository.MigrationStatusCancelled, &msg, false, true)
			return s.Get(id)
		}
		return got, nil
	default:
		return nil, fmt.Errorf("%w: 状态 %s 不可 cancel", ErrConflict, t.Status)
	}
}

// Resume 将 failed/cancelled → running 并从断点续（Runner 若有）。
func (s *MigrationService) Resume(id int64) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusFailed && t.Status != repository.MigrationStatusCancelled {
		return nil, fmt.Errorf("%w: 仅 failed/cancelled 可 resume，当前 %s", ErrConflict, t.Status)
	}
	if err := s.tasks.UpdateStatus(id, repository.MigrationStatusRunning, nil, true, false); err != nil {
		return nil, mapNotFound(err)
	}
	// 清空 finished_at 的语义：UpdateStatus 不清除 finished_at；MVP 可接受残留历史时间，report 以 status 为准。
	if s.runner != nil {
		s.runner.StartAsync(id)
	}
	return s.Get(id)
}

// FailInterruptedRunning 启动回收：running → failed。
func (s *MigrationService) FailInterruptedRunning() (int64, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return 0, err
	}
	return s.tasks.FailInterruptedRunning("进程中断，请 resume")
}

// MigrationDiscoverInput 同步发现入参。
type MigrationDiscoverInput struct {
	SourceType     string
	SourceConfig   map[string]any
	CredentialRef  string
	SourceAuth     *SourceAuth
	ConflictPolicy string
	Initiator      MigrationInitiator
}

// MigrationDiscoverResult 发现成功结果（已落库 planned）。
type MigrationDiscoverResult struct {
	Task *repository.MigrationTask
	Plan discover.Plan
}

// Discover 同步执行三来源发现；成功落库 planned 并返回 task+plan；失败不落库。
func (s *MigrationService) Discover(ctx context.Context, in MigrationDiscoverInput) (*MigrationDiscoverResult, error) {
	if err := validateSourceType(in.SourceType); err != nil {
		return nil, err
	}
	policy, err := normalizeConflictPolicy(in.ConflictPolicy)
	if err != nil {
		return nil, err
	}
	if err := validateDirectSourceAuth(in.SourceType, in.CredentialRef, in.SourceAuth); err != nil {
		return nil, err
	}
	if err := s.checkCredentialRef(in.CredentialRef, in.SourceType); err != nil {
		return nil, err
	}
	safeSourceConfig, err := discover.SanitizeSourceConfig(in.SourceType, in.SourceConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}

	cfg := discover.Config{}
	if safeSourceConfig != nil {
		if v, ok := safeSourceConfig["sourceRef"].(string); ok {
			cfg.SourceRef = v
		}
		if v, ok := safeSourceConfig["url"].(string); ok {
			cfg.URL = v
		}
		if v, ok := safeSourceConfig["path"].(string); ok {
			cfg.Path = v
		}
		cfg.RepositoryFormats = parseStringMap(safeSourceConfig["repositoryFormats"])
		cfg.RepositoryTypes = parseStringMap(safeSourceConfig["repositoryTypes"])
		cfg.RepositoryConfigs = parseMigrationRepositoryConfigs(safeSourceConfig["repositoryConfigs"])
		cfg.IncludeRepositories = parseIncludeRepos(safeSourceConfig["includeRepositories"])
		if v, ok := safeSourceConfig["allowPrivateSource"].(bool); ok {
			cfg.AllowPrivateSource = v
		}
	}
	if in.SourceType == repository.MigrationSourceOnlineREST {
		persisted, err := discover.PersistedSourceConfig(in.SourceType, safeSourceConfig)
		if err != nil {
			return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
		}
		if cfg.SourceRef != "" {
			resolved, err := discover.ResolveSourceRef(cfg.SourceRef)
			if err != nil {
				return nil, fmt.Errorf("%w: 来源引用不可用", ErrValidation)
			}
			cfg.URL = resolved
		} else {
			cfg.URL, _ = persisted["url"].(string)
		}
	}
	auth, err := resolveSourceAuth(in.CredentialRef, in.SourceAuth)
	if err != nil {
		return nil, err
	}
	cfg.SourceAuth = auth

	// 离线目录且索引就绪：直接用索引生成 plan，跳过 blob 扫描
	var plan discover.Plan
	usedIndex := false
	if in.SourceType == repository.MigrationSourceOfflineDir && s.offlineIndex != nil && cfg.Path != "" {
		p, ok, err := offindex.PlanFromIndex(s.offlineIndex, cfg.Path, cfg.IncludeRepositories, cfg.RepositoryFormats, cfg.RepositoryTypes)
		if err != nil {
			return nil, err
		}
		if ok {
			plan = p
			usedIndex = true
		}
	}
	if !usedIndex {
		src, err := s.sourceFactory(in.SourceType)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		plan, err = src.Discover(ctx, cfg)
		if err != nil {
			return nil, mapDiscoverErr(err)
		}
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}

	// 仅在发现成功后落库
	task, err := s.Create(MigrationCreateInput{
		SourceType:     in.SourceType,
		SourceConfig:   safeSourceConfig,
		CredentialRef:  in.CredentialRef,
		SourceAuth:     in.SourceAuth,
		ConflictPolicy: policy,
		PlanJSON:       string(planJSON),
		Initiator:      in.Initiator,
	})
	if err != nil {
		return nil, err
	}
	return &MigrationDiscoverResult{Task: task, Plan: plan}, nil
}

func resolveSourceAuth(credentialRef string, direct *SourceAuth) (credential.SourceAuth, error) {
	if direct != nil {
		return *direct, nil
	}
	if strings.TrimSpace(credentialRef) == "" {
		return credential.SourceAuth{Type: credential.TypeAnonymous}, nil
	}
	legacy, err := ResolveCredential(credentialRef)
	if err != nil {
		return credential.SourceAuth{}, err
	}
	return credential.FromLegacy(legacy), nil
}

func parseStringMap(raw any) map[string]string {
	values, ok := raw.(map[string]any)
	if !ok {
		if typed, ok := raw.(map[string]string); ok {
			return typed
		}
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(key) != "" && strings.TrimSpace(text) != "" {
			out[key] = text
		}
	}
	return out
}

func parseMigrationRepositoryConfigs(raw any) map[string]discover.TargetRepositoryConfig {
	values, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	configs := make(map[string]discover.TargetRepositoryConfig, len(values))
	for name, rawConfig := range values {
		fields, ok := rawConfig.(map[string]any)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		item := discover.TargetRepositoryConfig{}
		if remoteURL, ok := fields["remoteUrl"].(string); ok {
			item.RemoteURL = remoteURL
		}
		item.Members = parseIncludeRepos(fields["members"])
		configs[name] = item
	}
	return configs
}

func mapDiscoverErr(err error) error {
	var inv *discover.ErrInvalidConfig
	if errors.As(err, &inv) {
		return fmt.Errorf("%w: %s", ErrValidation, inv.Msg)
	}
	var auth *discover.ErrAuth
	if errors.As(err, &auth) {
		return fmt.Errorf("%w: %s", ErrValidation, auth.Msg)
	}
	var up *discover.ErrUpstream
	if errors.As(err, &up) {
		return fmt.Errorf("%w: %s", ErrUpstream, up.Msg)
	}
	return err
}

// Finalize 对 completed 任务做源侧增量补齐（须 Runner 实现 Finalize）。
func (s *MigrationService) Finalize(ctx context.Context, id int64) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusCompleted {
		return nil, fmt.Errorf("%w: 仅 completed 可 finalize，当前 %s", ErrConflict, t.Status)
	}
	fr, ok := s.runner.(MigrationFinalizerRunner)
	if !ok || s.runner == nil {
		return nil, fmt.Errorf("%w: finalize 执行器未配置", ErrValidation)
	}
	if err := fr.Finalize(ctx, id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Report 返回任务（report_json 由 api 层组装）。
func (s *MigrationService) Report(id int64) (*repository.MigrationTask, error) {
	return s.Get(id)
}

// UpdateSourceConfig 修改非终态（planned / failed）任务的来源配置并持久化。
// 仅允许调整 allowPrivateSource：把被私网安全策略拒绝的来源显式标记为可信内网地址。
// running/completed/cancelled 等终态拒绝修改 → ErrConflict。
func (s *MigrationService) UpdateSourceConfig(ctx context.Context, id int64, allowPrivateSource bool) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusPlanned && t.Status != repository.MigrationStatusFailed {
		return nil, fmt.Errorf("%w: 仅 planned/failed 可修改来源配置，当前 %s", ErrConflict, t.Status)
	}
	cfg := map[string]any{}
	if t.SourceConfig != "" && t.SourceConfig != "{}" {
		var raw map[string]any
		if err := json.Unmarshal([]byte(t.SourceConfig), &raw); err == nil && raw != nil {
			cfg = raw
		}
	}
	cfg["allowPrivateSource"] = allowPrivateSource
	encoded, err := encodeSourceConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	if err := s.tasks.SaveSourceConfig(id, encoded); err != nil {
		return nil, mapNotFound(err)
	}
	return s.Get(id)
}

// ApplyIncludeFilter 在 start 前可选收窄 plan：仅保留 include 中的仓库并写回 plan_json。
// include 为空则不改动。
func (s *MigrationService) ApplyIncludeFilter(id int64, include []string) (*repository.MigrationTask, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	if len(include) == 0 {
		return s.Get(id)
	}
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusPlanned {
		return nil, fmt.Errorf("%w: 仅 planned 可收窄 plan，当前 %s", ErrConflict, t.Status)
	}
	var plan discover.Plan
	if t.PlanJSON != "" && t.PlanJSON != "{}" {
		if err := json.Unmarshal([]byte(t.PlanJSON), &plan); err != nil {
			return nil, fmt.Errorf("%w: plan 解析失败", ErrValidation)
		}
	}
	if err := filterPlanRepositories(&plan, include); err != nil {
		return nil, err
	}
	b, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if err := s.tasks.SavePlan(id, string(b)); err != nil {
		return nil, mapNotFound(err)
	}
	return s.Get(id)
}

func parseIncludeRepos(raw any) []string {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ListRemoteRepositories 保留既有 sourceRef 调用方式。
func (s *MigrationService) ListRemoteRepositories(ctx context.Context, sourceRef, credentialRef string) ([]discover.RemoteRepository, error) {
	return s.ListRemoteRepositoriesWithSource(ctx, map[string]any{"sourceRef": sourceRef}, credentialRef, nil)
}

// ListRemoteRepositoriesWithSource 从在线 Nexus 仅拉仓库索引，不落库、不扫资产。
// 直接认证仅在本次出站请求中使用，绝不写入迁移任务。
func (s *MigrationService) ListRemoteRepositoriesWithSource(ctx context.Context, sourceConfig map[string]any, credentialRef string, sourceAuth *SourceAuth) ([]discover.RemoteRepository, error) {
	if err := validateDirectSourceAuth(repository.MigrationSourceOnlineREST, credentialRef, sourceAuth); err != nil {
		return nil, err
	}
	safeConfig, err := discover.SanitizeSourceConfig(repository.MigrationSourceOnlineREST, sourceConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	persisted, err := discover.PersistedSourceConfig(repository.MigrationSourceOnlineREST, safeConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	url, err := resolveOnlineSourceURL(persisted)
	if err != nil {
		return nil, err
	}
	auth, err := resolveSourceAuth(credentialRef, sourceAuth)
	if err != nil {
		return nil, err
	}
	// allowPrivateSource 仅当用户在来源中显式声明可信内网站时放行，默认保持 SSRF 防护。
	allowPrivate, _ := persisted["allowPrivateSource"].(bool)
	src, err := s.sourceFactory(repository.MigrationSourceOnlineREST)
	if err != nil {
		return nil, err
	}
	lister, ok := src.(interface {
		ListRemoteRepositoriesWithAuth(context.Context, string, credential.SourceAuth, bool, bool) ([]discover.RemoteRepository, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: 在线来源不支持仓库索引", ErrValidation)
	}
	items, err := lister.ListRemoteRepositoriesWithAuth(ctx, url, auth, true, allowPrivate)
	if err != nil {
		return nil, mapDiscoverErr(err)
	}
	return items, nil
}

func resolveOnlineSourceURL(sourceConfig map[string]any) (string, error) {
	if directURL, ok := sourceConfig["url"].(string); ok {
		return directURL, nil
	}
	ref, _ := sourceConfig["sourceRef"].(string)
	url, err := discover.ResolveSourceRef(ref)
	if err != nil {
		return "", fmt.Errorf("%w: 来源引用不可用", ErrValidation)
	}
	return url, nil
}

// ResolveCredential 从专用上游凭据命名空间读取逻辑引用；缺失返回 ErrValidation。
// 返回值不得写入日志或报告。
func ResolveCredential(ref string) (string, error) {
	v, err := upstream.ResolveCredential(ref)
	if err != nil {
		return "", fmt.Errorf("%w: 凭据引用不可用", ErrValidation)
	}
	return v, nil
}

func (s *MigrationService) checkCredentialRef(ref, sourceType string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		// 在线 REST 建议有凭据，但允许空（匿名可读的 Nexus）；执行期再处理 401。
		return nil
	}
	// 校验专用命名空间中的引用是否存在（值非空）；不缓存明文。
	_, err := ResolveCredential(ref)
	return err
}

func validateSourceType(t string) error {
	switch t {
	case repository.MigrationSourceOnlineREST,
		repository.MigrationSourceOfflineDir,
		repository.MigrationSourceOfflineBundle:
		return nil
	default:
		return fmt.Errorf("%w: 不支持的 sourceType %q", ErrValidation, t)
	}
}

func normalizeConflictPolicy(p string) (string, error) {
	if p == "" {
		return repository.MigrationConflictSkip, nil
	}
	switch p {
	case repository.MigrationConflictSkip,
		repository.MigrationConflictOverwrite,
		repository.MigrationConflictFail:
		return p, nil
	default:
		return "", fmt.Errorf("%w: 不支持的 conflictPolicy %q", ErrValidation, p)
	}
}

func encodeSourceConfig(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
