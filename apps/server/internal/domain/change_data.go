package domain

import "github.com/wcpe/jianartifact/apps/server/internal/repository"

// 变更日志/原子 operation 信封共用的实体类型、操作与 data 结构（FR-83 / ADR-0013）。
// 复制退役后这些结构仍是**原子 operation 信封**的载荷（asset_mutation 的
// ApplyWithOutbox* 把它们写入 replication_operation_outbox，同一事务提交），
// 因此从已删除的 replication.go 中独立出来保留。

// 实体类型与操作。
const (
	EntityAsset      = "asset"
	EntityRepository = "repository"
	EntityAcl        = "acl"
	EntityUser       = "user"
	EntityToken      = "token"
	EntitySetting    = "setting"

	OpPut    = "put"
	OpDelete = "delete"
)

// 自然键前缀（跨节点一致，不依赖 SQLite 数值 ID）。
const (
	EntityFormatMetadata = "format_metadata"

	keyPrefixAsset   = "asset:"
	keyPrefixRepo    = "repo:"
	keyPrefixAcl     = "acl:"
	keyPrefixUser    = "user:"
	keyPrefixToken   = "token:"
	keyPrefixSetting = "setting:"

	keyPrefixFormatMetadata = "format_metadata:"
)

// FormatMetadataKey 构造格式元数据自然键。
func FormatMetadataKey(repoName, format, nameNormalized, filename string) string {
	return keyPrefixFormatMetadata + repoName + "/" + format + "/" + nameNormalized + "/" + filename
}

// FormatMetadataChangeData 是格式元数据 put 变更/信封的 data。
type FormatMetadataChangeData struct {
	RepoName          string `json:"repoName"`
	Format            string `json:"format"`
	NameDisplay       string `json:"nameDisplay"`
	NameNormalized    string `json:"nameNormalized"`
	Version           string `json:"version"`
	VersionNormalized string `json:"versionNormalized"`
	Filename          string `json:"filename"`
	AssetPath         string `json:"assetPath"`
	Sha256            string `json:"sha256"`
	Size              int64  `json:"size"`
	RequiresPython    string `json:"requiresPython,omitempty"`
	Yanked            string `json:"yanked,omitempty"`
}

// formatMetadataChangeData 由仓储行构造信封 data。
func formatMetadataChangeData(repoName string, meta *repository.FormatMetadata) FormatMetadataChangeData {
	return FormatMetadataChangeData{
		RepoName:          repoName,
		Format:            meta.Format,
		NameDisplay:       meta.NameDisplay,
		NameNormalized:    meta.NameNormalized,
		Version:           meta.Version,
		VersionNormalized: meta.VersionNormalized,
		Filename:          meta.Filename,
		AssetPath:         meta.AssetPath,
		Sha256:            meta.Sha256,
		Size:              meta.Size,
		RequiresPython:    meta.RequiresPython,
		Yanked:            meta.Yanked,
	}
}

// TombstoneData 是删除变更的统一 data：仅标记已删除，实体定位由 entity_key 承担。
type TombstoneData struct {
	Deleted bool `json:"deleted"`
}

// AssetChangeData 是制品 put 变更的 data。
type AssetChangeData struct {
	Path        string `json:"path"`
	BlobHash    string `json:"blobHash"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Sha1        string `json:"sha1,omitempty"`
	Md5         string `json:"md5,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"` // UTC "YYYY-MM-DD HH:MM:SS"，源端创建时间
	UpdatedAt   string `json:"updatedAt,omitempty"` // UTC "YYYY-MM-DD HH:MM:SS"，源端最后修改时间
}

// RepoChangeData 是仓库 put 变更的 data。
type RepoChangeData struct {
	Name        string `json:"name"`
	Format      string `json:"format"`
	Type        string `json:"type"`
	Visibility  string `json:"visibility"`
	Description string `json:"description,omitempty"`
	Config      string `json:"config,omitempty"`
}

// AclEntryData 是 ACL 中一条授权（以 username 而非 userID 编址）。
type AclEntryData struct {
	Username string `json:"username"`
	Action   string `json:"action"`
}

// AclChangeData 是仓库 ACL 整仓快照的 put 变更 data。
type AclChangeData struct {
	RepoName string         `json:"repoName"`
	Entries  []AclEntryData `json:"entries"`
}

// UserChangeData 是用户 put 变更的 data（passwordHash 为 argon2id 哈希，非明文）。
type UserChangeData struct {
	Username         string `json:"username"`
	Role             string `json:"role"`
	Status           string `json:"status"`
	PasswordHash     string `json:"passwordHash"`
	WebLoginDisabled bool   `json:"webLoginDisabled,omitempty"`
	CreatedAt        string `json:"createdAt,omitempty"`
}

// TokenChangeData 是令牌 put 变更的 data（hash 为 sha256 摘要，明文不出现）。
type TokenChangeData struct {
	Hash      string `json:"hash"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Scopes    string `json:"scopes"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// SettingChangeData 是 setting put 变更的 data。
type SettingChangeData struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// 自然键构造器（跨节点一致）。
func AssetKey(repoName, path string) string { return keyPrefixAsset + repoName + "/" + path }

func RepoKey(name string) string { return keyPrefixRepo + name }

func AclKey(repoName string) string { return keyPrefixAcl + repoName }

func UserKey(username string) string { return keyPrefixUser + username }

func TokenKey(digest string) string { return keyPrefixToken + digest }

func SettingKey(key string) string { return keyPrefixSetting + key }
