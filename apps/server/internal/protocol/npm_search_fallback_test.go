package protocol

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 npm 搜索的**超时语义**与两处历史口径：
//
//   - 合并查询超时不得让整次搜索退化：回退逐成员检索，超时的那一个成员被跳过、其余成员
//     结果照常返回（这是引入合并查询之前的容错口径，属于可用性底线）；
//   - tarball 排除必须用完整的 `/-/` 标记，不能放宽成 `/-`——后者会误伤名字里含 `/-` 的
//     合法 packument（如 `@scope/-weird`）；
//   - 非 group 仓库它就是唯一成员，不再多发一次批量名字解析。
//
// 「合并查询超时、逐成员查询不超时」在真实数据库上无法稳定复现（两条路径共用同一个驱动与
// 同一份超时预算），因此回退分支靠注入桩覆盖（见 stubNpmSearch）。

// stubNpmSearch 替换「合并检索」与「单成员检索」两个原语，测试结束自动复原。
// 传 nil 表示保留真实实现（用于只模拟其中一侧失败）。
func stubNpmSearch(t *testing.T,
	merged func(svc *domain.RepositoryService, ctx context.Context, keyword string, repoIDs []int64, perRepoLimit int) ([]repository.Asset, error),
	member func(svc *domain.RepositoryService, ctx context.Context, keyword, repoName string, limit int) ([]repository.Asset, error)) {
	t.Helper()
	prevMerged, prevMember := npmSearchMerged, npmSearchMemberAssets
	if merged != nil {
		npmSearchMerged = merged
	}
	if member != nil {
		npmSearchMemberAssets = member
	}
	t.Cleanup(func() { npmSearchMerged, npmSearchMemberAssets = prevMerged, prevMember })
}

// captureNpmSearchLog 捕获本次用例产生的标准日志，用例结束自动复原输出目标。
func captureNpmSearchLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevWriter := log.Writer()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prevWriter) })
	return buf
}

// createNpmSearchGroup 建成员仓库与 group（成员按给定顺序）。
func createNpmSearchGroup(t *testing.T, env *npmSearchEnv, groupName string, members []string) {
	t.Helper()
	for _, name := range members {
		if _, err := env.repos.Create(name, "npm", "hosted", "public", ""); err != nil {
			t.Fatalf("建成员仓库 %s：%v", name, err)
		}
	}
	cfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{Members: members})
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	if _, err := env.repos.Create(groupName, "npm", "group", "public", cfg); err != nil {
		t.Fatalf("建 group 仓库 %s：%v", groupName, err)
	}
}

// npmSearchNames 提取响应里的包名序列，便于断言顺序。
func npmSearchNames(out npmSearchResponse) []string {
	names := make([]string, 0, len(out.Objects))
	for _, obj := range out.Objects {
		names = append(names, obj.Package.Name)
	}
	return names
}

