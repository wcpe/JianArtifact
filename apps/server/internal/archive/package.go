// Package archive 定义 JianArtifact 节点备份包的归档格式与读写。
//
// 与 Nexus 迁移用的「离线包」（OPERATIONS.md §3.2 的 bundle/manifest.json + content/）
// 是两个不同概念：本包用于整机备份与搬迁，载体是 SQLite 一致性快照与内容寻址 blob。
// manifest.kind 固定为 Kind，导入端据此拒绝误喂的其他包。
//
// 归档布局（tar.gz）：
//
//	manifest.json          包元数据，永远第一个写入
//	jianartifact.db        VACUUM INTO 产出的一致性快照
//	blobs.index            每行 "<sha256> <size>"，可按序比对与求差
//	blobs/<xx>/<yy>/<hash> 仅快照库 asset 表引用到的 blob
//
// 必须排除的节点本地内容：replication-credential.key、*-wal、*-shm、
// blobs/tmp/、blobs/quarantine/。这些都不属于可搬迁的业务数据边界。
package archive

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// Kind 是本包的标识，用于与 Nexus 迁移 bundle 区分。
	Kind = "jianartifact-node-backup"
	// SchemaVersion 是当前包格式版本；导入端只接受可识别版本。
	SchemaVersion = 1

	// 归档内固定条目名。
	ManifestName  = "manifest.json"
	DBName        = "jianartifact.db"
	BlobIndexName = "blobs.index"
	BlobDir       = "blobs"
)

// Mode 表示包的生成方式：热备份不停服，冻结窗口停写。
type Mode string

const (
	ModeHot    Mode = "hot"
	ModeFrozen Mode = "frozen"
)

// hashPattern 与 blobstore 一致：内容寻址的 sha256 十六进制。
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Counts 是包内各实体的记录数，供导入端与校验端核对规模。
type Counts struct {
	Users          int `json:"users"`
	Tokens         int `json:"tokens"`
	Repositories   int `json:"repositories"`
	ACLs           int `json:"acls"`
	Assets         int `json:"assets"`
	FormatMetadata int `json:"formatMetadata"`
}

// DBRef 描述归档内 SQLite 快照。
type DBRef struct {
	File      string `json:"file"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

// BlobsRef 描述归档内 blob 索引与集合规模。
type BlobsRef struct {
	File        string `json:"file"`
	Count       int    `json:"count"`
	TotalBytes  int64  `json:"totalBytes"`
	IndexSHA256 string `json:"indexSha256"`
}

// BlobSetSummary 是 blob 集合的摘要（规模与索引摘要），用于导入端判断"基线是否已就位"
// 而无需流式扫描整个基线归档。
//
// 它是 Manifest 的可选加性字段（Expected）：全量包为 nil；差包 = 应用该差包后应有的
// 完整集合摘要（即并集的 Summary）。旧包读得进、新包语义不变——导入端对 nil 走既有
// 全量流程，非 nil 才做并集核对。
type BlobSetSummary struct {
	Count       int    `json:"count"`
	TotalBytes  int64  `json:"totalBytes"`
	IndexSHA256 string `json:"indexSha256"`
}

// summaryHexPattern 约束 IndexSHA256 必须是 64 位小写十六进制。
var summaryHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Manifest 是 manifest.json 的结构。
type Manifest struct {
	SchemaVersion   int       `json:"schemaVersion"`
	Kind            string    `json:"kind"`
	PackageID       string    `json:"packageId"`
	Mode            Mode      `json:"mode"`
	BasePackageID   string    `json:"basePackageId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	NodeID          string    `json:"nodeId"`
	AppVersion      string    `json:"appVersion"`
	DBSchemaVersion int       `json:"dbSchemaVersion"`
	Counts          Counts    `json:"counts"`
	DB              DBRef     `json:"db"`
	Blobs           BlobsRef  `json:"blobs"`
	// Expected 是可选加性字段，仅差包（BasePackageID 非空）使用：应用该差包后应有的
	// 完整 blob 集合摘要。导入端用它判断目标是否已具备基线，避免流式扫描整个基线归档。
	// 全量包恒为 nil（omitempty 时不序列化）。见 BlobSetSummary 注释。
	Expected *BlobSetSummary `json:"expected,omitempty"`
}

// IsIncremental 表示本包是在某个基线包之上生成的差包。
func (m Manifest) IsIncremental() bool { return m.BasePackageID != "" }

