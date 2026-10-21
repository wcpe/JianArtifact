//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// fileOwnerUID 取文件的属主 uid。
//
// 判定"用 sudo 跑了 CLI"只需要文件 uid 与进程 euid 两个数，不需要 gid 或用户名——
// 服务到底以哪个用户运行，CLI 无从得知，去猜只会误报。
//
// 见 backup_cmd.go 的 warnIfMarkerOwnerDiffers：这条检查是为了在**问题产生的当场**
// 就提醒运维改属主，而不是等到下次重启才发现服务起不来。
func fileOwnerUID(info fs.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