// TestNpmSearchFallbackSkipsTimedOutMember 钉住超时回退的完整语义：
// 合并查询整体超时 → 回退逐成员 → 超时的那一个成员被跳过，其余成员结果仍在。
func TestNpmSearchFallbackSkipsTimedOutMember(t *testing.T) {
	env := newNpmSearchEnv(t)
	members := []string{"npm-fb-a", "npm-fb-b", "npm-fb-c"}
	createNpmSearchGroup(t, env, "npm-fb-grp", members)
	env.publishPackument(t, "npm-fb-a", "fb-alpha")
	env.publishPackument(t, "npm-fb-b", "fb-beta")
	env.publishPackument(t, "npm-fb-c", "fb-gamma")

	// 真实单成员检索实现：除被模拟超时的那个成员外，其余成员仍走真实链路。
	realMember := npmSearchMemberAssets
	stubNpmSearch(t,
		func(*domain.RepositoryService, context.Context, string, []int64, int) ([]repository.Asset, error) {
			return nil, fmt.Errorf("%w（测试注入：合并检索超时）", domain.ErrSearchTimeout)
		},
		func(svc *domain.RepositoryService, ctx context.Context, keyword, repoName string, limit int) ([]repository.Asset, error) {
			if repoName == "npm-fb-b" {
				return nil, fmt.Errorf("%w（测试注入：成员检索超时）", domain.ErrSearchTimeout)
			}
			return realMember(svc, ctx, keyword, repoName, limit)
		},
	)
	logs := captureNpmSearchLog(t)

	out, _ := env.search(t, "/npm/npm-fb-grp/-/v1/search?text=fb-&size=20")
	if out.Total != 2 {
		t.Fatalf("total = %d，期望 2（超时成员被跳过，其余成员结果保留）：%+v", out.Total, out.Objects)
	}
	names := npmSearchNames(out)
	if strings.Join(names, ",") != "fb-alpha,fb-gamma" {
		t.Fatalf("结果 = %v，期望 [fb-alpha fb-gamma]（成员顺序、超时成员缺席）", names)
	}
	// 少结果必须有痕：至少两条中文 warn，分别说明合并检索降级与某个成员被跳过，且带成员名。
	text := logs.String()
	t.Logf("降级日志：%s", strings.TrimSpace(text))
	if !strings.Contains(text, "npm 搜索超时") {
		t.Fatalf("缺少中文超时 warn 日志，实际日志：%q", text)
	}
	if !strings.Contains(text, "npm-fb-b") {
		t.Fatalf("warn 日志未指明被跳过的成员，实际日志：%q", text)
	}
}

