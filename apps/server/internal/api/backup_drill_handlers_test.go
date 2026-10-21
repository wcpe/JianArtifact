package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// drillAPIFixture 是恢复演练端点的最小装配：真实备份服务 + 真实演练服务 + 真实 sqlite。
//
// 刻意不用桩替换演练服务：这两个端点要验证的正是「202 是否真的会变成一份落库报告」，
// 而这条链路的正确性全在真实实现里（临时目录、解包、报告写入）。用桩会把被测对象换掉。
type drillAPIFixture struct {
	h       *Handlers
	svc     *domain.BackupService
	drill   *domain.BackupDrillService
	audits  *repository.AuditLogRepo
	dataDir string
}

// newDrillAPIFixture 建夹具并生成一个可演练的完成包，返回其包标识。
func newDrillAPIFixture(t *testing.T) (*drillAPIFixture, string) {
	t.Helper()
	dataDir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dataDir, "api-drill.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}

	store := blobstore.NewStore(filepath.Join(dataDir, "blobs"))
	backupSvc := domain.NewBackupService(db, repository.NewBackupPackageRepo(db), store,
		dataDir, filepath.Join(dataDir, "api-drill.db"), "0.11.0", func() string { return "np-drill-api" })

	// 写一件资产，让包体非空（空包的 blob 与 SQLite 检查都太容易蒙对）。
	res, err := db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-drill','raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	payload := []byte("drill-api-payload")
	hash, _, _, _, err := store.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type)
		VALUES (?, 'a/1.bin', ?, ?, 'application/octet-stream')`, repoID, hash, len(payload)); err != nil {
		t.Fatalf("插入资产：%v", err)
	}

	rec, err := backupSvc.Generate(t.Context(), domain.CreateBackupOptions{Mode: archive.ModeHot, Label: "演练夹具"})
	if err != nil {
		t.Fatalf("生成备份包：%v", err)
	}
	if rec.Status != repository.BackupStatusDone {
		t.Fatalf("生成状态 = %s，期望 done", rec.Status)
	}

	// 同一个演练服务实例同时给 Handlers 与用例用（用例要用它读 StagingRoot）。
	drillSvc := domain.NewBackupDrillService(backupSvc, repository.NewBackupVerificationRepo(db), dataDir)
	auditLogs := repository.NewAuditLogRepo(db)
	// 夹具收尾：等后台演练跑完再让 t.Cleanup 关库。
	// t.Cleanup 是后进先出，这条注册在 openAPITestDB 式关库之后，因此先于关库执行；
	// 少了它，用例一结束就关库，仍在跑的演练会写出一条 "database is closed" 日志——
	// 那是测试自身的竞态，不是产品缺陷，不该留在输出里误导排查。
	t.Cleanup(func() {
		for i := 0; i < 500; i++ {
			entries, err := os.ReadDir(drillSvc.StagingRoot())
			if err != nil || len(entries) == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return &drillAPIFixture{
		h: NewHandlers(Deps{
			Backups:     backupSvc,
			BackupDrill: drillSvc,
			AuditLogs:   auditLogs,
		}),
		svc:     backupSvc,
		drill:   drillSvc,
		audits:  auditLogs,
		dataDir: dataDir,
	}, rec.PackageID
}

// serveDrillRoute 经生成的 ServerInterfaceWrapper 发请求（测试口径即线上口径）。
func serveDrillRoute(h *Handlers, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: h,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.POST("/api/v1/backups/:id/drill", siw.DrillBackup)
	router.GET("/api/v1/backups/:id/verifications", siw.ListBackupVerifications)

	var reader *bytes.Buffer
	if body == "" {
		reader = bytes.NewBufferString("")
	} else {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 演练端点仅管理员可见：匿名 401、普通用户 403（发起与查询同边界）。
func TestBackupDrillEndpointsRequireAdmin(t *testing.T) {
	h := NewHandlers(Deps{})
	for _, principal := range []*auth.Principal{nil, userPrincipal()} {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/backups/bk-x/drill"},
			{http.MethodGet, "/api/v1/backups/bk-x/verifications"},
		} {
			rec := serveDrillRoute(h, principal, tc.method, tc.path, "")
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s 以 %v 身份请求：状态码 %d，期望 401 或 403", tc.method, tc.path, principal, rec.Code)
			}
		}
	}
}

// 未接线演练服务时明确 503，而不是空 200 或 panic。
func TestBackupDrillUnavailableWhenServiceMissing(t *testing.T) {
	h := NewHandlers(Deps{})
	rec := serveDrillRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/backups/bk-x/drill", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
}

// 核心行为：drill 返回 202 受理，随后报告真的落库并可经列表读到。
//
// 这两半必须一起断言："返回 202"本身不构成交付——真正承诺给用户的是
// 「我拿到的 202 最终变成一份能查到的报告」。
func TestBackupDrillAcceptsThenPersistsReport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", `{"level":"shallow"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("drill 状态码 = %d，期望 202；体：%s", rec.Code, rec.Body.String())
	}
	var accepted BackupDrillAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("解析受理体：%v（体：%s）", err, rec.Body.String())
	}
	if accepted.PackageId != pkgID {
		t.Fatalf("受理包标识 = %s，期望 %s", accepted.PackageId, pkgID)
	}
	if accepted.Level != Shallow {
		t.Fatalf("受理级别 = %s，期望 shallow", accepted.Level)
	}
	if _, err := time.Parse(time.RFC3339Nano, accepted.AcceptedAt); err != nil {
		t.Fatalf("受理时间不是 RFC3339：%v（%q）", err, accepted.AcceptedAt)
	}

	// 轮询直到报告出现（演练是后台任务，必须给它完成的机会）。
	report := waitForReport(t, f.h, pkgID)
	if !report.Ok {
		t.Fatalf("演练未通过：%+v", report.Checks)
	}
	// 四项检查齐全且全部通过——报告是结构化结论，不是一个 ok 字符串。
	want := map[string]bool{"integrity": true, "extract": true, "sqlite": true, "blobs": true}
	got := map[string]bool{}
	for _, check := range report.Checks {
		if check.Detail == "" {
			t.Fatalf("检查项 %s 缺人读结论", check.Name)
		}
		got[check.Name] = check.Ok
	}
	for name, ok := range want {
		if gotOK, present := got[name]; !present || gotOK != ok {
			t.Fatalf("检查项 %s：存在=%v 通过=%v（实得 %+v）", name, present, gotOK, report.Checks)
		}
	}
	if report.DurationMs < 0 {
		t.Fatalf("耗时 = %d，期望非负", report.DurationMs)
	}
}

