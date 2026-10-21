package domain

import (
	"os"
	"os/exec"
)

// 本文件是「进程自替换」跨平台部分的共享实现。
//
// 为什么一键重启的正统做法是"自替换"，而不是 systemctl / sudo / polkit：
// jianartifact 本就有权执行自己的二进制（systemd 的 ExecStart 就是它），
// syscall.Exec 只是"同一个进程换一份新映像"，**不引入任何新权限**。
// 因此 NoNewPrivileges=true、polkit、sudoers 这些提权路径上的门全都绕开了——
// 它们被绕开不是因为被绕过，而是因为压根不需要（本进程执行自己的映像从来不需要授权）。
//
// 反过来，调外部命令重启（`sudo -n systemctl restart` / 裸 `systemctl restart` 走 polkit）
// 在"系统级单元 + User=<非 root> + NoNewPrivileges=true"这一形态下两条路都会被拦住
// （前者 sudo 直接报 no new privileges flag，后者 polkit 要求交互式认证而无 TTY），
// 只能靠一条非常规的 polkit 规则救场——那等于为了重启自己而把该用户的单元管理权限放大。
//
// 两个必须记住的语义差异（运维视角）：
//  1. **单元状态不变**：systemd 视角上进程从未退出，ActiveEnterTimestamp / NRestart 都不动，
//     `systemctl status` 看不出重启过。这是可以接受的：本功能要的是"应用重新执行启动流程"
//     （备份恢复落地就靠这条），而不是"让 systemd 记一笔重启"。
//  2. 需要一个**短暂的中断窗口**：先优雅关停（不再接新请求）再替换映像，期间服务不可用。

// restartSelfArgv0 返回自替换要加载的程序路径：**必须是 os.Args[0]**，绝不能用 os.Executable()。
//
// 这是本功能最容易被"顺手改错"的一处，请按实测结论理解。部署脚本用 `current` 符号链接
// 指向具体 release 目录（/opt/jianartifact/current → releases/<时间戳>），
// systemd 的 ExecStart 给的正是 `/opt/jianartifact/current/jianartifact run`，于是实测结果是：
//
//	argv[0]（systemd 传的） = /opt/jianartifact/current/jianartifact  → 符号链接会被**重新解析**，加载新版本
//	os.Executable()         = /proc/self/exe 的内容 = releases/<旧时间戳>/jianartifact → 旧版本
//
// 探针实测：把 `current` 从 v1 切到 v2 后自替换，用 argv[0] 加载到 v2，用 /proc/self/exe 加载到 v1。
// 也就是说改成 os.Executable() 会让"部署切换后重启"**等于没有切换**——旧映像重新跑一遍，
// 部署脚本刚做的一切（含待生效的备份恢复所依赖的新代码）全部落空，且没有任何报错。
func restartSelfArgv0() string { return os.Args[0] }

// restartSelfInvocation 组装自替换的完整调用参数（程序路径 + 参数表 + 环境）。
//
// 抽成函数是为了让"argv0 到底取自哪里"能被单测直接断言（见 restart_exec_test.go）：
// 这条语义在这台开发机上永远看不出问题，只有到 Linux 部署机、且部署目录用了符号链接时
// 才会以"重启后仍是旧版本"的形式静默发作。
func restartSelfInvocation() (string, []string, []string) {
	return restartSelfArgv0(), os.Args, os.Environ()
}

// ReplaceSelf 用当前进程的映像与参数自替换（Unix 的 syscall.Exec）。
//
// 唯一的调用点是 main 的「一键重启」分支，且在**优雅关停之后**：先停掉 HTTP / TLS 不再接新请求，
// 再替换映像。反过来做的话，新旧映像会短暂同时持有同一端口（新映像绑不上）与同一份 SQLite，
// 而且旧映像还在服务的在途请求会随着 Exec 一起消失，没有任何收尾。
//
// 三个参数全部来自本进程自身，不引入任何外部输入（因而不存在注入面）：
//   - argv0 = os.Args[0]（理由见 restartSelfArgv0）；
//   - argv  = os.Args（新映像的命令行与旧进程逐字一致，systemd 视角上就是同一个进程）；
//   - env   = os.Environ()（原样透传，不增不减）。
//
// 成功时**不会返回**（当前进程的映像已被新映像接管）；失败返回可诊断的错误，
// 调用方必须把它报出来，而不是当作"重启过了"。
func ReplaceSelf() error {
	argv0, argv, env := restartSelfInvocation()
	return selfReplace(argv0, argv, env)
}

// resolveExecutable 判定一个路径能否作为可执行文件启动，交给 exec.LookPath。
//
// 选它的三个理由：os.Args[0] 可能只是裸命令名（靠 PATH 找得到就是可执行的），
// 它会对不存在 / 不是可执行位的路径如实报错，而且**不解析符号链接**（返回原样路径）——
// 恰好与自替换时要传给 Exec 的 argv[0] 语义一致（符号链接必须在 Exec 时由内核解析）。
func resolveExecutable(path string) error {
	_, err := exec.LookPath(path)
	return err
}