// TestNpmSearchSingleMemberTimeoutKeepsOneBudget 钉住单成员（含非 group 仓库）不做回退重试：
// 「跳过唯一成员」与「整体降级为空」结果相同，重试只会把最坏耗时从一份预算翻成两份。
func TestNpmSearchSingleMemberTimeoutKeepsOneBudget(t *testing.T) {
	env := newNpmSearchEnv(t)
	if _, err := env.repos.Create("npm-solo-timeout", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	env.publishPackument(t, "npm-solo-timeout", "solo-pkg")

	memberCalls := 0
	stubNpmSearch(t,
		func(*domain.RepositoryService, context.Context, string, []int64, int) ([]repository.Asset, error) {
			return nil, fmt.Errorf("%w（测试注入：合并检索超时）", domain.ErrSearchTimeout)
		},
		func(svc *domain.RepositoryService, ctx context.Context, keyword, repoName string, limit int) ([]repository.Asset, error) {
			memberCalls++
			return nil, fmt.Errorf("%w（测试注入：成员检索超时）", domain.ErrSearchTimeout)
		},
	)
	logs := captureNpmSearchLog(t)

	out, _ := env.search(t, "/npm/npm-solo-timeout/-/v1/search?text=solo&size=20")
	if out.Total != 0 || len(out.Objects) != 0 {
		t.Fatalf("超时后应为空结果（响应结构不变）：total=%d objects=%d", out.Total, len(out.Objects))
	}
	if memberCalls != 0 {
		t.Fatalf("单成员不应回退逐成员重试，实际重试 %d 次", memberCalls)
	}
	if !strings.Contains(logs.String(), "npm 搜索超时") {
		t.Fatalf("缺少中文超时 warn 日志，实际日志：%q", logs.String())
	}
}

// TestNpmSearchFallbackKeepsAllMembersWhenMergedQueryTimesOut 钉住回退路径与正常路径的结果
// 等价：只有合并查询超时、各成员都健康时，回退结果必须与常态逐条一致（顺序、去重、total）。
func TestNpmSearchFallbackKeepsAllMembersWhenMergedQueryTimesOut(t *testing.T) {
	env := newNpmSearchEnv(t)
	members := []string{"npm-eq-a", "npm-eq-b"}
	createNpmSearchGroup(t, env, "npm-eq-grp", members)
	env.publishPackument(t, "npm-eq-a", "eq-alpha")
	env.publishPackument(t, "npm-eq-a", "eq-beta")
	env.publishPackument(t, "npm-eq-b", "eq-gamma")
	// 同一包名在两个成员里都有：回退路径同样必须去重。
	env.publishPackument(t, "npm-eq-b", "eq-alpha")

	healthy, _ := env.search(t, "/npm/npm-eq-grp/-/v1/search?text=eq-&size=20")
	if healthy.Total != 3 {
		t.Fatalf("常态 total = %d，期望 3", healthy.Total)
	}

	stubNpmSearch(t,
		func(*domain.RepositoryService, context.Context, string, []int64, int) ([]repository.Asset, error) {
			return nil, fmt.Errorf("%w（测试注入：合并检索超时）", domain.ErrSearchTimeout)
		}, nil)
	fallback, _ := env.search(t, "/npm/npm-eq-grp/-/v1/search?text=eq-&size=20")

	if fallback.Total != healthy.Total {
		t.Fatalf("回退 total = %d，期望与常态一致（%d）", fallback.Total, healthy.Total)
	}
	got := strings.Join(npmSearchNames(fallback), ",")
	want := strings.Join(npmSearchNames(healthy), ",")
	if got != want {
		t.Fatalf("回退结果顺序/内容 = %s，期望与常态一致（%s）", got, want)
	}
}

// TestNpmSearchExcludesTarballByFullMarker 钉住排除词是完整的 `/-/`：
// 名字里含 `/-` 但不含 `/-/` 的 packument 必须留在结果里，tarball 路径必须被排除。
func TestNpmSearchExcludesTarballByFullMarker(t *testing.T) {
	env := newNpmSearchEnv(t)
	if _, err := env.repos.Create("npm-marker", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 合法 packument：`@scope/-weird` 的路径含 `/-`，放宽成 `/-` 会把它误排除。
	env.publishPackument(t, "npm-marker", "scoped/pkg")
	env.publishPackument(t, "npm-marker", "scoped/-dash")
	// tarball 路径：含完整 `/-/`，必须排除。
	env.publishPackument(t, "npm-marker", "scoped/pkg/-/pkg-1.0.0.tgz")
	env.publishPackument(t, "npm-marker", "scoped/-dash/-/dash-1.0.0.tgz")

	out, _ := env.search(t, "/npm/npm-marker/-/v1/search?text=scoped&size=20")
	if out.Total != 2 {
		t.Fatalf("total = %d，期望 2（只排除 tarball，不误伤含 /- 的包名）：%v", out.Total, npmSearchNames(out))
	}
	names := strings.Join(npmSearchNames(out), ",")
	if names != "scoped/-dash,scoped/pkg" {
		t.Fatalf("结果 = %s，期望 scoped/-dash,scoped/pkg（path 升序）", names)
	}
}

// TestNpmSearchSingleRepoSkipsMemberNameLookup 钉住非 group 仓库省掉一次批量名字解析：
// 它自己就是唯一成员，成员名解析没有意义。
func TestNpmSearchSingleRepoSkipsMemberNameLookup(t *testing.T) {
	env := newNpmSearchEnv(t)
	if _, err := env.repos.Create("npm-solo", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	env.publishPackument(t, "npm-solo", "solo-pkg")
	// 单成员 group 作为对照：同样只有一个成员，但要额外解析成员名。
	cfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{Members: []string{"npm-solo"}})
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	if _, err := env.repos.Create("npm-solo-grp", "npm", "group", "public", cfg); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}

	solo, soloQueries := env.search(t, "/npm/npm-solo/-/v1/search?text=solo&size=20")
	group, groupQueries := env.search(t, "/npm/npm-solo-grp/-/v1/search?text=solo&size=20")
	if solo.Total != 1 || group.Total != 1 {
		t.Fatalf("两侧都应只命中 1 条：单仓=%d 单成员 group=%d", solo.Total, group.Total)
	}
	if soloQueries >= groupQueries {
		t.Fatalf("单仓搜索应比单成员 group 少一次成员名解析，实际语句数 %d >= %d",
			soloQueries, groupQueries)
	}
}
