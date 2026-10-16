//go:build !windows

package domain

import (
	"path/filepath"
	"strings"
	"testing"
)

// Unix 侧的平台判据：selfReplaceSupported 必须为 true（部署形态就是 Linux）。
//
// 与 restart_exec_windows_test.go 配对，靠 build tag 保证两侧各在对应平台上生效。
func TestSelfReplaceSupportedOnUnix(t *testing.T) {
	if !selfReplaceSupported {
		t.Fatal("Unix 上 selfReplaceSupported 必须为 true（syscall.Exec 可用）")
	}
}

// 自替换失败时必须**返回错误**，绝不能静默返回 nil。
//
// 为什么这条值得单独测：main 依赖返回值来决定"报错退出"还是"防御性返回"。
// 若实现退化成"什么都不做也不报错"，症状是**最坏的一种**——接口回了 202、
// 进程仍在跑、日志里连一句失败都没有，管理员会以为重启成功。
//
// 用一个必定不存在的路径触发真实的 syscall.Exec 失败：Exec 失败时进程映像**不变**，
// 因此本调用可安全地在本测试进程内执行（成功才会换掉映像，而不存在的路径不可能成功）。
func TestSelfReplaceReturnsErrorOnFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here", "jianartifact")

	err := selfReplace(missing, []string{missing, "run"}, nil)
	if err == nil {
		t.Fatal("路径不存在时 selfReplace 必须返回错误，实际返回 nil（调用方会误以为映像已替换）")
	}
	if !strings.Contains(err.Error(), missing) && !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("错误应可诊断（含路径或底层原因），得 %q", err.Error())
	}
}
