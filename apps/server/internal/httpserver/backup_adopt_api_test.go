// 未管理备份包的 HTTP 边界集成测试。
//
// 覆盖三件事：
//   - 扫描能列出"磁盘有包体、登记无记录"的包，已登记的包不出现；
//   - 导入后包可从常规端点查到，且不再出现在未管理列表里；
//   - 导入只接受管理员，非法输入被拒。
package httpserver_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// forgetRegistration 制造"文件在、登记没了"的失步：直接从库里删登记行，保留包体。
// 这正是历史上孤儿包的形态（BackupService.Delete 是"先文件后登记"，正常路径不产生它）。
func (e *backupEnv) forgetRegistration(t *testing.T, packageID string) {
	t.Helper()
	if err := repository.NewBackupPackageRepo(e.db).Delete(packageID); err != nil {
		t.Fatalf("删除登记 %s：%v", packageID, err)
	}
}

// regularUserToken 造一个非管理员账号并登录，用于权限拒绝断言。
func (e *backupEnv) regularUserToken(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword("regular-user-pass-1")
	if err != nil {
		t.Fatalf("生成口令哈希：%v", err)
	}
	if _, err := repository.NewUserRepo(e.db).Create("regular", hash, "user"); err != nil {
		t.Fatalf("创建普通用户：%v", err)
	}
	code, body := e.request(t, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"username": "regular", "password": "regular-user-pass-1"})
	if code != http.StatusOK {
		t.Fatalf("普通用户登录失败：status=%d body=%s", code, body)
	}
	var out api.LoginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析登录响应：%v", err)
	}
	return out.Token
}

// TestUnmanagedBackupsScanAndAdopt 覆盖主链路：列出 → 导入 → 可查 → 从列表消失。
func TestUnmanagedBackupsScanAndAdopt(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)
	env.seedAsset(t, "unmanaged-adopt-payload")

	// 造一份真实包，然后抹掉它的登记，让它变成"未管理"。
	bc, bb := env.request(t, http.MethodPost, "/api/v1/backups", token, map[string]any{"mode": "hot"})
	if bc != http.StatusCreated {
		t.Fatalf("生成备份：status=%d body=%s", bc, bb)
	}
	var pkg api.BackupPackage
	if err := json.Unmarshal(bb, &pkg); err != nil {
		t.Fatalf("解析生成响应：%v", err)
	}
	env.waitDone(t, token, pkg.PackageId)
	env.forgetRegistration(t, pkg.PackageId)

	// 扫描：该包应出现且 valid=true。
	code, body := env.request(t, http.MethodGet, "/api/v1/backups/unmanaged", token, nil)
	if code != http.StatusOK {
		t.Fatalf("扫描未管理包：status=%d body=%s", code, body)
	}
	var list api.UnmanagedBackupList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("解析扫描结果：%v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("应有 1 个未管理包，实得 total=%d items=%d", list.Total, len(list.Items))
	}
	got := list.Items[0]
	if got.FileId != pkg.PackageId {
		t.Fatalf("未管理包标识应为 %s，实得 %s", pkg.PackageId, got.FileId)
	}
	if !got.Valid {
		t.Fatalf("真实包应判定为合法，实得 reason=%v", got.Reason)
	}

	// 导入。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/adopt", token,
		map[string]any{"fileIds": []string{got.FileId}})
	if code != http.StatusOK {
		t.Fatalf("导入：status=%d body=%s", code, body)
	}
	var res api.AdoptBackupsResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("解析导入结果：%v", err)
	}
	if res.Adopted != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("导入结果应为 1 成功，实得 %+v", res)
	}

	// 导入后可从常规端点查到，且状态 done。
	code, body = env.request(t, http.MethodGet, "/api/v1/backups/"+got.FileId, token, nil)
	if code != http.StatusOK {
		t.Fatalf("导入后应能查到该包：status=%d body=%s", code, body)
	}
	var stored api.BackupPackage
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatalf("解析包详情：%v", err)
	}
	if string(stored.Status) != "done" {
		t.Fatalf("导入包状态应为 done，实得 %v", stored.Status)
	}
	// 来源必须是 manual：标成 auto 会被保留策略当成周期备份淘汰掉。
	if string(stored.Origin) != "manual" {
		t.Fatalf("导入包来源应为 manual，实得 %v", stored.Origin)
	}

	// 再次扫描：该包不应再出现（差集已闭合）。
	code, body = env.request(t, http.MethodGet, "/api/v1/backups/unmanaged", token, nil)
	if code != http.StatusOK {
		t.Fatalf("重新扫描：status=%d body=%s", code, body)
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("解析扫描结果：%v", err)
	}
	if list.Total != 0 {
		t.Fatalf("导入后未管理列表应为空，实得 %d 个", list.Total)
	}
}

