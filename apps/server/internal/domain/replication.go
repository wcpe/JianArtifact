package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 变更日志实体类型与操作（FR-83，见 ADR-0013）。
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
	keyPrefixAsset   = "asset:"
	keyPrefixRepo    = "repo:"
	keyPrefixAcl     = "acl:"
	keyPrefixUser    = "user:"
	keyPrefixToken   = "token:"
	keyPrefixSetting = "setting:"

	settingKeyNodeID = "node_id"
)

// ChangeRecorder 是写路径记录变更日志的接口（FR-83）。
// 各写路径 service 持有本接口（可为 nil，nil 时写路径静默跳过）；
// 由 ReplicationService 实现，经 SetChangeRecorder 注入。
type ChangeRecorder interface {
	Record(entityType, entityKey, op string, data any) error
}

// --- 变更 data 结构（跨节点传输，JSON 序列化后落 repl_change.data） ---

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
	UpdatedAt   string `json:"updatedAt,omitempty"`
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
	Username     string `json:"username"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	PasswordHash string `json:"passwordHash"`
	CreatedAt    string `json:"createdAt,omitempty"`
}

// TokenChangeData 是令牌 put 变更的 data（hash 为 sha256 摘要，明文不出现）。
type TokenChangeData struct {
	Hash      string `json:"hash"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// SettingChangeData 是实例设置 put 变更的 data。
type SettingChangeData struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ReplicationService 负责复制变更日志的记录与应用（LWW 冲突解决）。
// 记录：写路径经 ChangeRecorder 调 Record 落 repl_change；
// 应用：复制协议把对端变更喂给 Apply（FR-84 负责拉取传输，本 FR 提供纯本地可测的应用逻辑）。
type ReplicationService struct {
	repl     *repository.ReplChangeRepo
	assets   *repository.AssetRepo
	repos    *repository.RepoRepo
	acls     *repository.AclRepo
	users    *repository.UserRepo
	tokens   *repository.TokenRepo
	settings *repository.SettingRepo
	blobs    *blobstore.Store

	nodeID string // 惰性初始化缓存
}

// NewReplicationService 构造 ReplicationService。blobs 供复制协议 blob 流式提供（FR-84）。
func NewReplicationService(
	repl *repository.ReplChangeRepo,
	assets *repository.AssetRepo,
	repos *repository.RepoRepo,
	acls *repository.AclRepo,
	users *repository.UserRepo,
	tokens *repository.TokenRepo,
	settings *repository.SettingRepo,
	blobs *blobstore.Store,
) *ReplicationService {
	return &ReplicationService{
		repl: repl, assets: assets, repos: repos,
		acls: acls, users: users, tokens: tokens, settings: settings,
		blobs: blobs,
	}
}

// OpenBlob 按内容哈希打开 blob 读取流（供复制协议 GET blob 端点流式返回，FR-84）。
// blob 不存在返回 blobstore 的 not found 错误。
func (s *ReplicationService) OpenBlob(hash string) (io.ReadCloser, int64, error) {
	return s.blobs.Open(hash)
}

