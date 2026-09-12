package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestAssetOperationPublishesV2OutboxWithoutItemChanges(t *testing.T) {
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, nil)
	svc.SetChangeRecorder(domain.NoopChangeRecorder{})
	svc.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))

	if _, err := repos.Create("raw-v2-ops", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, p := range []string{"dir/a.txt", "dir/b.txt"} {
		if _, err := svc.Put("raw-v2-ops", p, bytes.NewBufferString(p), "text/plain"); err != nil {
			t.Fatalf("发布 %s：%v", p, err)
		}
	}
	// setup 发布现在也写 outbox（信封语义），水位取 setup 之后的实际值。
	var before int64
	if err := db.Get(&before, `SELECT COALESCE(MAX(seq),0) FROM repl_change`); err != nil {
		t.Fatalf("读取 setup 后水位：%v", err)
	}

	result, err := svc.ApplyOperation("raw-v2-ops", domain.AssetOperation{
		Action:          domain.AssetOperationMove,
		Targets:         []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "dir"}},
		DestinationPath: "moved",
	})
	if err != nil {
		t.Fatalf("目录移动：%v", err)
	}
	records, err := repository.NewReplicationOperationRepo(db).ListRecordsSince(before, 10)
	if err != nil {
		t.Fatalf("读取 v2 记录：%v", err)
	}
	if len(records) != 1 || records[0].Type != "operation" || records[0].Operation == nil {
		t.Fatalf("操作应只发布一条 v2 envelope，得 %+v", records)
	}
	envelope := records[0].Operation
	if envelope.OperationID != result.OperationID || envelope.SourceNode == "" || len(envelope.Items) != 4 {
		t.Fatalf("v2 envelope 字段异常：%+v", envelope)
	}
	for i := 1; i < len(envelope.Items); i++ {
		if envelope.Items[i-1].Key >= envelope.Items[i].Key {
			t.Fatalf("operation items 未按实体键排序：%+v", envelope.Items)
		}
	}
	records, err = repository.NewReplicationOperationRepo(db).ListRecordsSince(before, 10)
	if err != nil {
		t.Fatalf("读取 operation outbox：%v", err)
	}
	for _, record := range records {
		if record.Change != nil {
			t.Fatalf("统一操作不得拆成 v1 item change，得 %+v", record)
		}
	}
}

// TestAssetOperationAuditFailureRollsBackViewAndOutbox 确保源端审计不能在
// operation/outbox 之后单独失败，任一审计写入失败都必须使整批回滚。
func TestAssetOperationAuditFailureRollsBackViewAndOutbox(t *testing.T) {
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, nil)
	svc.SetChangeRecorder(domain.NoopChangeRecorder{})
	svc.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))
	repoID, err := repos.Create("audit-operation", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("audit-operation", "keep.txt", bytes.NewBufferString("keep"), "text/plain"); err != nil {
		t.Fatalf("发布制品：%v", err)
	}
	var beforeSeq int64
	if err := db.Get(&beforeSeq, `SELECT COALESCE(MAX(seq),0) FROM repl_change`); err != nil {
		t.Fatalf("读取 setup 后水位：%v", err)
	}
	auditCalled := false
	_, err = svc.ApplyOperation("audit-operation", domain.AssetOperation{
		Action:  domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "keep.txt"}},
		Audit: domain.AssetOperationAudit{Commit: func(string, []repository.AssetMutationItem) repository.MutationCompletionHook {
			return func(*sqlx.Tx) error {
				auditCalled = true
				return errors.New("注入源端审计写入失败")
			}
		}},
	})
	if err == nil || !auditCalled {
		t.Fatalf("源端审计失败必须中止操作：err=%v called=%v", err, auditCalled)
	}
	if _, err := assets.GetByPath(repoID, "keep.txt"); err != nil {
		t.Fatalf("审计失败不得删除资产：%v", err)
	}
	records, err := repository.NewReplicationOperationRepo(db).ListRecordsSince(beforeSeq, 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("审计失败不得写 operation outbox：records=%+v err=%v", records, err)
	}
}

