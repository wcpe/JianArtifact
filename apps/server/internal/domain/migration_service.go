package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// MigrationService 迁移任务状态机：创建 planned、discover 落库、start/cancel/resume 守卫。
// 异步执行由 Runner 钩子接入（execute 增量）。
type MigrationService struct {
	tasks  *repository.MigrationTaskRepo
	runner MigrationRunner // 可选；nil 时 Start/Resume 仅更新状态
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

// NewMigrationService 构造 MigrationService。
func NewMigrationService(tasks *repository.MigrationTaskRepo, runner MigrationRunner) *MigrationService {
	return &MigrationService{tasks: tasks, runner: runner}
}

// MigrationCreateInput 创建 planned 任务的入参。
type MigrationCreateInput struct {
	SourceType     string
	SourceConfig   map[string]any // 序列化为 JSON；无密钥
	CredentialRef  string         // 可空；在线源建议填写
	ConflictPolicy string         // skip|overwrite|fail，默认 skip
	PlanJSON       string         // 可空，默认 "{}"
}

// Create 创建状态为 planned 的任务；不自动 running。
// 未知 credentialRef（环境变量不存在）→ ErrValidation。
func (s *MigrationService) Create(in MigrationCreateInput) (*repository.MigrationTask, error) {
	if err := validateSourceType(in.SourceType); err != nil {
		return nil, err
	}
	policy, err := normalizeConflictPolicy(in.ConflictPolicy)
	if err != nil {
		return nil, err
	}
	if err := s.checkCredentialRef(in.CredentialRef, in.SourceType); err != nil {
		return nil, err
	}
	cfgJSON, err := encodeSourceConfig(in.SourceConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: sourceConfig", ErrValidation)
	}
	plan := in.PlanJSON
	if plan == "" {
		plan = "{}"
	}
	if !json.Valid([]byte(plan)) {
		return nil, fmt.Errorf("%w: plan 须为合法 JSON", ErrValidation)
	}
	id, err := s.tasks.Create(repository.MigrationTaskCreate{
		Status:         repository.MigrationStatusPlanned,
		SourceType:     in.SourceType,
		SourceConfig:   cfgJSON,
		CredentialRef:  in.CredentialRef,
		ConflictPolicy: policy,
		PlanJSON:       plan,
	})
	if err != nil {
		return nil, err
	}
	return s.Get(id)
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
func (s *MigrationService) Start(id int64) (*repository.MigrationTask, error) {
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Status != repository.MigrationStatusPlanned {
		return nil, fmt.Errorf("%w: 仅 planned 可 start，当前 %s", ErrConflict, t.Status)
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
	return s.tasks.FailInterruptedRunning("进程中断，请 resume")
}

// MigrationDiscoverInput 同步发现入参。
type MigrationDiscoverInput struct {
	SourceType     string
	SourceConfig   map[string]any
	CredentialRef  string
	ConflictPolicy string
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
	if err := s.checkCredentialRef(in.CredentialRef, in.SourceType); err != nil {
		return nil, err
	}

	src, err := discover.NewSource(in.SourceType)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	cfg := discover.Config{}
	if in.SourceConfig != nil {
		if v, ok := in.SourceConfig["url"].(string); ok {
			cfg.URL = v
		}
		if v, ok := in.SourceConfig["path"].(string); ok {
			cfg.Path = v
		}
	}
	if strings.TrimSpace(in.CredentialRef) != "" {
		cred, err := ResolveCredential(in.CredentialRef)
		if err != nil {
			return nil, err
		}
		cfg.Credential = cred
	}

	plan, err := src.Discover(ctx, cfg)
	if err != nil {
		return nil, mapDiscoverErr(err)
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}

	// 仅在发现成功后落库
	task, err := s.Create(MigrationCreateInput{
		SourceType:     in.SourceType,
		SourceConfig:   in.SourceConfig,
		CredentialRef:  in.CredentialRef,
		ConflictPolicy: policy,
		PlanJSON:       string(planJSON),
	})
	if err != nil {
		return nil, err
	}
	return &MigrationDiscoverResult{Task: task, Plan: plan}, nil
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

// ResolveCredential 读取 credentialRef 对应环境变量；缺失返回 ErrValidation。
// 返回值不得写入日志或报告。
func ResolveCredential(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("%w: credentialRef 为空", ErrValidation)
	}
	v, ok := os.LookupEnv(ref)
	if !ok || v == "" {
		return "", fmt.Errorf("%w: 凭据引用 %s 未配置或为空", ErrValidation, ref)
	}
	return v, nil
}

func (s *MigrationService) checkCredentialRef(ref, sourceType string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		// 在线 REST 建议有凭据，但允许空（匿名可读的 Nexus）；执行期再处理 401。
		return nil
	}
	// 校验引用名是否存在（值非空）；不缓存明文。
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
