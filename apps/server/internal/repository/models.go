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
	// AuthSource 标记身份来源：local（本地口令）/ oidc / ldap（见 ADR-0029）。
	AuthSource string `db:"auth_source"`
	// ExternalSubject 是身份源内稳定标识（OIDC sub / LDAP DN）；空串表示未绑定外部身份。
	ExternalSubject string `db:"external_subject"`
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
	// Aliases 是仓库的别名列表（非列字段，由服务层按需填充；别名与主名共享命名空间）。
	Aliases []string `db:"-"`
}

// RepositoryConfig 是 repository.config 列的结构化视图：
// proxy 用 RemoteURL 存上游地址、CredentialRef 存受限逻辑名称（运行时只解析专用命名空间），group 用 Members 存有序成员仓库名。
// hosted 三者皆空。序列化后落 config 列（缺省 "{}"）。
type RepositoryConfig struct {
	RemoteURL        string   `json:"remoteUrl,omitempty"`
	CredentialRef    string   `json:"credentialRef,omitempty"`
	Members          []string `json:"members,omitempty"`
	ImmutableRelease bool     `json:"immutableRelease,omitempty"`
	// CacheRetentionDays 是 proxy 仓库代理缓存资产的保留天数（FR-41）：
	// 0/缺省 = 关闭（默认），仅允许 proxy 类型取非 0；负数非法。
	// 由 storage-cleanup 作业按此值淘汰超期缓存，删除走既有资产变更通道。
	CacheRetentionDays int `json:"cacheRetentionDays,omitempty"`
	// QuotaBytes 是仓库的存储配额上限（FR-41），口径为逻辑字节 SUM(asset.size)：
	// 0/缺省 = 不限，负数非法；group 类型不承载写入，必须为 0。
	QuotaBytes int64 `json:"quotaBytes,omitempty"`
	// QuotaAssets 是仓库的制品数配额上限（FR-41），口径为 COUNT(*)：
	// 0/缺省 = 不限，负数非法；group 类型不承载写入，必须为 0。
	QuotaAssets int64 `json:"quotaAssets,omitempty"`
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
//
// 主体分两类（FR-36，迁移 0046）：用户主体用 SubjectID 标记、SubjectGroupID 恒为 0；
// 用户组主体用 SubjectGroupID 标记、SubjectID 恒为 0。两列互斥由表内 CHECK 强制，
// 因此写入前必须把「不用的那一列」显式写 NULL。
type Acl struct {
	SubjectID      int64  `db:"subject_id"`
	SubjectType    string `db:"subject_type"`
	SubjectGroupID int64  `db:"subject_group_id"`
	Action         string `db:"action"`
}

// acl 主体类型取值（对应 acl.subject_type 的 CHECK）。
const (
	SubjectTypeUser  = "user"
	SubjectTypeGroup = "group"
)

// acl 动作取值（FR-36 六档，对应 acl.action 的 CHECK）。
// 三档 read / write / admin 是既有语义，publish / delete / acl_manage 为本次细化新增。
const (
	ActionRead      = "read"
	ActionWrite     = "write"
	ActionPublish   = "publish"
	ActionDelete    = "delete"
	ActionAclManage = "acl_manage"
	ActionAdmin     = "admin"
)

// Subject 是授权判定的主体：用户自身（UserID）及其所属用户组 ID 集合（GroupIDs）。
//
// 之所以要成「集合」而非单个 ID：一次判定必须同时看「用户自己的 ACL」与
// 「该用户所属任一组的 ACL」，把组 ID 集合一次性下推给 SQL，可以让这次判定
// 仍是**一条** COUNT 查询（否则每个组一次往返，鉴权热路径会退化成 N+1）。
//
// UserID == 0 表示匿名（或尚未映射到具体用户的主体）；匿名不属于任何组，
// 故 GroupIDs 被忽略。
type Subject struct {
	UserID   int64
	GroupIDs []int64
}

// UserSubject 构造「仅用户自身」的主体（无组归属）。
func UserSubject(userID int64) Subject { return Subject{UserID: userID} }

// AnonymousSubject 返回匿名主体：无用户 ID、无组归属，不在任何 ACL 条目上命中。
func AnonymousSubject() Subject { return Subject{} }

// IsAnonymous 判断是否为匿名主体（无用户 ID）。
func (s Subject) IsAnonymous() bool { return s.UserID == 0 }

// groupIDs 返回规范化后的组 ID 列表：去掉非正数与重复项，保持稳定顺序。
// 空列表表示主体不属任何组，调用方据此省略 SQL 中的组分支，避免构造出非法的 IN ()。
func (s Subject) groupIDs() []int64 {
	if len(s.GroupIDs) == 0 {
		return nil
	}
	out := make([]int64, 0, len(s.GroupIDs))
	seen := make(map[int64]bool, len(s.GroupIDs))
	for _, id := range s.GroupIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// UserGroup 是 user_group 表的行模型（FR-36）：授权主体之一是「用户组」。
// 组名全局唯一；ACL 里引用的是 id，故组改名不影响既有授权。
type UserGroup struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	Description string `db:"description"`
	CreatedAt   string `db:"created_at"`
}

// UserGroupMember 是 user_group_member 表的行模型：组 × 用户的多对多关联。
// 两侧外键均 ON DELETE CASCADE——删组清成员、删用户清归属。
type UserGroupMember struct {
	GroupID   int64  `db:"group_id"`
	UserID    int64  `db:"user_id"`
	CreatedAt string `db:"created_at"`
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
	// QuotaExempt 是仓库存储配额豁免标记（FR-41）：**非持久化字段**，仅供迁移导入与
	// 备份恢复等管理员批量操作在提交点声明「本次写入不受仓库配额约束」。零值表示
	// 正常受配额约束；协议发布路径不得置位（配额守卫在提交点读取它）。
	QuotaExempt bool `db:"-" json:"-"`
}
