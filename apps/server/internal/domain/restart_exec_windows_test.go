//go:build windows

package domain

import "testing"

// Windows 上"自替换不可用"必须是**如实报告**，而不是悄悄走一条别的路径。
//
// 本文件靠 build tag 与实现文件配对（restart_exec_unix_test.go 是另一侧）：
// 编译到 Windows 时才跑，因此这两条断言在 Linux 部署机上不会被误认为"已经验过了"。
func TestSelfReplaceUnavailableOnWindows(t *testing.T) {
	if selfReplaceSupported {
		t.Fatal("Windows 上 selfReplaceSupported 必须为 false（syscall.Exec 是 Unix 专有接口）")
	}
	// 调用必须返回错误：静默返回 nil 最坏——调用方会以为映像已被替换，
	// 接着走到"重启后的收尾"，行为完全不可预期。
	if err := selfReplace("whatever", []string{"whatever"}, nil); err == nil {
		t.Fatal("Windows 上 selfReplace 必须返回错误，实际返回 nil")
	}
}

// 平台判据必须如实传导到 Probe：Windows 上（即便其余判据都能满足）一律 supported:false。
func TestProbeReportsUnsupportedPlatformOnWindows(t *testing.T) {
	svc := NewRestartServiceWithHooks("jianartifact", RestartScopeSystem, RestartHooks{
		// 其余判据全部给足，让结论唯一地由**真实默认平台判据**决定。
		Argv0:   func() string { return "/opt/jianartifact/current/jianartifact" },
		Resolve: resolveOK(),
		EUID:    func() int { return 0 },
		Trigger: func() {},
	})

	plan := svc.Probe()
	if plan.Supported {
		t.Fatalf("Windows 上不得受理自替换，得 %+v", plan)
	}
	if plan.Reason == "" {
		t.Fatal("降级必须给出原因")
	}
	if plan.Command == "" {
		t.Fatal("降级时 plan.Command 不得为空（那是唯一的出路）")
	}
}