// 演练以只读路径运行：跑完不得留下临时目录，更不得写出 restore.pending
// （写它会在下次重启触发真正的数据替换——这是与真恢复的关键边界）。
func TestBackupDrillLeavesNoRestoreMarkers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("drill 状态码 = %d，期望 202", rec.Code)
	}
	waitForDrillSettled(t, f.h, f.drill, pkgID)

	// 临时目录根下不得有残留（每次演练一个子目录，无论成败都清理）——
	// 上面的 waitForDrillSettled 已把这一点等成确定性事实。
	if entries, err := os.ReadDir(f.drill.StagingRoot()); err == nil && len(entries) != 0 {
		t.Fatalf("演练临时目录未清理干净：%d 项残留", len(entries))
	}
	for _, marker := range []string{"restore.pending", "restore-staging"} {
		if _, err := os.Stat(filepath.Join(f.dataDir, marker)); err == nil {
			t.Fatalf("演练产生了 %s——它必须是只读路径", marker)
		}
	}
}

// 发起演练写审计：动作名 backup.drill、entityType=backup_package、entityKey=包标识。
func TestBackupDrillWritesAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", `{"level":"shallow"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("drill 状态码 = %d，期望 202", rec.Code)
	}
	// 审计在 202 之前就已写入，所以本用例断言审计其实不需要等后台演练。但**必须**先等它收尾：
	// 后台演练持有 fixture 的 t.TempDir()，不等它结束就返回，t.TempDir() 的清理会与
	// 演练自身的 RemoveAll 赛跑，偶发报 "TempDir RemoveAll cleanup: directory is not empty"
	// （实测在 CI 全量并发下真实出现过）。这里复用既有助手，与同文件的其它用例口径一致。
	waitForDrillSettled(t, f.h, f.drill, pkgID)

	entries, err := f.audits.List(repository.AuditFilter{Action: auditActionBackupDrill, Limit: 10})
	if err != nil {
		t.Fatalf("读审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup.drill 审计条数 = %d，期望 1", len(entries))
	}
	if entries[0].EntityType != "backup_package" || entries[0].EntityKey != pkgID {
		t.Fatalf("审计主体不符：%+v", entries[0])
	}
}

// 不可受理的请求必须在受理前被拒，而不是先给 202 再在后台悄悄失败。
func TestBackupDrillRejectsUnstartableRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	// 包不存在 → 404。
	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost, "/api/v1/backups/bk-nope/drill", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的包：状态码 = %d，期望 404", rec.Code)
	}

	// 级别非法 → 400。
	rec = serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", `{"level":"ultra"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法级别：状态码 = %d，期望 400；体：%s", rec.Code, rec.Body.String())
	}

	// 请求体不是 JSON → 400。
	rec = serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏请求体：状态码 = %d，期望 400", rec.Code)
	}
}

