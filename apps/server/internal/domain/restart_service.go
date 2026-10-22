package domain

import (
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 一键重启的作用域。
//
// 语义已经改变（本文件重写的原因）：作用域**不再决定本进程将要执行哪条命令**，
// 只决定"若你想手工重启本节点，该敲哪条命令"（见 Command 的说明）。
// 真正的执行路径是**进程自替换**（syscall.Exec 换新映像），不执行任何外部命令。
const (
	// RestartScopeSystem 走系统级 systemd（手工命令为 systemctl restart，非 root 时带 sudo -n）。
	RestartScopeSystem = "system"
	// RestartScopeSystemdUser 走用户级 systemd（手工命令为 systemctl --user restart）。
	RestartScopeSystemdUser = "systemd-user"
	// RestartScopeDocker 表示本节点跑在容器里（手工命令为 docker restart）。
	RestartScopeDocker = "docker"
)

const (
	// restartReasonMaxLen 是降级原因的长度上限（按 rune 截断）。
	// 原因会进响应体与审计表，必须有上限：它可能带进 argv[0] 与系统错误原文。
	restartReasonMaxLen = 160
)

// restartUnitPattern 是单元名的合法字符集：只允许 [A-Za-z0-9._@-]。
//
// 为什么必须校验（自替换之后仍然需要）：单元名会拼进 `command` 字段——那条**手工等效命令**是
// 给运维**复制到终端粘贴执行**的，那一侧是真的有 shell。因此单元名只要含 `;`、`$()`、
// 空格、引号等字符，就会从"展示文本"变成"注入载荷"。
// 合法字符集覆盖 systemd 单元名的全部常见形态（名字、`.service` 后缀、`@` 实例分隔、`.` 与 `_`），
// 且不含任何 shell 元字符。
//
// 注意它**保护的不是自替换本身**：自替换用的是本进程自己的 os.Args，与单元名毫无关系，
// 单元名在这里纯粹是"手工命令的展示素材"。
var restartUnitPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// RestartTrigger 是"可以开始重启了"的触发点，由装配层（main）注入。
//
// 为什么让域层只拿一个函数、而不自己跑去 syscall.Exec：自替换之前**必须先优雅关停
// HTTP / TLS 服务**（见 main 的关停顺序与理由），而"关停哪些监听器、怎么收尾"是 main 的
// 编排知识，不是领域规则。域层只负责"延迟多久、在什么条件下触发"，动作留给调用方——
// 顺带让这条顺序契约可被单测用桩验证（否则测试要么真把测试进程 Exec 掉，要么只能测个假的）。
type RestartTrigger func()

// RestartHooks 是构造 RestartService 时可替换的探测原语（nil 表示用真实实现）。
//
// 导出它的理由：自替换一旦真的执行，测试进程就没了，因此"判据对不对、顺序对不对"
// 只能靠注入桩来验；而测试机（Windows 开发机 / CI）也不保证能自替换。
type RestartHooks struct {
	// Argv0 返回进程的 argv[0]（生产实现恒为 os.Args[0]，理由见 restart_exec.go）。
	Argv0 func() string
	// Resolve 判定一个路径能否作为可执行文件启动（默认 exec.LookPath）。
	Resolve func(string) error
	// EUID 返回当前进程的 euid（默认 os.Geteuid；Windows 恒为 -1）。
	EUID func() int
	// PlatformSupported 返回本平台能否自替换（默认 = selfReplaceSupported 编译期常量）。
	//
	// 之所以留这个注入点：平台判据是**编译期**决定的，若不让它可替换，
	// "argv[0] 解析不了要降级"这类与平台无关的判据在 Windows 开发机上就永远走不到
	// （平台检查排在前面、直接短路），于是那条逻辑在本地测不出来、只能等 Linux CI 才发现。
	// 真实默认值由 TestRestartServiceRejectsUnsupportedPlatform 单独盯着常量本身。
	PlatformSupported func() bool
	// Trigger 是"该重启了"的触发点；生产接线由 main 经 SetTrigger 注入（见其说明）。
	Trigger RestartTrigger
}

// RestartService 是「一键重启本节点」的领域服务。
//
// 两项职责（与旧实现相比，"执行什么"整个换掉了）：
//   - Probe：同步判据——本节点能否**自替换**（平台支持 / `os.Args[0]` 可解析为可执行文件 /
//     触发点已接线），把"能不能重启"变成**响应之前**就能给出的确定答案；
//   - RestartAfter：在调用方送出响应**之后**延迟调用触发点，由调用方完成"优雅关停 + 自替换"。
//
// 并发安全：构造后除 trigger 外字段只读；trigger 由 SetTrigger 在启动期写入一次，
// 用互斥锁保护，使"启动期注入"与"请求期读取"之间没有数据竞争。
type RestartService struct {
	unit  string // 目标单元名；只来自环境变量，绝不接受请求体输入（用于生成手工命令）
	scope string // 作用域：system / systemd-user / docker

	argv0             func() string
	resolve           func(string) error
	euid              func() int
	platformSupported func() bool

	// triggerMu 保护 trigger：启动期写一次、请求期读多次。
	triggerMu sync.RWMutex
	trigger   RestartTrigger
}

// NewRestartService 构造一键重启服务；判据使用真实系统原语。
//
// 触发点**尚未注入**（由 main 在启动 HTTP 服务之前调 SetTrigger），
// 因此在此之前 Probe 会如实回 supported:false——这比"假装可用然后什么都不做"正确得多。
func NewRestartService(unit, scope string) *RestartService {
	return NewRestartServiceWithHooks(unit, scope, RestartHooks{})
}

// NewRestartServiceWithHooks 同上，但可替换判据原语与触发点（nil 表示用真实实现）。
//
// 导出它的理由：本功能最核心的契约是"响应先于动作"的顺序，而这条只有把真实 RestartService
// 接到 HTTP 边界上才测得准（用桩替掉整个 RestartService 就等于只测了桩自己的延迟）；
// 同时真实的 syscall.Exec 会把测试进程换掉，必须能注入桩。
func NewRestartServiceWithHooks(unit, scope string, hooks RestartHooks) *RestartService {
	svc := &RestartService{
		unit:              unit,
		scope:             scope,
		argv0:             hooks.Argv0,
		resolve:           hooks.Resolve,
		euid:              hooks.EUID,
		platformSupported: hooks.PlatformSupported,
		trigger:           hooks.Trigger,
	}
	if svc.argv0 == nil {
		svc.argv0 = restartSelfArgv0
	}
	if svc.resolve == nil {
		svc.resolve = resolveExecutable
	}
	if svc.platformSupported == nil {
		svc.platformSupported = func() bool { return selfReplaceSupported }
	}
	if svc.euid == nil {
		// os.Geteuid 在 Windows 上恒返回 -1（不等于 0），于是手工命令会带上 sudo：
		// 该命令只用于展示"在 Linux 部署机上怎么手工重启"，Windows 上本就不支持自替换。
		svc.euid = os.Geteuid
	}
	return svc
}

// SetTrigger 注入"该重启了"的触发点。
//
// 生产接线：main 在启动 HTTP 监听之前调用一次，把请求转成一个通道信号；
// main 的 select 收到信号后**先优雅关停、再自替换**（顺序见 main 里的说明）。
// 之所以由 main 注入而不是域层自己 Exec：优雅关停是 main 的编排知识（它才知道有哪些监听器）。
//
// 未注入时 Probe 一律回 supported:false：域层无法自己完成重启，
// 因此必须如实降级，绝不能受理一个自己做不到的请求。
func (s *RestartService) SetTrigger(trigger RestartTrigger) {
	s.triggerMu.Lock()
	s.trigger = trigger
	s.triggerMu.Unlock()
}

// currentTrigger 读取已注入的触发点；未注入返回 nil。
func (s *RestartService) currentTrigger() RestartTrigger {
	s.triggerMu.RLock()
	defer s.triggerMu.RUnlock()
	return s.trigger
}

// Scope 返回配置的作用域，供契约回显与审计使用。
func (s *RestartService) Scope() string { return s.scope }

// Command 返回**手工重启**的等效命令串（不预检，任何情况下都非空）。
//
// 语义边界（很关键，别再读错）：**它不是本进程将要执行的命令**。
// 自替换不执行任何外部命令——本进程直接把新映像换进来（见 restart_exec.go 的理由）。
// 本方法给出的是"若你想在终端里手工重启本节点，敲这条"，因此它随部署形态而变。
//
// 为什么契约里还要保留这样一个字段：一键重启的最高权限动作是**重启**，
// 运维必须有一条"不依赖本功能也能达到同样效果"的路径——接口降级时它是唯一出路，
// 受理成功时它是审计与事后复现的依据。所以它必须始终可复制、且与当前部署形态一致。
//
// 配置不可用时返回一句说明性**注释**：既满足"永远有东西可复制"，
// 又绝不把非法单元名原样拼进命令串——那等于把一处配置错误变成一段可粘贴的注入载荷。
func (s *RestartService) Command() string {
	if !restartUnitPattern.MatchString(s.unit) || !validRestartScope(s.scope) {
		return "# 无法生成重启命令：" + restartConfigProblem(s.unit, s.scope)
	}
	switch s.scope {
	case RestartScopeSystem:
		if s.isRoot() {
			// 进程本身就是 root（容器 / root 直启的部署形态很常见），不需要 sudo。
			return "systemctl restart " + s.unit
		}
		// `-n` 是给运维看的：手工执行时不要停在密码提示上，与文档口径一致。
		return "sudo -n systemctl restart " + s.unit
	case RestartScopeSystemdUser:
		return "systemctl --user restart " + s.unit
	default: // RestartScopeDocker（validRestartScope 已排除其余取值）
		return "docker restart " + s.unit
	}
}

// RestartPlan 是一次预检的结论，也是本功能展示与执行的**唯一真源**。
//
// 与旧实现的差别：旧结论里带着"选中的那条外部命令"（多条候选路径靠预检挑一条），
// 现在只有一条执行路径（自替换），因此计划里不再有命令，只有一个"已就绪"标记。
// 保留这层结构仍然是必要的——它让"预检说不可用、执行却照做"在类型上不可能：
// armed 不导出，零值 RestartPlan 一律拒绝触发（见 RestartAfter）。
type RestartPlan struct {
	// Supported 表示本节点能否自替换（平台支持 + argv[0] 可解析 + 触发点已接线）。
	Supported bool
	// Reason 说明不可用的原因；Supported 为真时为空。
	Reason string
	// Command 是**手工重启**的等效命令串（自替换不执行它）；Supported 为假时是降级出路。
	Command string

	// armed 是"本次预检确认可以自替换"的内部标记。刻意不导出：外部没有任何合法理由
	// 凭空构造一个"已预检"的结论塞进来——那会让 RestartAfter 在一个从未验证过的状态下触发重启。
	armed bool
}

// Probe 同步预检本节点能否自替换，返回本次结论。
//
// 判据三条（全部满足才算可用）：
//  1. 平台支持自替换（Unix 的 syscall.Exec；Windows 恒不支持，见 restart_exec_windows.go）；
//  2. `os.Args[0]` 能解析到一个可执行文件——它是自替换唯一要加载的东西，
//     解析不了就必然失败。注意它可能是相对路径或含符号链接：这里只**如实判断**，
//     既不替调用方改写路径（改写会破坏符号链接语义，见 restart_exec.go），也不假装成功；
//  3. 触发点已接线——域层自己不执行重启，没有触发点就等于做不到。
//
// 为什么在请求期同步判定，而不是"先受理、让后台失败"：后台失败对调用方是**不可观测**的——
// 进程马上就会重启、响应早已发出，前端只看到连接断开，无法区分"重启成功"与"根本没重启成"。
// 因此把可用性判断前移到响应之前：判据不过就如实回 supported:false + 原因 + 可手工执行的命令。
// 本方法**绝不**因为判据不满足而返回错误：那是正常的业务结果，不是异常。
func (s *RestartService) Probe() RestartPlan {
	if !restartUnitPattern.MatchString(s.unit) || !validRestartScope(s.scope) {
		return RestartPlan{
			Command: s.Command(),
			Reason:  restartTruncate("重启配置不可用："+restartConfigProblem(s.unit, s.scope), restartReasonMaxLen),
		}
	}
	if s.scope == RestartScopeDocker {
		// 容器内进程重启不了自己所在的容器：容器的生命周期由外部编排（compose / k8s）掌握，
		// 容器里既没有 systemctl，`docker restart` 也需要宿主机的守护进程（通常未挂载进容器）。
		// 自替换在这里也帮不上忙：替换掉的只是容器内的一个进程，编排层该做的重建不会被触发。
		return RestartPlan{
			Command: s.Command(),
			Reason:  "docker 作用域不支持自动重启：容器生命周期由外部编排掌握，请在宿主机执行 command 中的命令",
		}
	}
	if !s.platformSupported() {
		return RestartPlan{
			Command: s.Command(),
			Reason:  "本平台不支持进程自替换（syscall.Exec 仅 Unix 可用）：请在部署机上手工执行 command 中的命令",
		}
	}
	argv0 := s.argv0()
	if err := s.resolve(argv0); err != nil {
		// 如实说明是哪条路径解析不了、以及为什么——这是"重启没生效"最常见的根因
		// （二进制被换掉 / 部署目录的符号链接悬空 / 权限不对），必须让人一眼看到。
		return RestartPlan{
			Command: s.Command(),
			Reason: restartTruncate(
				"无法自替换：os.Args[0]（"+argv0+"）不是可执行文件："+err.Error(), restartReasonMaxLen),
		}
	}
	if s.currentTrigger() == nil {
		// 触发点未接线（理论上只在 CLI 子命令这类不提供 HTTP 服务的装配里出现）。
		return RestartPlan{
			Command: s.Command(),
			Reason:  "重启触发点未接线：本进程未启用一键重启",
		}
	}
	return RestartPlan{Supported: true, Command: s.Command(), armed: true}
}

// RestartAfter 在 delay 之后调用触发点（重启动作的起点），**立即返回**（不阻塞调用方）。
//
// 顺序契约（本功能的核心）：调用方必须先把响应送出去，再让重启落地。
// 重启会关停服务并替换本进程的映像，若先重启，响应根本发不出去，前端只能看到连接被重置——
// 既分不清"重启成功"还是"网络断了"，也拿不到 supported / command 这些降级信息。
// 因此这里只起一个后台 goroutine 睡够 delay 再调触发点，把响应送达的时间窗留给调用方。
//
// 为什么只能由传进来的 plan 决定是否触发：本方法在响应之后才执行，"再判一次"意味着
// 响应里说的与后台做的不再同源——判据刚被环境改过（二进制被挪走、触发点被换掉）就会出现
// "回了 202 却什么都不做"或反之。因此触发侧不接受任何"再决定一次"的余地。
//
// 未通过预检的 plan 一律不触发（含零值 RestartPlan 与手工构造的 Supported:true）：
// 调用方本就不该走到这里（handler 先 Probe 再决定是否受理），这里是纵深防御。
func (s *RestartService) RestartAfter(plan RestartPlan, delay time.Duration) {
	trigger := s.currentTrigger()
	if !plan.Supported || !plan.armed || trigger == nil {
		log.Printf("ERROR 一键重启未触发：未通过预检（%s）", plan.Reason)
		return
	}
	log.Printf("INFO 一键重启已排定：%s 后优雅关停并自替换（argv[0]=%s）", delay, s.argv0())
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		// 只调触发点，不在这里做关停与 Exec：关停是本进程编排的一部分（见 RestartTrigger 的说明）。
		// 触发之后本函数不再返回任何结果——重启的成败由日志与"服务是否回来"体现，
		// 此时响应早已发出，没有任何调用方还在等这个结果。
		trigger()
	}()
}

