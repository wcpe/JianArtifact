package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

func TestZzDiagImportProgress(t *testing.T) {
	src := newBackupEnv(t)
	srcTok := src.adminToken(t)
	src.seedAsset(t, "diag-payload")
	code, body := src.request(t, http.MethodPost, "/api/v1/backups", srcTok, map[string]any{"mode": "hot"})
	var created struct {
		PackageId string `json:"packageId"`
	}
	_ = json.Unmarshal(body, &created)
	src.waitDone(t, srcTok, created.PackageId)
	pkgPath := src.svc.PackagePath(created.PackageId)
	t.Logf("create backup code=%d pkg=%s", code, pkgPath)

	tgt := newBackupEnv(t)
	_ = tgt.adminToken(t)
	restoreSvc := domain.NewRestoreService(tgt.db, tgt.dir,
		filepath.Join(tgt.dir, "jianartifact.db"), filepath.Join(tgt.dir, "restore-blobs"))

	// 直接同步调用 Stage（与 domain 测试一致），确认 Stage 本身不挂起。
	type stageRes struct{ err error }
	ch := make(chan stageRes, 1)
	go func() {
		_, se := restoreSvc.Stage(context.Background(), domain.RestoreRequest{
			SourcePath: pkgPath, Origin: "cli",
		})
		ch <- stageRes{err: se}
	}()
	select {
	case r := <-ch:
		t.Logf("DIRECT Stage returned err=%v", r.err)
	case <-time.After(8 * time.Second):
		t.Logf("DIRECT Stage HUNG (8s)")
	}
}
