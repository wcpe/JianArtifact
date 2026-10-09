package repository_test

import (
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestDeleteRollbackRestoresGroupAcl 钉住「仓库删除回滚必须完整恢复组授权」。
//
// 背景（FR-36 回归）：用户组主体的授权信息全在 subject_group_id 上、subject_id 为 NULL，
// 而标签为用户的条目靠 subject_type 的列默认值就能兜底。因此回滚恢复语句若只写
// (repository_id, subject_id, action) 三列，用户授权看不出异常、组授权却会静默丢失——
// 权限系统里「静默丢授权」与「静默多授权」同样危险，必须用例钉住。
func TestDeleteRollbackRestoresGroupAcl(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "delete-rollback.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}

	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	repos := repository.NewRepoRepo(db)
	acls := repository.NewAclRepo(db)

	repoID, err := repos.Create("rollback-repo", "raw", "hosted", "private", "{}")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	uid, err := users.Create("rollback-user", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	gid, err := groups.Create("rollback-group", "回滚验证组")
	if err != nil {
		t.Fatalf("创建用户组：%v", err)
	}

	// 用户主体与组主体各一条，覆盖两种恢复分支。
	if err := acls.Replace(repoID, []repository.Acl{
		{SubjectID: uid, SubjectType: repository.SubjectTypeUser, Action: repository.ActionRead},
		{SubjectGroupID: gid, SubjectType: repository.SubjectTypeGroup, Action: repository.ActionPublish},
	}); err != nil {
		t.Fatalf("写入 ACL：%v", err)
	}
	before, err := acls.ListByRepo(repoID)
	if err != nil {
		t.Fatalf("读取快照：%v", err)
	}
	if len(before) != 2 {
		t.Fatalf("快照应有 2 条，得 %d", len(before))
	}

	// 构造一次「已完成但可回滚」的操作：写 journal 后删掉仓库（级联清 ACL），
	// 再走真实的回滚入口恢复。
	//
	// 快照必须在开事务之前取：SQLite 单写者，事务持有连接时再向同一连接池取连接会自锁。
	mutations := repository.NewAssetMutationRepo(db)
	repoRow, err := repos.GetByName("rollback-repo")
	if err != nil {
		t.Fatalf("读取仓库快照：%v", err)
	}
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO asset_mutation (id, status, created_at, updated_at) VALUES ('op-rollback', 'completed', datetime('now'), datetime('now'))`,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("建操作记录：%v", err)
	}
	if err := repository.PutRepositoryDeleteJournal(tx, "op-rollback", repository.RepositoryDeleteJournal{Repository: *repoRow, ACL: before}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写回滚快照：%v", err)
	}
	if err := repos.DeleteTx(tx, "rollback-repo"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("删除仓库：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交：%v", err)
	}

	// 删除后 ACL 应已随仓库级联清空。
	if got, err := acls.ListByRepo(repoID); err != nil || len(got) != 0 {
		t.Fatalf("级联删除后 ACL 应为空，得 %+v err=%v", got, err)
	}

	// 走真实回滚入口恢复。
	if err := mutations.RollbackCompleted("op-rollback", nil, "验证回滚恢复"); err != nil {
		t.Fatalf("回滚：%v", err)
	}

	after, err := acls.ListByRepo(repoID)
	if err != nil {
		t.Fatalf("回滚后读取 ACL：%v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("回滚后 ACL 条数 = %d，期望 %d（组授权丢失即此断言失败）", len(after), len(before))
	}

	var userKept, groupKept bool
	for _, a := range after {
		switch a.SubjectType {
		case repository.SubjectTypeUser:
			userKept = a.SubjectID == uid && a.Action == repository.ActionRead
		case repository.SubjectTypeGroup:
			groupKept = a.SubjectGroupID == gid && a.Action == repository.ActionPublish
		}
	}
	if !userKept {
		t.Errorf("回滚未恢复用户主体授权：%+v", after)
	}
	if !groupKept {
		t.Errorf("回滚未恢复组主体授权（FR-36 回归）：%+v", after)
	}

	// 授权要真的生效，而不只是行存在。
	ok, err := acls.HasPermission(repoID, repository.Subject{UserID: 0, GroupIDs: []int64{gid}}, repository.ActionPublish)
	if err != nil {
		t.Fatalf("回滚后判定组授权：%v", err)
	}
	if !ok {
		t.Error("回滚后该组的 publish 授权未生效")
	}
}
