package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 编译期断言：真实领域服务直接满足接口，wiring 只需一行赋值（无需适配器）。
var _ RestartController = (*domain.RestartService)(nil)

// restartTriggerStub 是重启触发点的桩：记录每次触发的**时刻**，并可按需返回失败。
//
// 为什么不直接用真的 domain.RestartService 去 Exec：那会把测试进程换掉，
// 整个用例集当场消失。桩替掉的是"重启动作"，而 Probe 的判据、handler 的状态码、
// 顺序契约全都仍由真实领域代码决定——因此这些断言测的是线上同一套逻辑。
type restartTriggerStub struct {
	mu      sync.Mutex
	times   []time.Time
	trigger chan struct{}
}

func newRestartTriggerStub() *restartTriggerStub {
	return &restartTriggerStub{trigger: make(chan struct{}, 8)}
}

func (s *restartTriggerStub) fn() domain.RestartTrigger {
	return func() {
		s.mu.Lock()
		s.times = append(s.times, time.Now())
		s.mu.Unlock()
		s.trigger <- struct{}{}
	}
}

func (s *restartTriggerStub) calls() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

// serveRestartRoute 经生成的 ServerInterfaceWrapper 发起重启请求（测试口径即线上口径）。
func serveRestartRoute(h *Handlers, principal *auth.Principal) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{Handler: h}
	router.POST("/api/v1/maintenance/restart", siw.RestartService)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/restart", nil))
	return rec
}

// serveRestartCapabilityRoute 经生成的 ServerInterfaceWrapper 发起**只读探测**请求。
// 与 POST 走同一套包装器，确保测试口径即线上口径（含鉴权中间件顺序）。
func serveRestartCapabilityRoute(h *Handlers, principal *auth.Principal) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{Handler: h}
	router.GET("/api/v1/maintenance/restart", siw.GetRestartCapability)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/maintenance/restart", nil))
	return rec
}

// restartTestHandlers 造一个接了真实 domain 服务的 handler，且平台判据被置为"支持"。
//
// 平台判据在 Windows 开发机上是 false（syscall.Exec 不可用），若不让它可注入，
// 所有"受理"用例都会走降级路径、把 202 断言变成假红。真实默认值由领域层
// TestRestartServiceRejectsUnsupportedPlatform 盯着；本层的注意力在状态码与顺序契约上。
func restartTestHandlers(stub *restartTriggerStub, unit, scope string, euid int) *Handlers {
	return NewHandlers(Deps{Restart: restartServiceForTest(stub, unit, scope, euid, nil)})
}

// restartServiceForTest 造真实领域服务，只把不可在本机执行的三种原语换掉：
// 平台判据（Windows 不支持）、argv[0] 解析（测试机没有那个路径）、euid（不一定是 root）。
// Argv0 仍返回真实的 os.Args[0]，使"路径取自哪里"这条断言依然有效。
func restartServiceForTest(stub *restartTriggerStub, unit, scope string, euid int, resolve func(string) error) *domain.RestartService {
	svc := domain.NewRestartServiceWithHooks(unit, scope, domain.RestartHooks{
		Resolve:           resolve,
		EUID:              func() int { return euid },
		PlatformSupported: func() bool { return true },
		Trigger:           stub.fn(),
	})
	return svc
}

// restartWaitTrigger 是"等一次重启触发"的观察窗。
// 必须**长于 restartDelay**（handler 排定的 1.8 秒延迟），否则会误判成"没触发"。
const restartWaitTrigger = 4 * time.Second

// waitTrigger 等一次触发并返回是否等到。
func waitTrigger(t *testing.T, stub *restartTriggerStub) bool {
	t.Helper()
	select {
	case <-stub.trigger:
		return true
	case <-time.After(restartWaitTrigger):
		return false
	}
}

