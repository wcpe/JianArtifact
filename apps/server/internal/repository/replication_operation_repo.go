package repository

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ReplicationVersion 是 operation item 的统一版本元组。
type ReplicationVersion struct {
	NodeID string `json:"nodeId"`
	TS     string `json:"ts"`
}

// OperationActor 是源端发起 operation 时固化的身份快照，不携带凭据。
type OperationActor struct {
	Username   string `json:"username"`
	UserID     *int64 `json:"userId,omitempty"`
	AuthSource string `json:"authSource"`
}

// OperationItem 是原子操作中的一个逻辑实体，数据格式与普通 Change 一致。
type OperationItem struct {
	Type       string             `json:"type"`
	Key        string             `json:"key"`
	Op         string             `json:"op"`
	Data       string             `json:"data"`
	BlobHashes []string           `json:"blobHashes,omitempty"`
	DependsOn  []string           `json:"dependsOn,omitempty"`
	Version    ReplicationVersion `json:"version"`
}

// OperationEnvelope 是 v2 复制流中的不可分割 operation 记录。
type OperationEnvelope struct {
	Kind             string             `json:"kind"`
	Seq              int64              `json:"seq"`
	StreamGeneration string             `json:"streamGeneration,omitempty"`
	OperationID      string             `json:"operationId"`
	SourceNode       string             `json:"sourceNode"`
	Version          ReplicationVersion `json:"version"`
	ItemCount        int                `json:"itemCount"`
	ManifestSHA256   string             `json:"manifestSha256"`
	Actor            OperationActor     `json:"actor"`
	Items            []OperationItem    `json:"items"`
}

// MaxOperationItems 是一个原子操作在复制通道中可传输的最大制品变更数。
// 一次最多 500 个实际制品；移动会为每个制品产生删除与写入两项，故传输上限为 1000。
const MaxOperationItems = 1000

// Change 是 repl_change 变更日志的一行。
// 复制退役后该表不再写入，但 v2 records 读模型（operation 回执核对）仍引用此结构。
type Change struct {
	Seq        int64  `db:"seq" json:"seq"`
	NodeID     string `db:"node_id" json:"nodeId"`
	Op         string `db:"op" json:"op"` // put | delete
	EntityType string `db:"entity_type" json:"entityType"`
	EntityKey  string `db:"entity_key" json:"entityKey"`
	Data       string `db:"data" json:"data"` // 变更后数据 JSON；delete 时为 tombstone
	TS         string `db:"ts" json:"ts"`     // 写入节点本地时钟（RFC3339Nano）
}

// ReplicationRecord 是 v2 records 的联合元素。
type ReplicationRecord struct {
	Type             string             `json:"type"` // change | operation
	Seq              int64              `json:"seq"`
	StreamGeneration string             `json:"streamGeneration,omitempty"`
	Change           *Change            `json:"change,omitempty"`
	Operation        *OperationEnvelope `json:"operation,omitempty"`
}

// ReplicationOperationRepo 读写 operation outbox。
type ReplicationOperationRepo struct{ db *persistence.DB }

// NewReplicationOperationRepo 构造 operation outbox 仓储。
func NewReplicationOperationRepo(db *persistence.DB) *ReplicationOperationRepo {
	return &ReplicationOperationRepo{db: db}
}