// Validate 校验包的必需字段与可识别版本。导入端在解包前必须先过这一关。
func (m Manifest) Validate() error {
	if m.Kind != Kind {
		return fmt.Errorf("不是节点备份包（kind=%q）", m.Kind)
	}
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("不支持的包格式版本 %d（本程序支持 %d）", m.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(m.PackageID) == "" {
		return fmt.Errorf("包缺少 packageId")
	}
	switch m.Mode {
	case ModeHot, ModeFrozen:
	default:
		return fmt.Errorf("未知的包模式 %q", m.Mode)
	}
	if m.DB.File == "" {
		return fmt.Errorf("包缺少 db.file")
	}
	if !hashPattern.MatchString(m.DB.SHA256) {
		return fmt.Errorf("db.sha256 非法：%q", m.DB.SHA256)
	}
	if m.DB.SizeBytes < 0 {
		return fmt.Errorf("db.sizeBytes 非法：%d", m.DB.SizeBytes)
	}
	if m.Blobs.File == "" {
		return fmt.Errorf("包缺少 blobs.file")
	}
	if m.Blobs.Count < 0 || m.Blobs.TotalBytes < 0 {
		return fmt.Errorf("blobs 规模非法：count=%d totalBytes=%d", m.Blobs.Count, m.Blobs.TotalBytes)
	}
	// Expected 是可选加性字段：全量包为 nil；差包必须给出合法的完整集合摘要。
	if m.Expected != nil {
		if m.Expected.Count < 0 {
			return fmt.Errorf("expected.count 非法：%d", m.Expected.Count)
		}
		if m.Expected.TotalBytes < 0 {
			return fmt.Errorf("expected.totalBytes 非法：%d", m.Expected.TotalBytes)
		}
		if m.Expected.IndexSHA256 != "" && !summaryHexPattern.MatchString(m.Expected.IndexSHA256) {
			return fmt.Errorf("expected.indexSha256 非法：%q", m.Expected.IndexSHA256)
		}
	}
	return nil
}

// MarshalManifest 序列化为带缩进的 JSON（便于人工排查）。
func MarshalManifest(m Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 manifest：%w", err)
	}
	return append(data, '\n'), nil
}

// UnmarshalManifest 解析 manifest.json 并校验必需字段。
func UnmarshalManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("解析 manifest：%w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// BlobRef 是 blob 集合中的一项。
type BlobRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// BlobIndex 是按哈希排序去重的 blob 集合。
// 顺序确定性是增量求差与摘要稳定的前提。
type BlobIndex []BlobRef

// NewBlobIndex 规范化输入：按哈希升序排序并去重（同哈希保留首次出现）。
func NewBlobIndex(refs []BlobRef) BlobIndex {
	seen := make(map[string]struct{}, len(refs))
	out := make(BlobIndex, 0, len(refs))
	for _, ref := range refs {
		if _, ok := seen[ref.Hash]; ok {
			continue
		}
		seen[ref.Hash] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// ParseBlobIndex 解析 blobs.index 文本；空行被忽略，非法行报错。
func ParseBlobIndex(r io.Reader) (BlobIndex, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	refs := make(BlobIndex, 0, 1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		hash, size, err := parseIndexLine(text)
		if err != nil {
			return nil, fmt.Errorf("blobs.index 第 %d 行：%w", line, err)
		}
		refs = append(refs, BlobRef{Hash: hash, Size: size})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 blobs.index：%w", err)
	}
	return NewBlobIndex(refs), nil
}

func parseIndexLine(text string) (string, int64, error) {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("期望 \"<sha256> <size>\"，实际 %q", text)
	}
	if !hashPattern.MatchString(fields[0]) {
		return "", 0, fmt.Errorf("哈希非法：%q", fields[0])
	}
	var size int64
	if _, err := fmt.Sscanf(fields[1], "%d", &size); err != nil {
		return "", 0, fmt.Errorf("大小非法：%q", fields[1])
	}
	if size < 0 {
		return "", 0, fmt.Errorf("大小不能为负：%d", size)
	}
	return fields[0], size, nil
}

// Render 输出确定性的索引文本，用于写入归档与计算摘要。
func (idx BlobIndex) Render() []byte {
	var b strings.Builder
	b.Grow(len(idx) * 76)
	for _, ref := range idx {
		b.WriteString(ref.Hash)
		b.WriteByte(' ')
		b.WriteString(fmt.Sprintf("%d", ref.Size))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// WriteTo 把索引写入 w，返回写入字节数。
func (idx BlobIndex) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(idx.Render())
	return int64(n), err
}

// SHA256 是索引文本的 sha256，用于跨节点比对集合是否一致。
func (idx BlobIndex) SHA256() string {
	sum := sha256.Sum256(idx.Render())
	return hex.EncodeToString(sum[:])
}

// TotalBytes 是集合内 blob 字节总量。
func (idx BlobIndex) TotalBytes() int64 {
	var total int64
	for _, ref := range idx {
		total += ref.Size
	}
	return total
}

// Hashes 返回哈希集合，便于 O(1) 判定。
func (idx BlobIndex) Hashes() map[string]struct{} {
	out := make(map[string]struct{}, len(idx))
	for _, ref := range idx {
		out[ref.Hash] = struct{}{}
	}
	return out
}

// Contains 判断哈希是否在集合内（线性扫描；批量判定请先取 Hashes）。
func (idx BlobIndex) Contains(hash string) bool {
	for _, ref := range idx {
		if ref.Hash == hash {
			return true
		}
	}
	return false
}
