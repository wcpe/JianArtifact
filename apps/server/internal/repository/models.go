// Package repository 提供元数据的持久化读写（SQLite，经 sqlx）。
//
// 分层（见 internal/doc.go）：repository -> persistence。仅负责数据存取，
// 不含业务规则（业务规则在 domain 层）。各 Repo 持有 *persistence.DB。
package repository

import (
	"encoding/json"
	"errors"
)

// User 是 user 表的行模型。PasswordHash 仅在内部流转，不对外暴露。
type User struct {
	ID               int64  `db:"id"`
	Username         string `db:"username"`
	PasswordHash     string `db:"password_hash"`
	Role             string `db:"role"`
	Status           string `db:"status"`
	WebLoginDisabled bool   `db:"web_login_disabled"`
	CreatedAt        string `db:"created_at"`
	// Email 是账号绑定邮箱（可空），用于审计检索与操作者身份快照。
	Email string `db:"email"`
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
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	Format      string `db:"format"`
	Type        string `db:"type"`
	Visibility  string `db:"visibility"`
	Description string `db:"description"` // 仓库描述（管理后台可配置，详情页展示）
	Config      string `db:"config"`      // 结构化配置 JSON（上游 URL、成员列表等）
	Online      bool   `db:"online"`      // 是否在线（默认 true；管理员可手动置 offline，FR-113）
	CreatedAt   string `db:"created_at"`
}

// RepositoryConfig 是 repository.config 列的结构化视图：
// proxy 用 RemoteURL 存上游地址、CredentialRef 存受限逻辑名称（运行时只解析专用命名空间），group 用 Members 存有序成员仓库名。
// hosted 三者皆空。序列化后落 config 列（缺省 "{}"）。
type RepositoryConfig struct {
	RemoteURL        string   `json:"remoteUrl,omitempty"`
	CredentialRef    string   `json:"credentialRef,omitempty"`
	Members          []string `json:"members,omitempty"`
	ImmutableRelease bool     `json:"immutableRelease,omitempty"`
}

// PublishPolicy 是用户×hosted 仓库的发布附加限制。
type PublishPolicy struct {
	UserID        int64    `db:"user_id" json:"userId"`
	RepositoryID  int64    `db:"repository_id" json:"repositoryId"`
	PathPrefixes  []string `db:"-" json:"pathPrefixes"`
	MaxAssetsHour int64    `db:"max_assets_hour" json:"maxAssetsHour"`
	MaxBytesDay   int64    `db:"max_bytes_day" json:"maxBytesDay"`
	MaxFileBytes  int64    `db:"max_file_bytes" json:"maxFileBytes"`
	UpdatedAt     string   `db:"updated_at" json:"updatedAt"`
}

// ErrQuotaExceeded 表示新增制品或字节数超过发布额度。
var ErrQuotaExceeded = errors.New("发布额度已用尽")

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
	Sha1         string `db:"sha1"`
	Md5          string `db:"md5"`
	CreatedAt    string `db:"created_at"`
	UpdatedAt    string `db:"updated_at"`
}