// ClusterStatus 是集群同步状态（FR-86，供 CLI / web 管理面）。
type ClusterStatus struct {
	NodeID       string `json:"nodeId"`
	PeerURL      string `json:"peerUrl,omitempty"`
	TokenSet     bool   `json:"tokenSet"`
	Enabled      bool   `json:"enabled"`
	Watermark    int64  `json:"watermark"`
	HasWatermark bool   `json:"hasWatermark"`
	LastSyncAt   string `json:"lastSyncAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
}

// ClusterStatus 读取集群同步状态（FR-86，FR-88 改造）。
// 对端配置从 setting 读取（web 可配置），令牌仅返回是否已配置（不暴露明文）。
func (s *ReplicationService) ClusterStatus() ClusterStatus {
	peerURL, _ := s.settings.Get(SettingKeyReplPeerURL)
	token, _ := s.settings.Get(SettingKeyReplPeerToken)
	st := ClusterStatus{
		NodeID:   s.NodeID(),
		PeerURL:  peerURL,
		TokenSet: token != "",
		Enabled:  true, // 缺省启用
	}
	if v, err := s.settings.Get(SettingKeyReplEnabled); err == nil {
		st.Enabled = v != "false"
	}
	if w, err := s.settings.Get(ReplicationWatermarkKey(peerURL)); err == nil {
		if seq, perr := strconv.ParseInt(w, 10, 64); perr == nil {
			st.Watermark = seq
			st.HasWatermark = true
		}
	}
	st.LastSyncAt, _ = s.settings.Get(SettingKeyReplLastSync)
	st.LastError, _ = s.settings.Get(SettingKeyReplLastError)
	return st
}

// PeerConfig 返回当前对端配置（URL/令牌，来自 setting，FR-88）。
func (s *ReplicationService) PeerConfig() (peerURL, token string, err error) {
	peerURL, err = s.settings.Get(SettingKeyReplPeerURL)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return "", "", err
	}
	token, terr := s.settings.Get(SettingKeyReplPeerToken)
	if terr != nil && !errors.Is(terr, repository.ErrNotFound) {
		return "", "", terr
	}
	return peerURL, token, nil
}

// SetPeerConfig 保存对端配置（URL/令牌，web 可配置，FR-88）。
// 仅保存配置，不开始同步（自动同步由开关控制，手动由 SyncNow 触发）。
func (s *ReplicationService) SetPeerConfig(peerURL, token string) error {
	if err := s.settings.Set(SettingKeyReplPeerURL, peerURL); err != nil {
		return err
	}
	return s.settings.Set(SettingKeyReplPeerToken, token)
}

// SetSyncEnabled 设置同步调度启停开关（FR-86）。
func (s *ReplicationService) SetSyncEnabled(enabled bool) error {
	v := "false"
	if enabled {
		v = "true"
	}
	return s.settings.Set(SettingKeyReplEnabled, v)
}

// Record 落一条变更日志（ChangeRecorder 接口实现）。
func (s *ReplicationService) Record(entityType, entityKey, op string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.repl.Append(s.NodeID(), op, entityType, entityKey, string(raw),
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// NodeID 返回本节点唯一标识：优先环境变量 JIAN_NODE_ID；未设则读取 / 生成
// 并持久化到 setting（key node_id），重启后保持一致。
func (s *ReplicationService) NodeID() string {
	if s.nodeID != "" {
		return s.nodeID
	}
	if id := os.Getenv("JIAN_NODE_ID"); id != "" {
		s.nodeID = id
		return id
	}
	if id, err := s.settings.Get(settingKeyNodeID); err == nil && id != "" {
		s.nodeID = id
		return id
	}
	id := randomHex(16)
	// 持久化失败不阻塞写路径（下次调用重试）；随机值保证并发下也唯一。
	_ = s.settings.Set(settingKeyNodeID, id)
	s.nodeID = id
	return id
}

// LatestSeq 返回本地最新变更序号（复制协议 since 水位用）。
func (s *ReplicationService) LatestSeq() (int64, error) { return s.repl.LatestSeq() }

// ListSince 返回 seq 大于 since 的变更（FR-84 增量拉取用）。
func (s *ReplicationService) ListSince(since int64, limit int) ([]repository.Change, error) {
	return s.repl.ListSince(since, limit)
}

// Apply 把一条远端变更应用到本地业务表（LWW 裁决 + tombstone 识别）。
// LWW：本地对该实体已有更晚写入（ts,node_id 更大）则跳过；否则应用。
// 应用成功后不落本地 repl_change（复制协议按 seq 顺序应用保证 tombstone
// 不被乱序覆盖；本地 seq 只反映本地自身的写，避免对端回声）。
func (s *ReplicationService) Apply(ch repository.Change) error {
	last, err := s.repl.LastChangeOf(ch.EntityType, ch.EntityKey)
	if err == nil && newer(last.TS, last.NodeID, ch.TS, ch.NodeID) {
		return nil // 本地已有更新写入，后写覆盖（LWW）
	}
	switch ch.EntityType {
	case EntityAsset:
		return s.applyAsset(ch)
	case EntityRepository:
		return s.applyRepository(ch)
	case EntityAcl:
		return s.applyAcl(ch)
	case EntityUser:
		return s.applyUser(ch)
	case EntityToken:
		return s.applyToken(ch)
	case EntitySetting:
		return s.applySetting(ch)
	default:
		return ErrValidation
	}
}

// newer 判断 (ts,node) 是否比 (ts2,node2) 更新：先比 ts（RFC3339Nano 字典序即时间序），
// 同 ts 以 node_id 字典序打破平局（时钟漂移兜底，见 ADR-0013 决策 5）。
func newer(ts, node, ts2, node2 string) bool {
	if ts != ts2 {
		return ts > ts2
	}
	return node > node2
}

func (s *ReplicationService) applyAsset(ch repository.Change) error {
	repoName, path, ok := splitAssetKey(ch.EntityKey)
	if !ok {
		return ErrValidation
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return err // 仓库未同步：对账兜底
	}
	if ch.Op == OpDelete {
		return s.assets.DeleteByPath(repo.ID, path)
	}
	var d AssetChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	return s.assets.Upsert(repo.ID, path, d.BlobHash, d.Size, d.ContentType, d.Sha1, d.Md5)
}

func (s *ReplicationService) applyRepository(ch repository.Change) error {
	name := strings.TrimPrefix(ch.EntityKey, keyPrefixRepo)
	existing, err := s.repos.GetByName(name)
	if ch.Op == OpDelete {
		if errors.Is(err, repository.ErrNotFound) {
			return nil // 幂等：已不存在
		}
		if err != nil {
			return err
		}
		return s.repos.Delete(name)
	}
	var d RepoChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	if existing != nil {
		if err := s.repos.UpdateVisibility(name, d.Visibility); err != nil {
			return err
		}
		if err := s.repos.UpdateDescription(name, d.Description); err != nil {
			return err
		}
		return s.repos.UpdateConfig(name, d.Config)
	}
	_, err = s.repos.Create(name, d.Format, d.Type, d.Visibility, d.Config)
	return err
}

func (s *ReplicationService) applyAcl(ch repository.Change) error {
	repoName := strings.TrimPrefix(ch.EntityKey, keyPrefixAcl)
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return err
	}
	var d AclChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	entries := make([]repository.Acl, 0, len(d.Entries))
	for _, e := range d.Entries {
		u, err := s.users.GetByUsername(e.Username)
		if errors.Is(err, repository.ErrNotFound) {
			continue // 用户尚未同步：跳过该条，对账兜底
		}
		if err != nil {
			return err
		}
		entries = append(entries, repository.Acl{SubjectID: u.ID, Action: e.Action})
	}
	return s.acls.Replace(repo.ID, entries)
}

func (s *ReplicationService) applyUser(ch repository.Change) error {
	username := strings.TrimPrefix(ch.EntityKey, keyPrefixUser)
	existing, err := s.users.GetByUsername(username)
	if ch.Op == OpDelete {
		if errors.Is(err, repository.ErrNotFound) {
			return nil // 幂等
		}
		if err != nil {
			return err
		}
		return s.users.Delete(existing.ID)
	}
	var d UserChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	if existing != nil {
		if err := s.users.Update(existing.ID, d.Role, d.Status); err != nil {
			return err
		}
		return s.users.UpdatePassword(existing.ID, d.PasswordHash)
	}
	_, err = s.users.Create(username, d.PasswordHash, d.Role)
	return err
}

func (s *ReplicationService) applyToken(ch repository.Change) error {
	digest := strings.TrimPrefix(ch.EntityKey, keyPrefixToken)
	existing, err := s.tokens.GetByDigest(digest)
	if ch.Op == OpDelete {
		if errors.Is(err, repository.ErrNotFound) {
			return nil // 幂等
		}
		if err != nil {
			return err
		}
		return s.tokens.Delete(existing.ID, existing.UserID)
	}
	if err == nil {
		return nil // 摘要已存在，幂等
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	var d TokenChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	u, err := s.users.GetByUsername(d.Username)
	if err != nil {
		return err // 用户未同步：对账兜底
	}
	_, err = s.tokens.Create(u.ID, d.Name, digest)
	return err
}

func (s *ReplicationService) applySetting(ch repository.Change) error {
	key := strings.TrimPrefix(ch.EntityKey, keyPrefixSetting)
	var d SettingChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil {
		return err
	}
	return s.settings.Set(key, d.Value)
}

// --- 自然键编解码 ---

// AssetKey 构造制品自然键：asset:<repoName>/<path>。
func AssetKey(repoName, path string) string { return keyPrefixAsset + repoName + "/" + path }

// RepoKey 构造仓库自然键：repo:<name>。
func RepoKey(name string) string { return keyPrefixRepo + name }

// AclKey 构造仓库 ACL 整仓快照自然键：acl:<repoName>。
func AclKey(repoName string) string { return keyPrefixAcl + repoName }

// UserKey 构造用户自然键：user:<username>。
func UserKey(username string) string { return keyPrefixUser + username }

// TokenKey 构造令牌自然键：token:<sha256 摘要>。
func TokenKey(digest string) string { return keyPrefixToken + digest }

// SettingKey 构造设置自然键：setting:<key>。
func SettingKey(key string) string { return keyPrefixSetting + key }

// splitAssetKey 从自然键解析仓库名与路径：asset:<repoName>/<path>。
// 仓库名不含 `/`，故以首个 `/` 分隔。
func splitAssetKey(key string) (repoName, path string, ok bool) {
	rest := strings.TrimPrefix(key, keyPrefixAsset)
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// randomHex 生成 n 字节随机数的 hex 字符串（用于节点 ID）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 不可用（理论极低）：以当前纳秒时间戳兜底，保证调用不阻塞写路径。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
