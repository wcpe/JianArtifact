package auth

import "testing"

func TestMachineScopeAllowed(t *testing.T) {
	scopes := []MachineTokenScope{
		{RepositoryID: 1, PathPrefix: "com/acme", Actions: []string{"publish"}},
		{RepositoryID: 1, PathPrefix: "", Actions: []string{"read"}},
		{RepositoryID: 2, PathPrefix: "org/x", Actions: []string{"read", "publish"}},
	}
	cases := []struct {
		repoID int64
		path   string
		action string
		want   bool
	}{
		{1, "com/acme/lib.jar", "publish", true},
		{1, "com/acme", "publish", true},
		{1, "com/acmeX/lib.jar", "publish", false}, // 段边界：foo 不命中 foobar
		{1, "com/acme", "read", true},              // 根 scope 兜底 read
		{1, "anywhere.txt", "read", true},
		{1, "", "read", true}, // 仓库级操作仅根 scope 命中
		{1, "", "publish", false},
		{1, "com/acme/lib.jar", "delete", false},
		{2, "org/x/a.jar", "publish", true},
		{2, "org/x/a.jar", "read", true},
		{3, "org/x/a.jar", "read", false}, // 仓库不匹配
	}
	for _, tc := range cases {
		if got := MachineScopeAllowed(scopes, tc.repoID, tc.path, tc.action); got != tc.want {
			t.Errorf("Allow(%d,%q,%q)=%v，期望 %v", tc.repoID, tc.path, tc.action, got, tc.want)
		}
	}
}

func TestParseMachineScopes(t *testing.T) {
	if scopes, err := ParseMachineScopes(""); err != nil || scopes != nil {
		t.Fatalf("空串应解析为 nil：%v，%v", scopes, err)
	}
	if _, err := ParseMachineScopes("{bad"); err == nil {
		t.Fatal("损坏 JSON 应报错而不是降级为空集")
	}
	scopes, err := ParseMachineScopes(`[{"repository_id":7,"path_prefix":"a/b","actions":["read"]}]`)
	if err != nil || len(scopes) != 1 || scopes[0].RepositoryID != 7 || scopes[0].PathPrefix != "a/b" {
		t.Fatalf("解析结果错误：%+v，%v", scopes, err)
	}
}

func TestMachinePurposeAllows(t *testing.T) {
	if !MachinePurposeAllows(TokenPurposeLegacy, "write") {
		t.Error("旧凭据应保留既有协议动作")
	}
	if MachinePurposeAllows(TokenPurposeArtifactRead, "publish") {
		t.Error("artifact_read 不能发布")
	}
	if !MachinePurposeAllows(TokenPurposeArtifactPublish, "publish") || !MachinePurposeAllows(TokenPurposeArtifactPublish, "read") {
		t.Error("artifact_publish 应允许显式 read/publish")
	}
	if MachinePurposeAllows("bogus", "read") {
		t.Error("未知用途一律拒绝")
	}
}
