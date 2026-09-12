package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestBackupImportUnauthorized 覆盖 URL 导入端点的鉴权：未带令牌 401。
func TestBackupImportUnauthorized(t *testing.T) {
	env := newBackupEnv(t)
	if code, _ := env.request(t, http.MethodPost, "/api/v1/backups/import", "",
		map[string]any{"sourceUrl": "http://127.0.0.1:1/x.tar.gz"}); code != http.StatusUnauthorized {
		t.Fatalf("未认证导入应 401，实际 %d", code)
	}
	// 列表与详情同样需要管理员。
	if code, _ := env.request(t, http.MethodGet, "/api/v1/backups/imports", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("未认证列表应 401，实际 %d", code)
	}
}

// TestBackupImportMissingSourceURL 覆盖契约校验：缺 sourceUrl → 400。
func TestBackupImportMissingSourceURL(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	if code, body := env.request(t, http.MethodPost, "/api/v1/backups/import", token,
		map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("缺 sourceUrl 应 400，实际 %d body=%s", code, body)
	}
}

// TestBackupImportListAndDetail 覆盖导入记录的列表/详情读取：
// 直接经仓储造两条记录 → 列表返回 {items,total} 且最近优先 → 详情命中 → 未知 id 404。
func TestBackupImportListAndDetail(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	importRepo := repository.NewBackupImportRepo(env.db)
	now := time.Now().UTC()
	rec1 := repository.BackupImport{
		ImportID:  "imp-test-1",
		Origin:    repository.ImportOriginURL,
		Status:    repository.ImportStatusFailed,
		SourceURL: "http://127.0.0.1:1/first.tar.gz",
		Operator:  "admin",
		CreatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		UpdatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
	}
	rec2 := repository.BackupImport{
		ImportID:  "imp-test-2",
		Origin:    repository.ImportOriginURL,
		Status:    repository.ImportStatusPendingRestart,
		SourceURL: "http://127.0.0.1:1/second.tar.gz",
		Operator:  "admin",
		CreatedAt: now.Format(time.RFC3339Nano),
		UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := importRepo.Create(rec1); err != nil {
		t.Fatalf("插入记录1：%v", err)
	}
	if err := importRepo.Create(rec2); err != nil {
		t.Fatalf("插入记录2：%v", err)
	}

	// 列表。
	code, body := env.request(t, http.MethodGet, "/api/v1/backups/imports?page=1&page_size=10", token, nil)
	if code != http.StatusOK {
		t.Fatalf("列表：status=%d body=%s", code, body)
	}
	var list api.BackupImportList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("解析列表：%v", err)
	}
	if list.Total < 2 {
		t.Fatalf("列表 total=%d，期望 >=2", list.Total)
	}
	if len(list.Items) < 2 {
		t.Fatalf("列表 items 长度=%d，期望 >=2", len(list.Items))
	}
	// 最近优先：后创建的 imp-test-2 应排在首位。
	if list.Items[0].ImportId != "imp-test-2" {
		t.Fatalf("列表未最近优先：首位=%s，期望 imp-test-2", list.Items[0].ImportId)
	}

	// 详情命中。
	if code, body = env.request(t, http.MethodGet, "/api/v1/backups/imports/imp-test-1", token, nil); code != http.StatusOK {
		t.Fatalf("详情：status=%d body=%s", code, body)
	}
	var detail api.BackupImport
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("解析详情：%v", err)
	}
	if detail.ImportId != "imp-test-1" {
		t.Fatalf("详情 id=%s，期望 imp-test-1", detail.ImportId)
	}

	// 未知 id → 404（验证 writeBackupImportErr 对 repository.ErrNotFound 的映射）。
	if code, _ = env.request(t, http.MethodGet, "/api/v1/backups/imports/imp-not-exist", token, nil); code != http.StatusNotFound {
		t.Fatalf("未知 id 应 404，实际 %d", code)
	}
}

