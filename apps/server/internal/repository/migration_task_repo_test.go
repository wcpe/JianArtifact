package repository_test

import (
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func openMigrationTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "mig.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

func TestMigrationTaskRepoCRUD(t *testing.T) {
	db := openMigrationTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)

	id, err := repo.Create(repository.MigrationTaskCreate{
		Status:         repository.MigrationStatusPlanned,
		SourceType:     repository.MigrationSourceOfflineBundle,
		SourceConfig:   `{"path":"/data/bundle"}`,
		CredentialRef:  "",
		ConflictPolicy: repository.MigrationConflictSkip,
		PlanJSON:       `{"repositories":[]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d", id)
	}

	got, err := repo.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID：%v", err)
	}
	if got.Status != repository.MigrationStatusPlanned {
		t.Errorf("status = %q", got.Status)
	}
	if got.SourceType != repository.MigrationSourceOfflineBundle {
		t.Errorf("sourceType = %q", got.SourceType)
	}
	if got.CredentialRef.Valid {
		t.Errorf("credential_ref 应为空，却有值 %q", got.CredentialRef.String)
	}
	// 确保无密钥列：表结构仅有 credential_ref 文本引用。
	if got.ConflictPolicy != repository.MigrationConflictSkip {
		t.Errorf("conflict = %q", got.ConflictPolicy)
	}

	if err := repo.UpdateStatus(id, repository.MigrationStatusRunning, nil, true, false); err != nil {
		t.Fatalf("UpdateStatus running：%v", err)
	}
	if err := repo.SaveCheckpoint(id, `{"cursor":"a"}`); err != nil {
		t.Fatalf("SaveCheckpoint：%v", err)
	}
	got, _ = repo.GetByID(id)
	if got.CheckpointJSON != `{"cursor":"a"}` {
		t.Errorf("checkpoint = %s", got.CheckpointJSON)
	}
	if !got.StartedAt.Valid {
		t.Error("期望 started_at 已设置")
	}

	n, err := repo.FailInterruptedRunning("进程中断，请 resume")
	if err != nil {
		t.Fatalf("FailInterruptedRunning：%v", err)
	}
	if n != 1 {
		t.Errorf("affected = %d，期望 1", n)
	}
	got, _ = repo.GetByID(id)
	if got.Status != repository.MigrationStatusFailed {
		t.Errorf("崩溃回收后 status = %q", got.Status)
	}
	if !got.ErrorMessage.Valid || got.ErrorMessage.String == "" {
		t.Error("期望 error_message 非空")
	}

	// 无明文密钥字段：Create 带 credential_ref 仅存引用名。
	id2, err := repo.Create(repository.MigrationTaskCreate{
		Status:         repository.MigrationStatusPlanned,
		SourceType:     repository.MigrationSourceOnlineREST,
		SourceConfig:   `{"url":"http://nexus.example"}`,
		CredentialRef:  "NEXUS_BASIC",
		ConflictPolicy: repository.MigrationConflictOverwrite,
		PlanJSON:       `{}`,
	})
	if err != nil {
		t.Fatalf("Create online：%v", err)
	}
	got2, err := repo.GetByID(id2)
	if err != nil {
		t.Fatalf("GetByID online：%v", err)
	}
	if !got2.CredentialRef.Valid || got2.CredentialRef.String != "NEXUS_BASIC" {
		t.Errorf("credential_ref = %+v", got2.CredentialRef)
	}

	total, err := repo.Count()
	if err != nil || total != 2 {
		t.Fatalf("Count = %d, err=%v", total, err)
	}
	list, err := repo.List(10, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("List len=%d err=%v", len(list), err)
	}
}

func TestMigrationTaskGetNotFound(t *testing.T) {
	db := openMigrationTestDB(t)
	repo := repository.NewMigrationTaskRepo(db)
	_, err := repo.GetByID(99999)
	if err != repository.ErrNotFound {
		t.Fatalf("err = %v，期望 ErrNotFound", err)
	}
}