// isRoot 判断当前进程是否以 root 运行（只用于生成手工命令的 sudo 前缀）。
func (s *RestartService) isRoot() bool { return s.euid() == 0 }

// validRestartScope 判断作用域取值是否合法。
func validRestartScope(scope string) bool {
	switch scope {
	case RestartScopeSystem, RestartScopeSystemdUser, RestartScopeDocker:
		return true
	default:
		return false
	}
}

// restartConfigProblem 用一句话说明配置为什么不可用。
// 刻意**不回显**配置原文：它可能含控制字符或换行，回显到界面 / 审计里只会帮倒忙。
func restartConfigProblem(unit, scope string) string {
	if !restartUnitPattern.MatchString(unit) {
		return "配置的单元名含非法字符（只允许 A-Za-z0-9._@-）"
	}
	if !validRestartScope(scope) {
		return "配置的作用域取值非法（只允许 system / systemd-user / docker）"
	}
	return "配置不可用"
}

// restartTruncate 把一段文本压平（换行与连续空白压成单个空格）并按 rune 截断。
//
// 压平是必须的：系统错误与路径都可能带换行，原样进响应会破坏前端排版（原因字段是单行文案）。
// 按 rune 截断则不会切断多字节字符。上限本身也是硬要求——原因会进响应体与审计表。
func restartTruncate(text string, max int) string {
	msg := strings.Join(strings.Fields(text), " ")
	runes := []rune(msg)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return msg
}

// 编译期断言：默认构造下 resolveExecutable 必须满足 RestartHooks.Resolve 的签名，
// 避免有人改了其中一处后另一处静默不匹配（两处都由本包维护，但签名耦合不该靠人记）。
var _ func(string) error = resolveExecutable