// TestBackupImportCreatesQueuedRecord 覆盖全新节点经 URL 拉取导入的受理路径：
// 无 restore.pending 时 POST /api/v1/backups/import 应立刻返回 202 并登记一条 queued 记录。
//
// 这是此前被 restore_service.go 的 MarkedPending 缺文件未归一化 (nil,nil) 阻塞的回归点
// （彼时全新节点恒返 500）。该 bug 已修，故此处为正向断言。
func TestBackupImportCreatesQueuedRecord(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	// 指向一个不可达的回环地址即可：受理（202）发生在后台拉取之前，与拉取成败无关。
	code, body := env.request(t, http.MethodPost, "/api/v1/backups/import", token,
		map[string]any{"sourceUrl": "http://127.0.0.1:1/fresh-node.tar.gz"})
	if code != http.StatusAccepted {
		t.Fatalf("全新节点导入应 202，实际 %d body=%s", code, body)
	}
	var rec api.BackupImport
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("解析导入响应：%v（原文 %s）", err, body)
	}
	if rec.ImportId == "" {
		t.Fatal("导入响应缺少 importId")
	}
	if rec.Status != "queued" {
		t.Fatalf("应登记 queued 记录，实得 status=%q", rec.Status)
	}
}

// TestBackupImportFetchFailed 覆盖 URL 拉取失败路径：指向不可达地址 → 记录置 failed +
// error_code=fetch_failed（后台 goroutine 失败，HTTP 受理仍为 202）。
func TestBackupImportFetchFailed(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)
	id := startImport(t, env, token, "http://127.0.0.1:1/dead.tar.gz", false)
	rec := waitImportTerminal(t, env, token, id)
	if rec.Status != "failed" {
		t.Fatalf("拉取失败应置 failed，实得 status=%q", rec.Status)
	}
	if rec.ErrorCode == nil || *rec.ErrorCode != "fetch_failed" {
		t.Fatalf("期望 error_code=fetch_failed，实得 %v", rec.ErrorCode)
	}
}

// TestBackupImportRestorePendingReturns409 覆盖「已有待生效恢复」时拒绝叠加：
// 在 RestoreService 的同一 dataDir 写入 restore.pending，再触发导入。
// MarkedPending 检查在受理路径同步执行，故返回 409 + error_code=restore_pending。
func TestBackupImportRestorePendingReturns409(t *testing.T) {
	env := newBackupEnv(t)
	token := env.adminToken(t)

	// 直接写入待生效标记（与 RestoreService 共用 env.dir 作为 dataDir）。
	pending := domain.PendingRestore{
		ImportID:             "rs-pre",
		PackageID:            "bk-pre",
		StagedDBRelativePath: filepath.Join("restore-staging", archive.DBName),
	}
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		t.Fatalf("序列化标记：%v", err)
	}
	if err := os.WriteFile(filepath.Join(env.dir, "restore.pending"), data, 0o600); err != nil {
		t.Fatalf("写待生效标记：%v", err)
	}

	code, body := env.request(t, http.MethodPost, "/api/v1/backups/import", token,
		map[string]any{"sourceUrl": "http://127.0.0.1:1/x.tar.gz"})
	if code != http.StatusConflict {
		t.Fatalf("已有待生效恢复应 409，实际 %d body=%s", code, body)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("解析错误体：%v", err)
	}
	if errBody.Error.Code != "restore_pending" {
		t.Fatalf("期望 error_code=restore_pending，实得 %q", errBody.Error.Code)
	}
}

// TestBackupImportTargetNotEmptyFailsImport 覆盖「目标非空且未 overwrite」拒绝：
// 在目标实例植入一个真实用户（内置 anonymous 不计入非空，见 TargetNonEmpty），
// 拉取一个真实可校验的包 → 后台 Stage 判非空 → 记录置 failed + error_code=target_not_empty。
//
// 注：target_not_empty 在异步 Stage 内判定，故 HTTP 受理仍为 202，失败以记录终态表达
// （writeBackupImportErr 中 409 + target_not_empty 的映射对 URL 通道不可同步触达，
// 这是当前实现下唯一可观测的行为；已就此点与 team-lead 对齐）。
func TestBackupImportTargetNotEmptyFailsImport(t *testing.T) {
	src := newBackupEnv(t)
	srcTok := src.adminToken(t)
	url, _ := serveRealBackupPackage(t, src, srcTok, "payload-for-nonempty")

	tgt := newBackupEnv(t)
	tgtTok := tgt.adminToken(t)
	// 植入真实用户（anonymous 被 TargetNonEmpty 排除，故必须显式插入非匿名主体）。
	if _, err := tgt.db.Exec(
		`INSERT INTO user (username, password_hash, role, status) VALUES ('real-user','ph','admin','active')`); err != nil {
		t.Fatalf("插入真实用户：%v", err)
	}

	id := startImport(t, tgt, tgtTok, url, false)
	rec := waitImportTerminal(t, tgt, tgtTok, id)
	if rec.Status != "failed" {
		t.Fatalf("非空目标导入应失败，实得 status=%q", rec.Status)
	}
	if rec.ErrorCode == nil || *rec.ErrorCode != "target_not_empty" {
		t.Fatalf("期望 error_code=target_not_empty，实得 %v", rec.ErrorCode)
	}
}