// 重启是最高权限动作：匿名 401、普通用户 403，且**越权请求绝不能触发重启**。
func TestRestartServiceRequiresAdmin(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystem, 1000)

	for _, tc := range []struct {
		name      string
		principal *auth.Principal
		want      int
	}{
		{"匿名", nil, http.StatusUnauthorized},
		{"普通用户", userPrincipal(), http.StatusForbidden},
	} {
		rec := serveRestartRoute(h, tc.principal)
		if rec.Code != tc.want {
			t.Fatalf("%s 请求重启应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	// 关键断言：越权路径连预检都不该跑，更不能排定任何重启。
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("越权请求不得触发重启，实际 %d 次：%v", len(calls), calls)
	}
	// 同一处理器在管理员身份下确实会受理：排除"越权用例只是因为端点整个不工作才通过"。
	if rec := serveRestartRoute(h, adminPrincipal()); rec.Code != http.StatusAccepted {
		t.Fatalf("管理员应受理（202），得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 判据不满足 → **200** + supported:false + reason + command 非空，且绝不触发重启。
// 这是降级契约的核心：降级是正常业务结果，不是错误码。
func TestRestartServiceDegradesWhenProbeFails(t *testing.T) {
	cases := []struct {
		name    string
		unit    string
		scope   string
		resolve func(string) error
		wantIn  []string
	}{
		{
			// argv[0] 指向的二进制被移走 / 没有可执行位：这是自替换唯一会提前失败的环节。
			name: "argv0 不是可执行文件", unit: "jianartifact", scope: domain.RestartScopeSystem,
			resolve: func(string) error { return errors.New("exec: no such file or directory") },
			wantIn:  []string{"无法自替换", "os.Args[0]"},
		},
		{
			name: "docker 作用域不支持自动重启", unit: "jianartifact", scope: domain.RestartScopeDocker,
			wantIn: []string{"docker"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRestartTriggerStub()
			svc := restartServiceForTest(stub, tc.unit, tc.scope, 1000, tc.resolve)
			h := NewHandlers(Deps{Restart: svc})

			rec := serveRestartRoute(h, adminPrincipal())
			if rec.Code != http.StatusOK {
				t.Fatalf("判据不满足应 200（降级是正常业务结果），得 %d：%s", rec.Code, rec.Body.String())
			}
			var result MaintenanceRestartResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatalf("解析降级结果：%v", err)
			}
			if result.Supported {
				t.Fatalf("判据不满足不得报 supported:true，得 %+v", result)
			}
			if result.Command == "" {
				t.Fatal("降级时 command 必须非空（前端要渲染可复制命令）")
			}
			if result.Reason == nil || *result.Reason == "" {
				t.Fatal("降级时 reason 必须非空（前端要说明原因）")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(*result.Reason, want) {
					t.Fatalf("reason 应说明原因（含 %q），得 %q", want, *result.Reason)
				}
			}
			// 原因必须是单行：系统错误里的换行会破坏前端排版。
			if strings.ContainsAny(*result.Reason, "\n\r") {
				t.Fatalf("reason 不得含换行，得 %q", *result.Reason)
			}
			if result.DelayMs != nil {
				t.Fatalf("未受理时不应给出 delayMs，得 %d", *result.DelayMs)
			}
			if calls := stub.calls(); len(calls) != 0 {
				t.Fatalf("降级路径绝不能触发重启，实际 %d 次：%v", len(calls), calls)
			}
		})
	}
}

// 配置非法（单元名含注入字符 / 作用域非法）→ 降级 200，且 command 绝不回显注入原文。
func TestRestartServiceRejectsInjectedUnitName(t *testing.T) {
	const injected = "jianartifact; rm -rf /"

	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, injected, domain.RestartScopeSystem, 0)

	rec := serveRestartRoute(h, adminPrincipal())
	if rec.Code != http.StatusOK {
		t.Fatalf("非法单元名应降级为 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析降级结果：%v", err)
	}
	if result.Supported {
		t.Fatalf("非法单元名不得受理，得 %+v", result)
	}
	if result.Reason == nil || *result.Reason == "" {
		t.Fatal("非法单元名应给出原因")
	}
	if result.Command == "" {
		t.Fatal("降级时 command 仍须非空")
	}
	// 注入防护仍是最关键的一条：command 是**给人复制到终端粘贴执行**的串，那一侧有 shell。
	for _, bad := range []string{"rm -rf", ";", "jianartifact;"} {
		if strings.Contains(result.Command, bad) {
			t.Fatalf("command 不得含注入内容 %q，得 %q", bad, result.Command)
		}
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("非法单元名绝不能触发重启，实际 %d 次：%v", len(calls), calls)
	}
}

// 可用 → 202 + supported:true + command/scope/delayMs 齐全；
// 且**响应必须先于重启动作**：handler 返回时尚未触发，延迟到期后才触发。
//
// 这是本功能的正面现场：自替换不需要任何权限，因此在任何部署形态下（用户级单元 /
// 系统级单元 / 加了 NoNewPrivileges 的加固单元）都应当受理。
func TestRestartServiceAcceptsAndTriggersAfterResponse(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystem, 1000)

	rec := serveRestartRoute(h, adminPrincipal())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("判据满足应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	responseAt := time.Now()

	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析受理结果：%v", err)
	}
	if !result.Supported {
		t.Fatalf("判据满足应 supported:true，得 %+v", result)
	}
	if result.Command == "" {
		t.Fatal("受理时 command 仍须非空（供审计与人工复现）")
	}
	// command 是**手工等效命令**，不是本进程将要执行的命令（本进程走自替换）。
	if want := "sudo -n systemctl restart jianartifact"; result.Command != want {
		t.Fatalf("受理时 command 应为手工等效命令 %q，得 %q", want, result.Command)
	}
	if result.Scope == nil || *result.Scope != domain.RestartScopeSystem {
		t.Fatalf("应回显作用域 system，得 %v", result.Scope)
	}
	if result.DelayMs == nil || *result.DelayMs <= 0 {
		t.Fatalf("受理时应给出正的 delayMs，得 %v", result.DelayMs)
	}

	// 顺序断言之一：**响应写出时重启动作尚未发生**（否则 202 根本发不出去）。
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("响应送出时不得已触发重启，实际 %d 次：%v", len(calls), calls)
	}
	// 顺序断言之二：延迟到期后真的触发了，且晚于响应。
	if !waitTrigger(t, stub) {
		t.Fatal("受理后重启动作未在延迟到期时触发")
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("应恰好触发一次，实际 %d 次：%v", len(calls), calls)
	}
	if !calls[0].After(responseAt) {
		t.Fatalf("重启动作必须发生在响应之后：响应 %s、触发 %s", responseAt, calls[0])
	}
}

