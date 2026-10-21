//go:build !windows

package domain

import "syscall"

// selfReplaceSupported 表示本平台支持"进程自替换"。
//
// 判据不是"能不能调通某个命令"，而是"这个平台有没有 syscall.Exec"：
// Unix（Linux / macOS / BSD）都有，因此部署形态（rootless 用户级单元 / 系统级单元 / 容器）
// 一律可用——自替换不需要任何额外权限，也就没有任何形态能拦住它。
const selfReplaceSupported = true

// selfReplace 用新映像替换当前进程的映像（Unix 专有的 syscall.Exec）。
//
// 语义要点（与 systemd 的 `systemctl restart` 不同，务必分清）：
//   - **不 fork**：PID 不变（实测 27271 → 27271），PPID 仍是 1，
//     在 systemd 看来这就是同一个进程、同一次启动，因此单元状态与重启计数都不会变；
//   - **环境与参数由调用方给全**：Exec 不会替调用方补任何东西，argv / env 全量传入；
//   - **成功即不返回**：当前 goroutine 所在的整个进程映像已被接管，
//     旧的 Go 运行时、所有 goroutine 与已打开的 fd 都不复存在。
//     关于 fd：Go 打开的文件默认带 O_CLOEXEC，内核在 exec 时自动关闭，
//     因此"故意不关监听器"也不会让新映像绑不上端口（实测在服务全套加固下可原样重绑）。
func selfReplace(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}