func TestAssetOperationRawDeleteMoveAndAtomicFailure(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-ops", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, p := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		if _, err := svc.Put("raw-ops", p, bytes.NewBufferString(p), "text/plain"); err != nil {
			t.Fatalf("发布 %s：%v", p, err)
		}
	}
	if _, err := svc.ApplyOperation("raw-ops", domain.AssetOperation{
		Action: domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "dir"},
			{Type: domain.AssetTargetRawPath, Path: "missing"}},
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("混合存在与不存在目标应整批失败，得 %v", err)
	}
	if _, rc, err := svc.Get("raw-ops", "dir/a.txt"); err != nil {
		t.Fatalf("整批失败不应删除已有文件：%v", err)
	} else {
		_ = rc.Close()
	}
	result, err := svc.ApplyOperation("raw-ops", domain.AssetOperation{
		Action:          domain.AssetOperationMove,
		Targets:         []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "dir"}},
		DestinationPath: "moved",
	})
	if err != nil || result.OperationID == "" || result.Affected != 2 {
		t.Fatalf("目录移动结果异常：result=%+v err=%v", result, err)
	}
	if _, rc, err := svc.Get("raw-ops", "moved/sub/b.txt"); err != nil {
		t.Fatalf("移动后文件不可读：%v", err)
	} else {
		_ = rc.Close()
	}
	for _, oldPath := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		if _, rc, err := svc.Get("raw-ops", oldPath); !errors.Is(err, domain.ErrNotFound) {
			if rc != nil {
				_ = rc.Close()
			}
			t.Fatalf("目录移动后旧路径 %s 应不存在，得 %v", oldPath, err)
		}
	}
	result, err = svc.ApplyOperation("raw-ops", domain.AssetOperation{
		Action:  domain.AssetOperationRename,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "moved/a.txt"}},
		NewPath: "moved/a-renamed.txt",
	})
	if err != nil || result.Affected != 1 {
		t.Fatalf("单项重命名结果异常：result=%+v err=%v", result, err)
	}
	if _, rc, err := svc.Get("raw-ops", "moved/a.txt"); !errors.Is(err, domain.ErrNotFound) {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatalf("重命名后旧路径应不存在，得 %v", err)
	}
	if _, rc, err := svc.Get("raw-ops", "moved/a-renamed.txt"); err != nil {
		t.Fatalf("重命名后新路径不可读：%v", err)
	} else {
		_ = rc.Close()
	}
	for p, body := range map[string]string{
		"moved/conflict-source.txt": "source",
		"moved/conflict-target.txt": "target",
	} {
		if _, err := svc.Put("raw-ops", p, bytes.NewBufferString(body), "text/plain"); err != nil {
			t.Fatalf("准备重命名冲突文件 %s：%v", p, err)
		}
	}
	if _, err := svc.ApplyOperation("raw-ops", domain.AssetOperation{
		Action:  domain.AssetOperationRename,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "moved/conflict-source.txt"}},
		NewPath: "moved/conflict-target.txt",
	}); err == nil {
		t.Fatal("重命名到已存在目标必须拒绝")
	}
	for p, want := range map[string]string{
		"moved/conflict-source.txt": "source",
		"moved/conflict-target.txt": "target",
	} {
		_, rc, err := svc.Get("raw-ops", p)
		if err != nil {
			t.Fatalf("冲突失败后路径 %s 不可读：%v", p, err)
		}
		data, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil || string(data) != want {
			t.Fatalf("冲突失败后路径 %s 内容改变：%q，错误：%v", p, data, readErr)
		}
	}
}

// TestAssetOperationRejectsOverlappingDirectoryMove 确保重叠目录不能导致同一
// 制品在同一批次被重复迁移，失败时不暴露任何部分结果。
func TestAssetOperationRejectsOverlappingDirectoryMove(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("overlap-move", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, p := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		if _, err := svc.Put("overlap-move", p, bytes.NewBufferString(p), "text/plain"); err != nil {
			t.Fatalf("发布 %s：%v", p, err)
		}
	}
	_, err := svc.ApplyOperation("overlap-move", domain.AssetOperation{
		Action: domain.AssetOperationMove, DestinationPath: "moved",
		Targets: []domain.AssetOperationTarget{
			{Type: domain.AssetTargetRawPath, Path: "dir"},
			{Type: domain.AssetTargetRawPath, Path: "dir/sub"},
		},
	})
	if !errors.Is(err, domain.ErrValidation) && !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("重叠目录移动必须拒绝：%v", err)
	}
	for _, p := range []string{"dir/a.txt", "dir/sub/b.txt"} {
		_, rc, err := svc.Get("overlap-move", p)
		if err != nil {
			t.Fatalf("失败后原路径必须保留 %s：%v", p, err)
		}
		_ = rc.Close()
	}
	if _, _, err := svc.Get("overlap-move", "moved/a.txt"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("失败后不得留下目标路径：%v", err)
	}
}