// Append 在传入事务中追加一个 operation 标记与完整 outbox，二者共享 repl_change.seq。
// 调用方必须把业务完成状态放在同一个事务中，以保证完成视图与 outbox 不可分割。
func (r *ReplicationOperationRepo) Append(tx *sqlx.Tx, envelope OperationEnvelope) (int64, error) {
	if err := validateEnvelope(envelope); err != nil {
		return 0, err
	}
	itemsJSON, err := json.Marshal(envelope.Items)
	if err != nil {
		return 0, fmt.Errorf("序列化操作清单：%w", err)
	}
	manifest := sha256.Sum256(itemsJSON)
	manifestHex := hex.EncodeToString(manifest[:])
	if envelope.ManifestSHA256 != "" && envelope.ManifestSHA256 != manifestHex {
		return 0, errors.New("操作清单摘要不匹配")
	}
	result, err := tx.Exec(`INSERT INTO repl_change (node_id, op, entity_type, entity_key, data, ts)
		VALUES (?, 'operation', 'operation', ?, ?, ?)`, envelope.SourceNode, envelope.OperationID,
		string(itemsJSON), envelope.Version.TS)
	if err != nil {
		return 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO replication_operation_outbox
		(seq, operation_id, source_node, version_node, version_ts, item_count, manifest_sha256, actor, actor_user_id, actor_auth_source, items_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, seq, envelope.OperationID, envelope.SourceNode,
		envelope.Version.NodeID, envelope.Version.TS, len(envelope.Items), manifestHex, envelope.Actor.Username,
		envelope.Actor.UserID, envelope.Actor.AuthSource, string(itemsJSON)); err != nil {
		return 0, err
	}
	return seq, nil
}

// AppendStandalone 追加一个已完成操作 outbox；业务调用方应优先使用事务版本。
func (r *ReplicationOperationRepo) AppendStandalone(envelope OperationEnvelope) (int64, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := r.Append(tx, envelope)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// GetBySeq 读取指定 seq 的完整 operation。
func (r *ReplicationOperationRepo) GetBySeq(seq int64) (*OperationEnvelope, error) {
	var row operationRow
	if err := r.db.Get(&row, `SELECT seq, operation_id, source_node, version_node, version_ts,
		item_count, manifest_sha256, actor, actor_user_id, actor_auth_source, items_json FROM replication_operation_outbox WHERE seq=?`, seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return row.envelope()
}

// HasAfter 判断 since 之后是否已有 completed operation envelope。
func (r *ReplicationOperationRepo) HasAfter(since int64) (bool, error) {
	var count int
	if err := r.db.Get(&count, `SELECT COUNT(*) FROM replication_operation_outbox WHERE seq>?`, since); err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListRecordsSince 返回 v2 联合 records；limit 按 record 计数，绝不拆 operation。
func (r *ReplicationOperationRepo) ListRecordsSince(since int64, limit int) ([]ReplicationRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	var rows []Change
	if err := r.db.Select(&rows, `SELECT seq, node_id, op, entity_type, entity_key, data, ts
		FROM repl_change WHERE seq>? ORDER BY seq LIMIT ?`, since, limit); err != nil {
		return nil, err
	}
	records := make([]ReplicationRecord, 0, len(rows))
	for _, row := range rows {
		if row.Op == "operation" && row.EntityType == "operation" {
			op, err := r.GetBySeq(row.Seq)
			if err != nil {
				return nil, fmt.Errorf("读取 operation outbox seq=%d：%w", row.Seq, err)
			}
			op.Seq = row.Seq
			records = append(records, ReplicationRecord{Type: "operation", Seq: row.Seq, Operation: op})
			continue
		}
		rowCopy := row
		records = append(records, ReplicationRecord{Type: "change", Seq: row.Seq, Change: &rowCopy})
	}
	return records, nil
}

// HasMoreAfter 判断指定 record 游标后是否还有记录。
func (r *ReplicationOperationRepo) HasMoreAfter(seq int64) (bool, error) {
	var count int
	if err := r.db.Get(&count, `SELECT COUNT(*) FROM repl_change WHERE seq>?`, seq); err != nil {
		return false, err
	}
	return count > 0, nil
}

func validateEnvelope(envelope OperationEnvelope) error {
	if envelope.OperationID == "" || envelope.SourceNode == "" || envelope.Version.NodeID == "" || envelope.Version.TS == "" {
		return errors.New("operation envelope 缺少标识或版本")
	}
	if len(envelope.Items) == 0 || len(envelope.Items) > MaxOperationItems {
		return fmt.Errorf("operation 清单数量必须为 1 到 %d", MaxOperationItems)
	}
	if envelope.ItemCount != len(envelope.Items) {
		return errors.New("operation itemCount 与清单数量不一致")
	}
	seen := make(map[string]struct{}, len(envelope.Items))
	previous := ""
	for _, item := range envelope.Items {
		if item.Type == "" || item.Key == "" || (item.Op != "put" && item.Op != "delete") {
			return errors.New("operation item 字段非法")
		}
		if item.Version.NodeID != envelope.Version.NodeID || item.Version.TS != envelope.Version.TS {
			return errors.New("operation item 版本与 envelope 不一致")
		}
		identity := item.Type + "\x00" + item.Key
		if _, exists := seen[identity]; exists {
			return errors.New("operation 清单包含重复实体")
		}
		seen[identity] = struct{}{}
		if previous != "" && previous >= identity {
			return errors.New("operation 清单未按实体键稳定排序")
		}
		previous = identity
		if err := validateItemDependencies(item.DependsOn); err != nil {
			return err
		}
		if err := validateBlobHashes(item.BlobHashes); err != nil {
			return err
		}
		if err := validateAssetPutBlob(item); err != nil {
			return err
		}
	}
	return nil
}

// validateAssetPutBlob 确保资产写入的实际 blob 与预取清单完全一致，避免空哈希
// 或无关哈希让接收端先暴露没有内容的制品元数据。
func validateAssetPutBlob(item OperationItem) error {
	if item.Type != "asset" || item.Op != "put" {
		return nil
	}
	var data struct {
		BlobHash string `json:"blobHash"`
	}
	if err := json.Unmarshal([]byte(item.Data), &data); err != nil {
		return errors.New("operation 资产数据非法")
	}
	if !validBlobHash(data.BlobHash) {
		return errors.New("operation 资产 blob 哈希非法")
	}
	if len(item.BlobHashes) != 1 || item.BlobHashes[0] != data.BlobHash {
		return errors.New("operation 资产 blob 清单必须且只能声明实际哈希")
	}
	return nil
}

func validateItemDependencies(dependencies []string) error {
	seen := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if strings.TrimSpace(dependency) == "" {
			return errors.New("operation 依赖键不能为空")
		}
		if _, exists := seen[dependency]; exists {
			return errors.New("operation 依赖键重复")
		}
		seen[dependency] = struct{}{}
	}
	return nil
}

func validateBlobHashes(hashes []string) error {
	seen := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		if !validBlobHash(hash) {
			return errors.New("operation blob 哈希非法")
		}
		if _, exists := seen[hash]; exists {
			return errors.New("operation blob 哈希重复")
		}
		seen[hash] = struct{}{}
	}
	return nil
}

func validBlobHash(hash string) bool {
	return len(hash) == 64 && strings.Trim(hash, "0123456789abcdef") == ""
}

// ValidateOperationEnvelope 校验 v2 operation 的边界与清单字段。
func ValidateOperationEnvelope(envelope OperationEnvelope) error { return validateEnvelope(envelope) }

// OperationManifestSHA256 返回按传输顺序计算的 operation 清单摘要。
func OperationManifestSHA256(items []OperationItem) (string, error) {
	raw, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

type operationRow struct {
	Seq             int64  `db:"seq"`
	OperationID     string `db:"operation_id"`
	SourceNode      string `db:"source_node"`
	VersionNode     string `db:"version_node"`
	VersionTS       string `db:"version_ts"`
	ItemCount       int    `db:"item_count"`
	ManifestSHA256  string `db:"manifest_sha256"`
	Actor           string `db:"actor"`
	ActorUserID     *int64 `db:"actor_user_id"`
	ActorAuthSource string `db:"actor_auth_source"`
	ItemsJSON       string `db:"items_json"`
}

func (r operationRow) envelope() (*OperationEnvelope, error) {
	var items []OperationItem
	if err := json.Unmarshal([]byte(r.ItemsJSON), &items); err != nil {
		return nil, fmt.Errorf("解析 operation 清单：%w", err)
	}
	return &OperationEnvelope{Kind: "operation", Seq: r.Seq, OperationID: r.OperationID,
		SourceNode: r.SourceNode, Version: ReplicationVersion{NodeID: r.VersionNode, TS: r.VersionTS},
		ItemCount: r.ItemCount, ManifestSHA256: r.ManifestSHA256,
		Actor: OperationActor{Username: r.Actor, UserID: r.ActorUserID, AuthSource: r.ActorAuthSource}, Items: items}, nil
}
