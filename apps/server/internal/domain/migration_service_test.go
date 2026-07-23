package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestMigrationServiceCreatePlannedAndStart(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)

	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:     repository.MigrationSourceOfflineDir,
		SourceConfig:   map[string]any{"path": "/data/nexus"},
		ConflictPolicy: repository.MigrationConflictSkip,
		PlanJSON:       `{"repositories":[]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if task.Status != repository.MigrationStatusPlanned {
		t.Fatalf("创建后 status = %q，期望 planned", task.Status)
	}

	started, err := svc.Start(task.ID)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}
	if started.Status != repository.MigrationStatusRunning {
		t.Fatalf("start 后 status = %q", started.Status)
	}

	// 再次 start → 409 语义 ErrConflict
	if _, err := svc.Start(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("二次 start err = %v，期望 ErrConflict", err)
	}
}

func TestMigrationServiceCredentialRef(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)

	// 未知引用
	_, err := svc.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"url": "http://n"},
		CredentialRef: "JIAN_TEST_MISSING_CRED_XYZ",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("未知 credentialRef err = %v，期望 ErrValidation", err)
	}

	t.Setenv("JIAN_TEST_NEXUS_CRED", "user:pass")
	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:    repository.MigrationSourceOnlineREST,
		SourceConfig:  map[string]any{"url": "http://n"},
		CredentialRef: "JIAN_TEST_NEXUS_CRED",
	})
	if err != nil {
		t.Fatalf("Create with ref：%v", err)
	}
	if !task.CredentialRef.Valid || task.CredentialRef.String != "JIAN_TEST_NEXUS_CRED" {
		t.Errorf("credential_ref = %+v", task.CredentialRef)
	}
	// 库中不应出现明文
	if task.SourceConfig == "user:pass" || task.PlanJSON == "user:pass" {
		t.Fatal("明文密钥出现在持久化字段")
	}
}

func TestMigrationServiceCancelAndResume(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	task, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": "/b"},
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	cancelled, err := svc.Cancel(task.ID)
	if err != nil {
		t.Fatalf("Cancel planned：%v", err)
	}
	if cancelled.Status != repository.MigrationStatusCancelled {
		t.Fatalf("status = %q", cancelled.Status)
	}
	resumed, err := svc.Resume(task.ID)
	if err != nil {
		t.Fatalf("Resume：%v", err)
	}
	if resumed.Status != repository.MigrationStatusRunning {
		t.Fatalf("resume 后 status = %q", resumed.Status)
	}
}

func TestMigrationServiceIllegalTransitions(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)
	task, _ := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{"path": "/x"},
	})
	// 直接标 completed 后 resume/start 应冲突
	msg := "done"
	if err := repo.UpdateStatus(task.ID, repository.MigrationStatusCompleted, &msg, true, true); err != nil {
		t.Fatalf("UpdateStatus：%v", err)
	}
	if _, err := svc.Start(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed start：%v", err)
	}
	if _, err := svc.Resume(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed resume：%v", err)
	}
	if _, err := svc.Cancel(task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("completed cancel：%v", err)
	}
}

func TestMigrationServiceFailInterrupted(t *testing.T) {
	db := newTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	svc := domain.NewMigrationService(repo, nil)
	task, _ := svc.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{"path": "/x"},
	})
	if _, err := svc.Start(task.ID); err != nil {
		t.Fatalf("Start：%v", err)
	}
	n, err := svc.FailInterruptedRunning()
	if err != nil || n != 1 {
		t.Fatalf("FailInterrupted n=%d err=%v", n, err)
	}
	got, _ := svc.Get(task.ID)
	if got.Status != repository.MigrationStatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestMigrationServiceInvalidSource(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	_, err := svc.Create(domain.MigrationCreateInput{
		SourceType:   "docker",
		SourceConfig: map[string]any{},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v", err)
	}
}