func TestAssetOperationRejectsMoveIntoOwnSubtreeWithoutMutation(t *testing.T) {
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, nil)
	svc.SetChangeRecorder(domain.NoopChangeRecorder{})
	svc.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))
	repoID, err := repos.Create("self-subtree-move", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("self-subtree-move", "dir/a.txt", bytes.NewBufferString("keep"), "text/plain"); err != nil {
		t.Fatalf("发布制品：%v", err)
	}
	var beforeSeq int64
	if err := db.Get(&beforeSeq, `SELECT COALESCE(MAX(seq),0) FROM repl_change`); err != nil {
		t.Fatalf("读取 setup 后水位：%v", err)
	}
	auditCalled := false
	if _, err := svc.ApplyOperation("self-subtree-move", domain.AssetOperation{
		Action: domain.AssetOperationMove, DestinationPath: "dir/nested",
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetRawPath, Path: "dir"}},
		Audit: domain.AssetOperationAudit{Commit: func(string, []repository.AssetMutationItem) repository.MutationCompletionHook {
			auditCalled = true
			return nil
		}},
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("移动到自身子树必须冲突：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "dir/a.txt"); err != nil {
		t.Fatalf("拒绝后原路径必须保留：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "dir/nested/a.txt"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("拒绝后不得写入子树目标：%v", err)
	}
	records, err := repository.NewReplicationOperationRepo(db).ListRecordsSince(beforeSeq, 10)
	if err != nil {
		t.Fatalf("读取拒绝后的 outbox：%v", err)
	}
	if len(records) != 0 {
		t.Fatalf("拒绝后不得写入 outbox：%+v", records)
	}
	if auditCalled {
		t.Fatal("拒绝后不得提交成功审计")
	}
}

func TestAssetOperationMavenVersionRebuildsMetadata(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("maven-ops", "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	metadata := `<metadata><groupId>com.example</groupId><artifactId>demo</artifactId><versioning><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>`
	for p, body := range map[string]string{
		"com/example/demo/1.0/demo-1.0.jar":   "old",
		"com/example/demo/2.0/demo-2.0.jar":   "new",
		"com/example/demo/maven-metadata.xml": metadata,
	} {
		if _, err := svc.Put("maven-ops", p, bytes.NewBufferString(body), "application/octet-stream"); err != nil {
			t.Fatalf("发布 %s：%v", p, err)
		}
	}
	result, err := svc.ApplyOperation("maven-ops", domain.AssetOperation{
		Action:  domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetMavenVersion, Path: "com/example/demo/1.0"}},
	})
	if err != nil || result.Affected != 2 {
		t.Fatalf("Maven 版本删除结果异常：result=%+v err=%v", result, err)
	}
	asset, rc, err := svc.Get("maven-ops", "com/example/demo/maven-metadata.xml")
	if err != nil {
		t.Fatalf("metadata 应保留：%v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if bytes.Contains(data, []byte("<version>1.0</version>")) || !bytes.Contains(data, []byte("<version>2.0</version>")) {
		t.Fatalf("metadata 未正确移除版本：%s", data)
	}
	if asset.Size != int64(len(data)) {
		t.Fatalf("metadata size 不一致：%d != %d", asset.Size, len(data))
	}
}

func TestAssetOperationMavenRejectsNonLogicalTargetsAndDeletesCompleteArtifact(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("maven-logical", "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	metadata := `<metadata><versioning><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>`
	for p, body := range map[string]string{
		"com/example/demo/1.0/demo-1.0.jar":          "old",
		"com/example/demo/1.0/demo-1.0.pom":          "pom",
		"com/example/demo/1.0/demo-1.0.jar.sha256":   "checksum",
		"com/example/demo/2.0/demo-2.0.jar":          "new",
		"com/example/demo/maven-metadata.xml":        metadata,
		"com/example/demo/maven-metadata.xml.sha256": "metadata-checksum",
		"com/example/other/1.0/other-1.0.jar":        "other",
		"com/example/other/maven-metadata.xml":       `<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>`,
	} {
		if _, err := svc.Put("maven-logical", p, bytes.NewBufferString(body), "application/octet-stream"); err != nil {
			t.Fatalf("发布 %s：%v", p, err)
		}
	}

	for _, target := range []domain.AssetOperationTarget{
		{Type: domain.AssetTargetMavenVersion, Path: "com/example/demo/1.0/demo-1.0.pom"},
		{Type: domain.AssetTargetMavenVersion, Path: "com/example/demo/1.0/demo-1.0.jar.sha256"},
		{Type: domain.AssetTargetMavenArtifact, Path: "com/example/demo/maven-metadata.xml"},
		{Type: domain.AssetTargetMavenVersion, Path: "com/example/demo/1.0/nested"},
	} {
		if _, err := svc.ApplyOperation("maven-logical", domain.AssetOperation{
			Action: domain.AssetOperationDelete, Targets: []domain.AssetOperationTarget{target},
		}); !errors.Is(err, domain.ErrLogicalDeleteRequired) {
			t.Fatalf("非法 Maven 目标 %+v 必须要求逻辑删除，得 %v", target, err)
		}
	}
	if _, rc, err := svc.Get("maven-logical", "com/example/demo/1.0/demo-1.0.jar"); err != nil {
		t.Fatalf("非法目标不得删除版本文件：%v", err)
	} else {
		_ = rc.Close()
	}

	result, err := svc.ApplyOperation("maven-logical", domain.AssetOperation{
		Action:  domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetMavenArtifact, Path: "com/example/demo"}},
	})
	if err != nil || result.Affected != 6 {
		t.Fatalf("完整 artifact 删除结果异常：result=%+v err=%v", result, err)
	}
	for _, p := range []string{
		"com/example/demo/1.0/demo-1.0.jar",
		"com/example/demo/1.0/demo-1.0.pom",
		"com/example/demo/1.0/demo-1.0.jar.sha256",
		"com/example/demo/2.0/demo-2.0.jar",
		"com/example/demo/maven-metadata.xml",
		"com/example/demo/maven-metadata.xml.sha256",
	} {
		if _, rc, err := svc.Get("maven-logical", p); !errors.Is(err, domain.ErrNotFound) {
			if rc != nil {
				_ = rc.Close()
			}
			t.Fatalf("artifact 删除后 %s 应不存在，得 %v", p, err)
		}
	}
	if _, rc, err := svc.Get("maven-logical", "com/example/other/maven-metadata.xml"); err != nil {
		t.Fatalf("相邻 artifact 的 metadata 不得受影响：%v", err)
	} else {
		_ = rc.Close()
	}
}