// root 身份下 command 不带 sudo（进程本就有权管单元）。
func TestRestartServiceSkipsSudoWhenRoot(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystem, 0)

	rec := serveRestartRoute(h, adminPrincipal())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("root 下应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析受理结果：%v", err)
	}
	if want := "systemctl restart jianartifact"; result.Command != want {
		t.Fatalf("root 下 command 应为不带 sudo 的 %q，得 %q", want, result.Command)
	}
	if !waitTrigger(t, stub) {
		t.Fatal("受理后应触发重启")
	}
}

// user 级作用域：command 为 systemctl --user restart（仓库自带的部署形态）。
func TestRestartServiceSystemdUserScope(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystemdUser, 1000)

	rec := serveRestartRoute(h, adminPrincipal())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("systemd-user 作用域应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析受理结果：%v", err)
	}
	if want := "systemctl --user restart jianartifact"; result.Command != want {
		t.Fatalf("command 应为 %q，得 %q", want, result.Command)
	}
	if result.Scope == nil || *result.Scope != domain.RestartScopeSystemdUser {
		t.Fatalf("应回显作用域 systemd-user，得 %v", result.Scope)
	}
	if !waitTrigger(t, stub) {
		t.Fatal("受理后应触发重启")
	}
}

// 未装配重启能力时端点明确不可用（503），不静默返回假成功。
func TestRestartServiceUnavailableWithoutController(t *testing.T) {
	h := NewHandlers(Deps{})
	if rec := serveRestartRoute(h, adminPrincipal()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配时应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 审计：受理记 ok、降级记 failed，且都带上可复现的信息（命令 / 原因）。
func TestRestartServiceAuditsBothOutcomes(t *testing.T) {
	accepted := newRestartTriggerStub()
	auditLogs := repository.NewAuditLogRepo(openAPITestDB(t))
	h := NewHandlers(Deps{
		Restart:   restartServiceForTest(accepted, "jianartifact", domain.RestartScopeSystem, 0, nil),
		AuditLogs: auditLogs,
	})
	if rec := serveRestartRoute(h, adminPrincipal()); rec.Code != http.StatusAccepted {
		t.Fatalf("受理应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceServiceRestart})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("受理应写 1 条审计，实际 %d 条", len(entries))
	}
	if entries[0].Result != "ok" || entries[0].Actor != "admin" ||
		!strings.Contains(entries[0].Detail, "systemctl restart jianartifact") {
		t.Fatalf("受理审计内容不符：%+v", entries[0])
	}

	// 降级也要留痕：这是"为什么没重启"的唯一事后证据。
	auditLogs2 := repository.NewAuditLogRepo(openAPITestDB(t))
	h2 := NewHandlers(Deps{
		Restart:   restartServiceForTest(newRestartTriggerStub(), "bad unit", domain.RestartScopeSystem, 0, nil),
		AuditLogs: auditLogs2,
	})
	if rec := serveRestartRoute(h2, adminPrincipal()); rec.Code != http.StatusOK {
		t.Fatalf("降级应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	entries2, err := auditLogs2.List(repository.AuditFilter{Action: auditActionMaintenanceServiceRestart})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries2) != 1 || entries2[0].Result != "failed" || !strings.Contains(entries2[0].Detail, "受理=否") {
		t.Fatalf("降级审计内容不符：%+v", entries2)
	}
}

// 只读探测端点（GET /api/v1/maintenance/restart）：鉴权边界与 POST 完全一致
// （匿名 401 / 普通用户 403），且**越权请求连预检都不该跑**。
//
// 这个端点的存在理由本身就是一条安全约束：界面要拿它渲染可复制命令，
// 所以它必须可被"打开弹窗"这种无害动作反复调用——若它也会排定重启，
// 打开页面就等于重启服务。鉴权是这条约束的第一道门。
func TestGetRestartCapabilityRequiresAdmin(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystem, 1000)

	for _, tc := range []struct {
		name      string
		principal *auth.Principal
		want      int
	}{
		{"匿名", nil, http.StatusUnauthorized},
		{"普通用户", userPrincipal(), http.StatusForbidden},
	} {
		rec := serveRestartCapabilityRoute(h, tc.principal)
		if rec.Code != tc.want {
			t.Fatalf("%s 探测重启能力应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("越权请求不得触发重启，实际 %d 次：%v", len(calls), calls)
	}
	// 同一处理器在管理员身份下确实给出探测结果：排除"越权用例只是因为端点整个不工作才通过"。
	if rec := serveRestartCapabilityRoute(h, adminPrincipal()); rec.Code != http.StatusOK {
		t.Fatalf("管理员应拿到探测结果（200），得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 可用时的只读探测：200 + supported:true + command 非空，且**绝不触发重启**。
//
// "零次触发"是本端点最关键的不变量：探测端点一旦顺手排定重启，
// 就成了"打开页面即重启"的陷阱——界面为了显示一条命令而让服务重启，是绝对不可接受的。
// 这里刻意等待超过 restartDelay 再断言，以免"已经排定但还没到触发时刻"被误判为通过。
func TestGetRestartCapabilityDoesNotScheduleRestart(t *testing.T) {
	stub := newRestartTriggerStub()
	h := restartTestHandlers(stub, "jianartifact", domain.RestartScopeSystem, 1000)

	rec := serveRestartCapabilityRoute(h, adminPrincipal())
	if rec.Code != http.StatusOK {
		t.Fatalf("只读探测应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析探测结果：%v", err)
	}
	if !result.Supported {
		t.Fatalf("判据满足应 supported:true，得 %+v", result)
	}
	if result.Command == "" {
		t.Fatal("探测结果必须带非空 command（界面要拿它渲染可复制命令）")
	}
	if want := "sudo -n systemctl restart jianartifact"; result.Command != want {
		t.Fatalf("探测给出的命令应为手工等效命令 %q，得 %q", want, result.Command)
	}
	if result.Scope == nil || *result.Scope != domain.RestartScopeSystem {
		t.Fatalf("应回显作用域 system，得 %v", result.Scope)
	}
	// 没有受理就没有延迟：delayMs 必须缺省，否则前端会误以为重启已经在路上。
	if result.DelayMs != nil {
		t.Fatalf("只读探测不得给出 delayMs（没有受理就没有延迟），得 %d", *result.DelayMs)
	}

	// 核心不变量：等过 restartDelay 之后，仍然**一次触发都没有**。
	select {
	case <-stub.trigger:
		t.Fatal("只读探测排定了重启：探测端点绝不能触发重启（否则打开页面即重启）")
	case <-time.After(restartWaitTrigger):
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("只读探测必须零次触发，实际 %d 次：%v", len(calls), calls)
	}
}

// 不可用（判据不满足）时的只读探测：200 + supported:false + reason + command 非空，
// 同样零次触发——降级路径更不该有任何执行动作。
func TestGetRestartCapabilityReportsDegradedWithoutTrigger(t *testing.T) {
	cases := []struct {
		name    string
		unit    string
		scope   string
		resolve func(string) error
		wantIn  []string
	}{
		{
			// 本机现场：argv[0] 指向的二进制不可执行（部署切换中途 / 权限不对）。
			name: "argv0 不可执行", unit: "jianartifact", scope: domain.RestartScopeSystem,
			resolve: func(string) error { return errors.New("permission denied") },
			wantIn:  []string{"无法自替换", "permission denied"},
		},
		{
			name: "docker 作用域不支持自动重启", unit: "jianartifact", scope: domain.RestartScopeDocker,
			wantIn: []string{"docker"},
		},
		{
			name: "非法单元名", unit: "jianartifact; rm -rf /", scope: domain.RestartScopeSystem,
			wantIn: []string{"非法字符"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRestartTriggerStub()
			svc := restartServiceForTest(stub, tc.unit, tc.scope, 1000, tc.resolve)
			h := NewHandlers(Deps{Restart: svc})

			rec := serveRestartCapabilityRoute(h, adminPrincipal())
			if rec.Code != http.StatusOK {
				t.Fatalf("判据不满足应 200（降级是正常业务结果），得 %d：%s", rec.Code, rec.Body.String())
			}
			var result MaintenanceRestartResult
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatalf("解析探测结果：%v", err)
			}
			if result.Supported {
				t.Fatalf("判据不满足不得报 supported:true，得 %+v", result)
			}
			if result.Command == "" {
				t.Fatal("降级时 command 必须非空（界面要渲染可复制命令）")
			}
			if result.Reason == nil || *result.Reason == "" {
				t.Fatal("降级时 reason 必须非空（界面要说明原因）")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(*result.Reason, want) {
					t.Fatalf("reason 应说明原因（含 %q），得 %q", want, *result.Reason)
				}
			}
			if result.DelayMs != nil {
				t.Fatalf("未受理时不应给出 delayMs，得 %d", *result.DelayMs)
			}
			if calls := stub.calls(); len(calls) != 0 {
				t.Fatalf("只读探测绝不能触发重启，实际 %d 次：%v", len(calls), calls)
			}
		})
	}
}

// 只读探测必须带上 argv[0] 的判据：即使平台与触发点都就绪，
// argv[0] 不可执行时也必须如实降级——否则界面会显示"支持自动重启"而实际重启不了。
func TestGetRestartCapabilityRejectsUnresolvableArgv0(t *testing.T) {
	stub := newRestartTriggerStub()
	svc := restartServiceForTest(stub, "jianartifact", domain.RestartScopeSystem, 1000,
		func(string) error { return errors.New("exec: no such file or directory") })
	h := NewHandlers(Deps{Restart: svc})

	rec := serveRestartCapabilityRoute(h, adminPrincipal())
	if rec.Code != http.StatusOK {
		t.Fatalf("只读探测应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceRestartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析探测结果：%v", err)
	}
	if result.Supported {
		t.Fatalf("argv[0] 不可执行时必须降级，得 %+v", result)
	}
	// 原因要能定位到具体是哪条路径，否则运维只能猜。
	if result.Reason == nil || !strings.Contains(*result.Reason, "os.Args[0]") {
		t.Fatalf("reason 应点名 os.Args[0]，得 %v", result.Reason)
	}
	if calls := stub.calls(); len(calls) != 0 {
		t.Fatalf("只读探测必须零次触发，实际 %d 次：%v", len(calls), calls)
	}
}

// 只读探测**不写审计**：读操作不改变任何状态，与 ListMaintenanceJobs 一致。
// 若这里写审计，前端每次打开弹窗都会在审计库里留一条记录，真正的重启受理反而被淹没。
func TestGetRestartCapabilityWritesNoAudit(t *testing.T) {
	auditLogs := repository.NewAuditLogRepo(openAPITestDB(t))
	h := NewHandlers(Deps{
		Restart:   restartServiceForTest(newRestartTriggerStub(), "jianartifact", domain.RestartScopeSystem, 0, nil),
		AuditLogs: auditLogs,
	})

	if rec := serveRestartCapabilityRoute(h, adminPrincipal()); rec.Code != http.StatusOK {
		t.Fatalf("只读探测应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceServiceRestart})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("只读探测不得写审计，实际 %d 条：%+v", len(entries), entries)
	}
}

// restartRecordingController 包一层真实领域服务，单独记下 RestartAfter 被调用过几次。
//
// 为什么需要它（变异验证实测的教训）：只读探测端点的"零次重启"若只用触发点观察窗来验，
// 就得等过 restartDelay（1.8 秒）才抓得住"已排定但还没到时刻"——三个降级用例累计 12 秒，
// 代价过高，而且它验的是"延迟之后确实没触发"这种**间接**证据。
// 直接记录 RestartAfter 是否被调用，得到的是一条与时间无关的**直接**不变量：
// 只读端点在任何结论下都不得排定重启，无论延迟多长、平台是否支持。
type restartRecordingController struct {
	inner             *domain.RestartService
	restartAfterCalls int
	probeCalls        int
}

func (c *restartRecordingController) Command() string { return c.inner.Command() }
func (c *restartRecordingController) Scope() string   { return c.inner.Scope() }

func (c *restartRecordingController) Probe() domain.RestartPlan {
	c.probeCalls++
	return c.inner.Probe()
}

func (c *restartRecordingController) RestartAfter(plan domain.RestartPlan, delay time.Duration) {
	c.restartAfterCalls++
	c.inner.RestartAfter(plan, delay)
}

// 只读探测端点**在任何结论下都不得调用 RestartAfter**（打开页面即重启的唯一防线）。
//
// 两种结论都覆盖：可用（supported:true）与降级（supported:false）。降级那一侧尤其要说清——
// 它虽会被 RestartAfter 自身的纵深防御拒绝，但"调用过"本身就已越界：
// 只读端点一旦开始依赖执行侧兜底，将来任何一次执行侧放松都会直接变成"打开页面即重启"。
func TestGetRestartCapabilityNeverCallsRestartAfter(t *testing.T) {
	cases := []struct {
		name    string
		unit    string
		scope   string
		resolve func(string) error
	}{
		{name: "可用", unit: "jianartifact", scope: domain.RestartScopeSystem},
		{name: "降级：argv0 不可执行", unit: "jianartifact", scope: domain.RestartScopeSystem,
			resolve: func(string) error { return errors.New("no such file") }},
		{name: "降级：docker 作用域", unit: "jianartifact", scope: domain.RestartScopeDocker},
		{name: "降级：非法单元名", unit: "bad unit", scope: domain.RestartScopeSystem},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRestartTriggerStub()
			rec := &restartRecordingController{
				inner: restartServiceForTest(stub, tc.unit, tc.scope, 1000, tc.resolve),
			}
			h := NewHandlers(Deps{Restart: rec})

			if code := serveRestartCapabilityRoute(h, adminPrincipal()).Code; code != http.StatusOK {
				t.Fatalf("只读探测应 200，得 %d", code)
			}
			// 结论是什么不重要：重要的是**一次都没排定过**。
			if rec.probeCalls != 1 {
				t.Fatalf("应恰好预检一次，实际 %d 次", rec.probeCalls)
			}
			if rec.restartAfterCalls != 0 {
				t.Fatalf("只读探测绝不得调用 RestartAfter，实际 %d 次（打开页面即重启）", rec.restartAfterCalls)
			}
			if calls := stub.calls(); len(calls) != 0 {
				t.Fatalf("只读探测必须零次触发，实际 %d 次：%v", len(calls), calls)
			}
		})
	}
}

// 未装配重启能力时只读探测同样明确不可用（503），不静默返回假成功。
func TestGetRestartCapabilityUnavailableWithoutController(t *testing.T) {
	h := NewHandlers(Deps{})
	if rec := serveRestartCapabilityRoute(h, adminPrincipal()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配时应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
}
