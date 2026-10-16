package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// TestRestoreFromLocalPackageRequiresAdmin 未认证一律 401。
func TestRestoreFromLocalPackageRequiresAdmin(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	if code, _ := env.request(t, http.MethodPost, "/api/v1/backups/bk-x/restore", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", code)
	}
}

// TestRestoreFromLocalPackageRejectsMissingPackage 包不存在时 404（不能凭空恢复）。
func TestRestoreFromLocalPackageRejectsMissingPackage(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)
	if code, _ := env.request(t, http.MethodPost, "/api/v1/backups/bk-does-not-exist/restore", token, nil); code != http.StatusNotFound {
		t.Fatalf("包不存在应 404，实际 %d", code)
	}
}

// TestRestoreFromLocalPackageRejectsNotDone 未生成完成的包不能作为恢复源。
//
// 判据与演练一致（都要求 done），但**消息不同**：把"无法演练"原样抛给恢复请求
// 会让用户以为点错了功能，故用 ErrRestorePackageNotDone 单独表达。
func TestRestoreFromLocalPackageRejectsNotDone(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 直接登记一条非 done 的包（生成中）。
	if _, err := env.db.Exec(
		`INSERT INTO backup_package (package_id, mode, status, origin, counts_json, created_at)
		 VALUES ('bk-pending', 'hot', 'packing', 'manual', '{}', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("插入测试包：%v", err)
	}

	code, body := env.request(t, http.MethodPost, "/api/v1/backups/bk-pending/restore", token, nil)
	if code != http.StatusConflict {
		t.Fatalf("非 done 包应 409，实际 %d body=%s", code, body)
	}
	if got := decodeErrorCode(t, body); got != "backup_not_done" {
		t.Fatalf("error_code 应为 backup_not_done，实际 %q", got)
	}
}

// decodeErrorCode 从错误响应体里取出 error.code。
func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("解析错误响应：%v（原文 %s）", err, body)
	}
	return parsed.Error.Code
}

// TestRestoreFromLocalPackageAcceptsLocalOrigin 端到端走通"用本机包恢复"，
// 并断言导入记录以 origin=local 落库。
//
// **这条守卫来自一次实机 500**：新增该入口时只改了契约与 devmock，漏了数据库——
// 0035 给 backup_import.origin 写了表级 CHECK IN ('url','upload','cli')，
// 写入 'local' 被拒，并被兜成 internal_error，日志里也看不出原因。
// 契约 / devmock / 前端三处一致**并不能**保证真实数据库接受该取值。
// 故必须有一条走真实库的用例真正执行一次插入，把这类"约束没跟上"拦在本地。
func TestRestoreFromLocalPackageAcceptsLocalOrigin(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 造真实包：先生成（需要 db 与 blob 都在 env 里），再对它发起恢复。
	env.seedAsset(t, "restore-origin-payload")
	pkg, err := env.svc.Generate(context.Background(), domain.CreateBackupOptions{
		Mode:  archive.ModeHot,
		Label: "恢复入口验收",
	})
	if err != nil {
		t.Fatalf("生成备份包：%v", err)
	}

	code, body := env.request(t, http.MethodPost,
		"/api/v1/backups/"+pkg.PackageID+"/restore", token, map[string]any{"overwrite": true})
	if code != http.StatusAccepted {
		t.Fatalf("受理应 202，实际 %d body=%s", code, body)
	}

	var rec api.BackupImport
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("解析响应：%v（原文 %s）", err, body)
	}
	// 核心断言：origin 必须是 local，且真的落了库（读回来的值来自数据库而非请求回显）。
	if rec.Origin != "local" {
		t.Fatalf("origin 应为 local，实际 %q", rec.Origin)
	}
	stored, err := env.backupImports.Get(rec.ImportId)
	if err != nil {
		t.Fatalf("读回导入记录：%v", err)
	}
	if stored.Origin != "local" {
		t.Fatalf("落库的 origin 应为 local，实际 %q（若这里失败，多半是 CHECK 约束没跟着放宽）", stored.Origin)
	}
}