// TestUnmanagedBackupsAdoptRejectsInvalid 覆盖拒绝：不存在的标识与路径穿越都不得导入。
func TestUnmanagedBackupsAdoptRejectsInvalid(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)

	code, body := env.request(t, http.MethodPost, "/api/v1/backups/adopt", token,
		map[string]any{"fileIds": []string{"../etc/passwd", "bk-does-not-exist"}})
	if code != http.StatusOK {
		t.Fatalf("导入非法标识：status=%d body=%s", code, body)
	}
	var res api.AdoptBackupsResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("解析导入结果：%v", err)
	}
	if res.Adopted != 0 {
		t.Fatalf("非法标识不应导入任何包，实得 %+v", res)
	}
	if res.Items[0].FileId != "../etc/passwd" {
		t.Fatalf("结果应按请求顺序回显，实得 %+v", res.Items)
	}
}

// TestUnmanagedBackupsRequiresAdmin 覆盖权限：非管理员被拒。
func TestUnmanagedBackupsRequiresAdmin(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	userToken := env.regularUserToken(t)

	code, body := env.request(t, http.MethodGet, "/api/v1/backups/unmanaged", userToken, nil)
	if code != http.StatusForbidden {
		t.Fatalf("非管理员扫描应 403，实得 %d body=%s", code, body)
	}
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/adopt", userToken,
		map[string]any{"fileIds": []string{"x"}})
	if code != http.StatusForbidden {
		t.Fatalf("非管理员导入应 403，实得 %d body=%s", code, body)
	}
}

// TestUnmanagedBackupsSkipsNonBackupFiles 覆盖身份校验：
// backups/ 下无法识别的归档会被列出但标为非法，且不可导入。
func TestUnmanagedBackupsSkipsNonBackupFiles(t *testing.T) {
	t.Parallel()
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 放一个不是备份包的 tar.gz。
	if err := os.MkdirAll(filepath.Join(env.dir, "backups"), 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	fakeID := "bk-20260101-000000-fake01"
	if err := os.WriteFile(filepath.Join(env.dir, "backups", fakeID+".tar.gz"), []byte("not a tarball"), 0o600); err != nil {
		t.Fatalf("写伪包：%v", err)
	}

	code, body := env.request(t, http.MethodGet, "/api/v1/backups/unmanaged", token, nil)
	if code != http.StatusOK {
		t.Fatalf("扫描：status=%d body=%s", code, body)
	}
	var list api.UnmanagedBackupList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("解析扫描结果：%v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("应列出 1 个候选，实得 %d", len(list.Items))
	}
	if list.Items[0].Valid {
		t.Fatal("伪包应判定为非法")
	}
	if list.Items[0].Reason == nil || *list.Items[0].Reason == "" {
		t.Fatal("非法包必须给出原因")
	}

	// 导入它应被跳过。
	code, body = env.request(t, http.MethodPost, "/api/v1/backups/adopt", token,
		map[string]any{"fileIds": []string{fakeID}})
	if code != http.StatusOK {
		t.Fatalf("导入伪包：status=%d body=%s", code, body)
	}
	var res api.AdoptBackupsResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("解析导入结果：%v", err)
	}
	if res.Adopted != 0 || res.Skipped != 1 {
		t.Fatalf("伪包应被跳过（0 成功 1 跳过），实得 %+v", res)
	}
}
