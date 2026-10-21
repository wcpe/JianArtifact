package domain_test

import (
	"testing"
)

// 本文件钉住 RepositoryService.ReadableRepoIDs 的一条**危险契约**：它没有 isAdmin 参数。
//
// 也就是说，把管理员主体的 ID 误传进来不会报错，也不会被特判，而是静默按普通主体判定——
// 结果只剩 public ∪ 该用户自身被授 read 的仓库，管理员该看到的私有仓库全部消失。
// 这是设计分工而不是缺陷（管理面由调用方按 isAdmin 分支绕开本方法：仓库列表在
// isAdmin 时走 ListWithStats 全量，搜索在 isAdmin 时把可搜范围置为「不限」），
// 但它属于「悄悄降权」式失误：没有报错、没有日志，只表现为列表少了一半仓库。
//
// 故这里把行为本身钉住：一旦有人改实现让它「认管理员」，本测试会失败，迫使其回头审查
// 所有调用点是否仍各自保留了 isAdmin 分支——那正是这条契约必须显式化的原因。

// TestReadableRepoIDsAdminSubjectIsDowngradedToPublic 断言管理员主体传进本方法后的降权行为，
// 并对照真正的管理面路径（ListWithStats）确实返回全量。
func TestReadableRepoIDsAdminSubjectIsDowngradedToPublic(t *testing.T) {
	t.Parallel()
	f := newReadableFixture(t)

	adminID, err := f.users.Create("readable-admin", "hash", "admin")
	if err != nil {
		t.Fatalf("建管理员用户：%v", err)
	}
	subject := f.subjectOf(t, adminID)

	readable, err := f.svc.ReadableRepoIDs(f.allRows, subject)
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	// 管理员本体没有组归属，也没有任何 ACL：本方法只放行公开仓库。
	if got, want := f.readableNames(t, readable), []string{"readable-public"}; !equalNames(got, want) {
		t.Fatalf("管理员主体被传进本方法时应静默降权为「仅公开仓库」：\n期望 = %v\n实际 = %v", want, got)
	}
	// 与逐仓库判定一致，说明它不是「漏判管理员」而是根本不把管理员当作输入维度。
	if got, want := f.readableNames(t, readable), f.expectedNames(t, subject); !equalNames(got, want) {
		t.Fatalf("批量判定与逐仓库判定不一致：\n批量   = %v\n逐仓库 = %v", got, want)
	}

	// 对照：管理面走的是调用方的 isAdmin 分支（ListWithStats），返回全部仓库，
	// 包括无任何 ACL 的 readable-denied。若哪天调用方改成直接把管理员主体丢进
	// ReadableRepoIDs，这里的对照就会与上面的降权结果形成明显落差。
	rows, _, total, err := f.svc.ListWithStats(1000, 0)
	if err != nil {
		t.Fatalf("管理面列举仓库失败：%v", err)
	}
	if total != len(f.allRows) || len(rows) != len(f.allRows) {
		t.Fatalf("管理面应返回全量仓库：total=%d rows=%d，期望 %d", total, len(rows), len(f.allRows))
	}
	foundDenied := false
	for i := range rows {
		if rows[i].Name == "readable-denied" {
			foundDenied = true
			break
		}
	}
	if !foundDenied {
		t.Fatalf("管理面应包含无任何 ACL 的 readable-denied：%v", f.readableNames(t, readable))
	}
}
