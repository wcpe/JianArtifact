// Package repository 提供元数据的持久化读写（SQLite，经 sqlx）。
//
// 分层（见 internal/doc.go）：repository -> persistence。仅负责数据存取，
// 不含业务规则（业务规则在 domain 层）。各 Repo 持有 *persistence.DB。
package repository

// User 是 user 表的行模型。PasswordHash 仅在内部流转，不对外暴露。
type User struct {
	ID           int64  `db:"id"`
	Username     string `db:"username"`
	PasswordHash string `db:"password_hash"`
	Role         string `db:"role"`
	Status       string `db:"status"`
	CreatedAt    string `db:"created_at"`
}

// Token 是 api_token 表的行模型（不含摘要，列表场景使用）。
type Token struct {
	ID        int64  `db:"id"`
	UserID    int64  `db:"user_id"`
	Name      string `db:"name"`
	CreatedAt string `db:"created_at"`
}

// Repository 是 repository 表的行模型。
type Repository struct {
	ID         int64  `db:"id"`
	Name       string `db:"name"`
	Format     string `db:"format"`
	Type       string `db:"type"`
	Visibility string `db:"visibility"`
	CreatedAt  string `db:"created_at"`
}

// Acl 是 acl 表中一条授权（主体 × 动作）。
type Acl struct {
	SubjectID int64  `db:"subject_id"`
	Action    string `db:"action"`
}