func TestAssetOperationNpmVersionUpdatesPackument(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("npm-ops", "npm", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	packument := map[string]any{
		"name": "demo",
		"versions": map[string]any{
			"1.0.0": map[string]any{"dist": map[string]any{"tarball": "https://registry/demo/-/demo-1.0.0.tgz"}},
			"2.0.0": map[string]any{"dist": map[string]any{"tarball": "https://registry/demo/-/demo-2.0.0.tgz"}},
		},
		"dist-tags": map[string]any{"latest": "2.0.0"},
	}
	packumentBytes, _ := json.Marshal(packument)
	if _, err := svc.Put("npm-ops", "demo", bytes.NewReader(packumentBytes), "application/json"); err != nil {
		t.Fatalf("发布 packument：%v", err)
	}
	if _, err := svc.Put("npm-ops", "demo/-/demo-1.0.0.tgz", bytes.NewBufferString("old"), "application/octet-stream"); err != nil {
		t.Fatalf("发布 tarball：%v", err)
	}
	if _, err := svc.Put("npm-ops", "demo/-/demo-2.0.0.tgz", bytes.NewBufferString("new"), "application/octet-stream"); err != nil {
		t.Fatalf("发布 tarball：%v", err)
	}
	result, err := svc.ApplyOperation("npm-ops", domain.AssetOperation{
		Action:  domain.AssetOperationDelete,
		Targets: []domain.AssetOperationTarget{{Type: domain.AssetTargetNpmVersion, Path: "demo@1.0.0"}},
	})
	if err != nil || result.Affected != 2 {
		t.Fatalf("npm 版本删除结果异常：result=%+v err=%v", result, err)
	}
	_, rc, err := svc.Get("npm-ops", "demo")
	if err != nil {
		t.Fatalf("packument 应保留：%v", err)
	}
	updated, _ := io.ReadAll(rc)
	_ = rc.Close()
	if bytes.Contains(updated, []byte(`"1.0.0"`)) || !bytes.Contains(updated, []byte(`"2.0.0"`)) {
		t.Fatalf("packument 未正确更新：%s", updated)
	}
	if _, rc, err := svc.Get("npm-ops", "demo/-/demo-1.0.0.tgz"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("旧 tarball 应删除，得 %v", err)
	} else if rc != nil {
		_ = rc.Close()
	}
}
