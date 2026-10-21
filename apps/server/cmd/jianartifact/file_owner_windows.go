//go:build windows

package main

import "io/fs"

// fileOwnerUID 在 Windows 上不提供属主判定。
//
// Windows 的 ACL 模型与 Unix 的 uid 不是一回事（没有可比的 euid 语义），强行映射只会
// 得出错误的"同/不同属主"结论。返回 ok=false 让调用方静默跳过该提示——
// 本检查是**锦上添花的即时提醒**，不是正确性依赖，缺了它不影响导入本身。
func fileOwnerUID(fs.FileInfo) (uint32, bool) {
	return 0, false
}
