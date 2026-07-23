// Package repository 提供元数据的持久化读写（SQLite，经 sqlx）。
//
// 分层（见 internal/doc.go）：repository -> persistence。仅负责数据存取，
// 不含业务规则（业务规则在 domain 层）。各 Repo 持有 *persistence.DB。
package repository

import "encoding/json"

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
	Config     string `db:"config"` // 结构化配置 JSON（上游 URL、成员列表等）
	CreatedAt  string `db:"created_at"`
}

// RepositoryConfig 是 repository.config 列的结构化视图：
// proxy 用 RemoteURL 存上游地址，group 用 Members 存有序成员仓库名。
// hosted 两者皆空。序列化后落 config 列（缺省 "{}"）。
type RepositoryConfig struct {
	RemoteURL string   `json:"remoteUrl,omitempty"`
	Members   []string `json:"members,omitempty"`
}

// DecodeConfig 解析仓库的 config 列为结构化配置；空串视为空配置。
func (r *Repository) DecodeConfig() (RepositoryConfig, error) {
	var c RepositoryConfig
	if r.Config == "" {
		return c, nil
	}
	err := json.Unmarshal([]byte(r.Config), &c)
	return c, err
}

// EncodeRepositoryConfig 把结构化配置序列化为 config 列的 JSON 文本。
func EncodeRepositoryConfig(c RepositoryConfig) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Acl 是 acl 表中一条授权（主体 × 动作）。
type Acl struct {
	SubjectID int64  `db:"subject_id"`
	Action    string `db:"action"`
}

// Asset 是 asset 表的行模型：仓库内某路径的制品，指向内容寻址 blob。
type Asset struct {
	ID           int64  `db:"id"`
	RepositoryID int64  `db:"repository_id"`
	Path         string `db:"path"`
	BlobHash     string `db:"blob_hash"`
	Size         int64  `db:"size"`
	ContentType  string `db:"content_type"`
	CreatedAt    string `db:"created_at"`
	UpdatedAt    string `db:"updated_at"`
}
