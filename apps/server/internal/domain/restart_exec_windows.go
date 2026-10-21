//go:build windows

package domain

import "errors"

// selfReplaceSupported 在 Windows 上恒为 false。
//
// syscall.Exec 是 Unix 专有接口，Windows 没有"用新映像替换当前进程"这种系统调用
// （CreateProcess 只能起新进程，起完当前进程还得自己退出，那是"重启"而不是"自替换"）。
// 因此本平台的正确行为不是硬凑一条路径，而是**如实报告不可用**：
// Probe 会回 supported:false 并给出手工等效命令，绝不假装成功。
//
// 影响范围仅限开发机（`go build ./...` 与单测要能在 Windows 上跑）与"把 Windows 当部署机"
// 这一非目标形态；正式部署是 Linux，selfReplaceSupported 为 true。
const selfReplaceSupported = false

// errSelfReplaceUnsupported 是"本平台不支持自替换"的错误。
var errSelfReplaceUnsupported = errors.New("本平台不支持进程自替换：syscall.Exec 是 Unix 专有接口")

// selfReplace 在 Windows 上不可用；返回错误而不是 panic 或静默返回 nil——
// 静默成功最坏：调用方会以为映像已被替换，接着走到"重启后的收尾"，行为完全不可预期。
func selfReplace(string, []string, []string) error {
	return errSelfReplaceUnsupported
}
