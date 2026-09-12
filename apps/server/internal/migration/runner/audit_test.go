package runner_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestRunnerFinalResultAuditUsesPersistedInitiatorAndReplicatesFormatMetadata(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "migration-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	taskRepo := repository.NewMigrationTaskRepo(db)
	auditRepo := repository.NewAuditLogRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	settings := repository.NewSettingRepo(db)
	nodeIdentity := domain.NewNodeIdentity(settings)
	metadataRepo := repository.NewFormatMetadataRepo(db)
	repoSvc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assetRepo, domain.NewSettingService(settings), repository.NewUserRepo(db))
	repoSvc.SetEnabledFormats(formats.New(formats.Known...))
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	repoSvc.SetMutationCoordinator(assetSvc.MutationCoordinator())
	metadataSvc := domain.NewFormatMetadataService(assetSvc, repoRepo, metadataRepo, nil)
	assetSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	repoSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	metadataSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	assetSvc.SetNodeIdentity(nodeIdentity)
	r := runner.New(
		runner.TaskStoreAdapter{Repo: taskRepo},
		runner.AssetServiceAdapter{Assets: assetSvc, Repos: repoRepo, AssetR: assetRepo},
		runner.RepoAdminAdapter{Repos: repoSvc},
	)
	r.SetFormatImporter(domain.NewMigrationFormatImporter(metadataSvc, domain.NewCargoService(assetSvc, repoSvc), domain.NewOCIService(assetSvc, repoSvc)))
	r.SetAuditLogRepo(auditRepo)
	migrations := domain.NewMigrationService(taskRepo, r)
	initiator := domain.MigrationInitiator{Username: "migration-admin", UserID: 42, AuthSource: "web_jwt"}

	completedRoot := writeFormatBundle(t, "migration-audit-pypi", "pypi", "pypi/packages/demo/demo-1.0.0-py3-none-any.whl", []byte("wheel"))
	completed, err := migrations.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": completedRoot},
		Initiator:    initiator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Start(completed.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, migrations, completed.Task.ID, repository.MigrationStatusCompleted)
	assertMigrationResultAudit(t, auditRepo, completed.Task.ID, "completed", false, initiator)
	assertMigrationReplication(t, repository.NewReplicationOperationRepo(db))

	failedRoot := writeFormatBundle(t, "migration-audit-failed", "pypi", "pypi/packages/demo/not-a-distribution.bin", []byte("invalid"))
	failed, err := migrations.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": failedRoot},
		Initiator:    initiator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Start(failed.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, migrations, failed.Task.ID, repository.MigrationStatusFailed)
	assertMigrationResultAudit(t, auditRepo, failed.Task.ID, "failed", true, initiator)
}

func assertMigrationResultAudit(t *testing.T, auditRepo *repository.AuditLogRepo, taskID int64, result string, rollback bool, want domain.MigrationInitiator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := auditRepo.List(repository.AuditFilter{Action: "migration.result", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.EntityKey != strconv.FormatInt(taskID, 10) {
				continue
			}
			if entry.Actor != want.Username || entry.UserID == nil || *entry.UserID != want.UserID || entry.AuthSource != want.AuthSource {
				t.Fatalf("审计发起人 = %+v", entry)
			}
			if entry.Result != result || entry.Detail != "taskId="+strconv.FormatInt(taskID, 10)+" result="+result+" rollback="+boolText(rollback) {
				t.Fatalf("审计结果 = %+v", entry)
			}
			if strings.Contains(entry.Detail, "source") || strings.Contains(entry.Detail, "secret") || strings.Contains(entry.Detail, "credential") || strings.Contains(entry.Detail, "token") {
				t.Fatalf("审计详情泄露来源或凭据：%q", entry.Detail)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("缺少迁移结果审计 task=%d result=%s", taskID, result)
}

func assertMigrationReplication(t *testing.T, operations *repository.ReplicationOperationRepo) {
	t.Helper()
	rows, err := operations.ListRecordsSince(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 复制退役后迁移写入只产生 v2 operation 信封（v1 change 不再写），
	// 断言存在含资产条目的 operation 信封（目标仓库由 EnsureMigrationRepository 在信封外建）。
	foundOperation := false
	for _, row := range rows {
		if row.Operation != nil && len(row.Operation.Items) > 0 {
			foundOperation = true
		}
	}
	if !foundOperation {
		t.Fatalf("迁移缺少原子 operation 信封：全部=%+v", rows)
	}
	records, err := operations.ListRecordsSince(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Operation == nil {
			continue
		}
		var asset domain.AssetChangeData
		var metadata domain.FormatMetadataChangeData
		foundAsset, foundMetadata := false, false
		for _, item := range record.Operation.Items {
			switch item.Type {
			case domain.EntityAsset:
				if item.Op != domain.OpPut {
					continue
				}
				if err := json.Unmarshal([]byte(item.Data), &asset); err != nil {
					t.Fatalf("解析迁移资产 operation 项：%v", err)
				}
				foundAsset = true
			case domain.EntityFormatMetadata:
				if item.Op != domain.OpPut {
					continue
				}
				if err := json.Unmarshal([]byte(item.Data), &metadata); err != nil {
					t.Fatalf("解析迁移格式元数据 operation 项：%v", err)
				}
				foundMetadata = true
			}
		}
		if !foundAsset || !foundMetadata {
			continue
		}
		if metadata.RepoName != "migration-audit-pypi" || metadata.Format != "pypi" || metadata.AssetPath != asset.Path || metadata.Sha256 != asset.BlobHash {
			t.Fatalf("迁移 PyPI operation 资产与元数据不一致：operation=%+v asset=%+v metadata=%+v", record.Operation, asset, metadata)
		}
		return
	}
	t.Fatalf("迁移缺少包含资产与格式元数据的原子 operation：records=%+v", records)
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