// 报告列表：包不存在 404（不返回空列表——那会把"包没了"误读成"还没演练"）；
// 存在的包在无报告时返回空数组而非 null，且 total 与 items 口径一致。
func TestListBackupVerificationsShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodGet, "/api/v1/backups/bk-nope/verifications", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的包：状态码 = %d，期望 404", rec.Code)
	}

	rec = serveDrillRoute(f.h, adminPrincipal(), http.MethodGet, "/api/v1/backups/"+pkgID+"/verifications", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("空历史应序列化为空数组，实得：%s", rec.Body.String())
	}

	// 跑一次演练后再查：total 计数与 items 长度一致，且最近优先。
	if rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("drill 状态码 = %d，期望 202", rec.Code)
	}
	waitForReport(t, f.h, pkgID)

	rec = serveDrillRoute(f.h, adminPrincipal(), http.MethodGet, "/api/v1/backups/"+pkgID+"/verifications", "")
	var list BackupVerificationList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表：%v（体：%s）", err, rec.Body.String())
	}
	if list.Total != len(list.Items) || list.Total != 1 {
		t.Fatalf("total=%d items=%d，期望都为 1", list.Total, len(list.Items))
	}
}

// 列表分页参数生效：越界页返回空数组但 total 仍是全量总数。
func TestListBackupVerificationsPaging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f, pkgID := newDrillAPIFixture(t)

	if rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodPost,
		"/api/v1/backups/"+pkgID+"/drill", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("drill 状态码 = %d，期望 202", rec.Code)
	}
	waitForReport(t, f.h, pkgID)

	rec := serveDrillRoute(f.h, adminPrincipal(), http.MethodGet,
		fmt.Sprintf("/api/v1/backups/%s/verifications?page=9&page_size=1", pkgID), "")
	var list BackupVerificationList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表：%v", err)
	}
	if list.Total != 1 {
		t.Fatalf("total = %d，期望 1（越界页不该改变总数）", list.Total)
	}
	if len(list.Items) != 0 {
		t.Fatalf("越界页应无条目，实得 %d 条", len(list.Items))
	}
}

// 列表里的 origin 字段：契约把它声明为 required，因此任何来源的行都必须带合法取值。
func TestBackupPackageOriginAlwaysPresent(t *testing.T) {
	if got := toAPIBackupPackage(repository.BackupPackage{PackageID: "bk-a"}).Origin; got != Manual {
		t.Fatalf("空来源应兜底为 manual，实得 %q", got)
	}
	if got := toAPIBackupPackage(repository.BackupPackage{Origin: repository.BackupOriginAuto}).Origin; got != Auto {
		t.Fatalf("auto 应原样透出，实得 %q", got)
	}
	// 未知取值（绕过 repository 构造的记录）按 manual 呈现，而不是吐出非法枚举。
	if got := toAPIBackupPackage(repository.BackupPackage{Origin: "unknown"}).Origin; got != Manual {
		t.Fatalf("未知来源应兜底为 manual，实得 %q", got)
	}
}

// waitForReport 轮询报告列表直到出现一条报告（后台演练完成）。
//
// 注意：报告行落库发生在临时目录清理**之前**（见 Drill 的实现顺序），
// 所以"拿到报告"不等于"演练已彻底收尾"。需要观察收尾副作用的用例请用 waitForDrillSettled。
func waitForReport(t *testing.T, h *Handlers, pkgID string) BackupVerificationReport {
	t.Helper()
	for i := 0; i < 500; i++ {
		rec := serveDrillRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/backups/"+pkgID+"/verifications", "")
		var list BackupVerificationList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err == nil && len(list.Items) > 0 {
			return list.Items[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待演练报告超时（包 %s）", pkgID)
	return BackupVerificationReport{}
}

// waitForDrillSettled 等到演练彻底收尾：报告已落库，且临时目录已清理干净。
//
// 拆成两个条件而不是只等其中一个，是因为后台演练的两步有确定顺序（先写报告、后清目录），
// 只等报告会让"清理"这一步的断言与它赛跑；只等目录又可能错过"报告压根没写"的失败。
func waitForDrillSettled(t *testing.T, h *Handlers, drill *domain.BackupDrillService, pkgID string) BackupVerificationReport {
	t.Helper()
	report := waitForReport(t, h, pkgID)
	for i := 0; i < 500; i++ {
		entries, err := os.ReadDir(drill.StagingRoot())
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("读取演练临时目录：%v", err)
		}
		if len(entries) == 0 {
			return report
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待演练临时目录清理超时（包 %s）", pkgID)
	return BackupVerificationReport{}
}
