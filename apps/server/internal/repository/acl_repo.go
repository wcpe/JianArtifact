package repository

import (
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AclRepo 读写 acl 表。
type AclRepo struct{ db *persistence.DB }

// NewAclRepo 构造 AclRepo。
func NewAclRepo(db *persistence.DB) *AclRepo { return &AclRepo{db: db} }

// ListByRepo 返回某仓库的全部 ACL 条目。
//
// 用户主体与组主体在同一列集合里返回：组条目只有 SubjectGroupID 有值、SubjectID 为 0，
// 调用方按 SubjectType 区分。排序把「先用户后组」放在最前，使同一仓库的条目呈现稳定顺序。
func (r *AclRepo) ListByRepo(repoID int64) ([]Acl, error) {
	var acls []Acl
	// COALESCE 把「另一主体的列」从 NULL 归一成 0：两种主体各自有一列恒为 NULL，
	// 而 Go 侧的行模型用 int64（零值即「不适用」），直接 Scan NULL 会报类型转换错误。
	err := r.db.Select(&acls,
		`SELECT COALESCE(subject_id, 0) AS subject_id, subject_type,
			COALESCE(subject_group_id, 0) AS subject_group_id, action
		FROM acl WHERE repository_id = ?
		ORDER BY subject_type, COALESCE(subject_group_id, subject_id), action`, repoID)
	return acls, err
}

// Replace 以事务方式覆盖某仓库的全部 ACL。
//
// 两类主体在同一趟写入：组条目写入 subject_group_id 并把 subject_id 显式写 NULL
// （subject_type='group' 的 CHECK 要求该列为 NULL，零值不等于 NULL）。
func (r *AclRepo) Replace(repoID int64, entries []Acl) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM acl WHERE repository_id = ?`, repoID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, e := range entries {
		if e.SubjectType == SubjectTypeGroup {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO acl (repository_id, subject_id, subject_type, subject_group_id, action)
				VALUES (?, NULL, ?, ?, ?)`,
				repoID, SubjectTypeGroup, e.SubjectGroupID, e.Action,
			); err != nil {
				_ = tx.Rollback()
				return err
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO acl (repository_id, subject_id, subject_type, subject_group_id, action)
			VALUES (?, ?, ?, NULL, ?)`,
			repoID, e.SubjectID, SubjectTypeUser, e.Action,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// satisfyingGrants 返回「能满足该动作请求」的已授权动作集合（即蕴含方的反向展开）。
//
// 判定方向：请求动作 A 被放行，当且仅当**存在某条已授权动作 G，使得 G 蕴含 A**。
// 本函数返回的正是全部这样的 G——传入请求的动作，产出要去 acl 表里匹配的
// action IN (...) 候选集。方向搞反会让权限反向放大（例如「只读」被当成「可写」），
// 故此处显式按「请求 → 蕴含它的授权」推导。
//
// 蕴含关系（FR-36）：
//   - admin 蕴含全部六档，故任何请求都能被 admin 授权满足；
//   - write 蕴含 read 与 publish，故 read / publish 请求可被 write 授权满足，
//     而 write 请求不能被 read 授权满足（只读不得反向扩成可写）；
//   - read 只蕴含自身——read 授权只能满足 read 请求；
//   - publish / delete / acl_manage 也只蕴含自身，且彼此互不满足：授予其一
//     不得顺带拿到另一档，否则「只许发布、不许删除」这类细粒度配置形同虚设。
//
// 未登记的动作（含空串）一律退化为「只由自身满足」，避免未知动作被宽泛授权放行。
func satisfyingGrants(action string) []string {
	switch action {
	case ActionRead:
		return []string{ActionRead, ActionWrite, ActionAdmin}
	case ActionWrite:
		return []string{ActionWrite, ActionAdmin}
	case ActionPublish:
		return []string{ActionPublish, ActionWrite, ActionAdmin}
	case ActionDelete:
		return []string{ActionDelete, ActionAdmin}
	case ActionAclManage:
		return []string{ActionAclManage, ActionAdmin}
	case ActionAdmin:
		return []string{ActionAdmin}
	default:
		return []string{action}
	}
}

// HasPermission 判断主体对仓库是否拥有指定动作的授权。
//
// 判定口径（FR-36）：**用户自身**的 ACL 命中 **或** 该用户所属**任一用户组**的
// ACL 命中，二者取或。组主体只是把「给 N 个用户逐条授予」收敛成一条，不改变
// 用户自身的既有授权。
//
// 匿名主体（UserID == 0）不属于任何组：只按「subject_id = 0」匹配，
// 而 acl.subject_id 恒为 NULL 或正数，故恒不命中，无需调用方特判。
func (r *AclRepo) HasPermission(repoID int64, subject Subject, action string) (bool, error) {
	grants := satisfyingGrants(action)
	groups := subject.groupIDs()
	// 主体不属任何组：只查用户分支。少了组分支，SQL 里也就不会出现空的 IN ()。
	if len(groups) == 0 {
		return r.countGrants(repoID, subject.UserID, nil, grants)
	}
	// 两个分支各自计数后相加：同一条 SQL 里混用「subject_id = ?」与
	// 「subject_group_id IN (...)」会把 OR 的优先级和 NULL 语义搅在一起，
	// 拆成两条计数更直白、也更好走索引。命中即短路，第二条不必执行。
	if ok, err := r.countGrants(repoID, subject.UserID, nil, grants); err != nil || ok {
		return ok, err
	}
	return r.countGrants(repoID, 0, groups, grants)
}

// countGrants 统计匹配的 ACL 行数是否大于 0。
//
// 参数互斥：groupIDs 为空时按用户主体查（subject_id = userID）；
// 非空时按组主体查（subject_id IS NULL AND subject_group_id IN (...)），userID 被忽略。
func (r *AclRepo) countGrants(repoID, userID int64, groupIDs []int64, grants []string) (bool, error) {
	var (
		query string
		args  []any
		err   error
	)
	if len(groupIDs) == 0 {
		query, args, err = sqlx.In(
			`SELECT COUNT(*) FROM acl WHERE repository_id = ? AND subject_type = 'user' AND subject_id = ? AND action IN (?)`,
			repoID, userID, grants,
		)
	} else {
		query, args, err = sqlx.In(
			`SELECT COUNT(*) FROM acl WHERE repository_id = ? AND subject_type = 'group' AND subject_group_id IN (?) AND action IN (?)`,
			repoID, groupIDs, grants,
		)
	}
	if err != nil {
		return false, err
	}
	var n int
	if err := r.db.Get(&n, r.db.Rebind(query), args...); err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteByGroup 删除以该用户组为主体的全部 ACL 条目（FR-36）。
//
// 删组时表外键本可 CASCADE 清理这些条目，但外键默认关闭的部署（或后续改为
// 逻辑删除的组）下会留下悬空授权，故在领域层显式清理一次：幂等，删不到行不算错。
func (r *AclRepo) DeleteByGroup(groupID int64) error {
	_, err := r.db.Exec(`DELETE FROM acl WHERE subject_type = 'group' AND subject_group_id = ?`, groupID)
	return err
}