// TestBackupImportSuccessStagesPendingRestart 覆盖成功链路：从源实例生成一个真实包并经
// httptest 服务拉取导入到全新目标 → 记录走到 pending_restart、restore_pending_at 非空、
// blob_count > 0、package_id 与源包一致。需上游 NewTestClient 放行回环方可跑通。
func TestBackupImportSuccessStagesPendingRestart(t *testing.T) {
	src := newBackupEnv(t)
	srcTok := src.adminToken(t)
	url, pkgID := serveRealBackupPackage(t, src, srcTok, "payload-to-import")

	tgt := newBackupEnv(t)
	tgtTok := tgt.adminToken(t)
	// tgt 已自举管理员（非空），导入到已有节点须显式 overwrite。
	id := startImport(t, tgt, tgtTok, url, true)
	rec := waitImportTerminal(t, tgt, tgtTok, id)

	if rec.Status != "pending_restart" {
		t.Fatalf("成功导入应 pending_restart，实得 status=%q errorCode=%v",
			rec.Status, deref(t, rec.ErrorCode))
	}
	if rec.RestorePendingAt == nil || deref(t, rec.RestorePendingAt) == "" {
		t.Fatal("restore_pending_at 应非空")
	}
	if rec.BlobCount == nil || *rec.BlobCount <= 0 {
		t.Fatalf("blob_count 应 >0，实得 %v", rec.BlobCount)
	}
	if rec.PackageId == nil || *rec.PackageId != pkgID {
		t.Fatalf("package_id 应=%s，实得 %v", pkgID, rec.PackageId)
	}
}

// serveRealBackupPackage 在源环境生成一份真实（可校验）的节点备份包并起一个 httptest
// 服务对外提供下载，返回服务地址与源包标识。供 target_not_empty / 成功链路用例复用。
func serveRealBackupPackage(t *testing.T, srcEnv *backupEnv, srcToken, content string) (string, string) {
	t.Helper()
	srcEnv.seedAsset(t, content)
	code, body := srcEnv.request(t, http.MethodPost, "/api/v1/backups", srcToken, map[string]any{"mode": "hot"})
	if code != http.StatusCreated {
		t.Fatalf("生成备份：status=%d body=%s", code, body)
	}
	var created api.BackupPackage
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("解析生成响应：%v", err)
	}
	srcEnv.waitDone(t, srcToken, created.PackageId)

	pkgPath := srcEnv.svc.PackagePath(created.PackageId)
	if _, err := os.Stat(pkgPath); err != nil {
		t.Fatalf("包文件不存在：%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, pkgPath)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, created.PackageId
}

// startImport 通过 HTTP 触发一次 URL 拉取导入并返回导入 id（仅断言 202）。
// overwrite 控制是否在导入请求中显式允许覆盖非空目标：全新节点若已自举管理员
// （本测试为了鉴权令牌都会自举），目标即非空，导入成功路径须传 overwrite=true。
func startImport(t *testing.T, env *backupEnv, token, sourceURL string, overwrite bool) string {
	t.Helper()
	body := map[string]any{"sourceUrl": sourceURL}
	if overwrite {
		body["overwrite"] = true
	}
	code, raw := env.request(t, http.MethodPost, "/api/v1/backups/import", token, body)
	if code != http.StatusAccepted {
		t.Fatalf("触发导入：status=%d body=%s", code, raw)
	}
	var rec api.BackupImport
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("解析导入响应：%v（原文 %s）", err, raw)
	}
	if rec.ImportId == "" {
		t.Fatal("导入响应缺少 importId")
	}
	return rec.ImportId
}

// waitImportTerminal 轮询导入详情直到终态（failed / pending_restart），超时 10s。
func waitImportTerminal(t *testing.T, env *backupEnv, token, id string) api.BackupImport {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := env.request(t, http.MethodGet, "/api/v1/backups/imports/"+id, token, nil)
		if code != http.StatusOK {
			t.Fatalf("查询导入详情：status=%d body=%s", code, body)
		}
		var rec api.BackupImport
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Fatalf("解析导入详情：%v", err)
		}
		switch rec.Status {
		case "failed", "pending_restart":
			return rec
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待导入终态超时")
	return api.BackupImport{}
}
