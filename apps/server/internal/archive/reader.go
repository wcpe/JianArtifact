package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxManifestBytes 限制 manifest 读取上限，避免恶意归档用超大 manifest 撑爆内存。
const maxManifestBytes = 4 << 20

// blobEntryPattern 严格约束归档内 blob 条目名，兼作路径穿越防护。
var blobEntryPattern = regexp.MustCompile(`^blobs/([0-9a-f]{2})/([0-9a-f]{2})/([0-9a-f]{64})$`)

// errStopWalk 是遍历提前结束的内部哨兵。
var errStopWalk = errors.New("停止遍历归档")

// Reader 读取节点备份包。
//
// tar.gz 不支持随机寻址，因此 Open 只缓存 manifest（4MB 上限）；完整性校验与解包
// 都需要重新流式读取一遍归档。对本用途（单次迁移）这是可接受的取舍。
type Reader struct {
	path string
	m    Manifest
}

// Open 读取归档的 manifest 并校验格式版本与必需字段；不校验内容摘要。
func Open(filePath string) (*Reader, Manifest, error) {
	var (
		m     Manifest
		found bool
	)
	err := walkArchive(filePath, func(hdr *tar.Header, tr *tar.Reader) error {
		if hdr.Name != ManifestName {
			return nil
		}
		if hdr.Size > maxManifestBytes {
			return fmt.Errorf("manifest 过大（%d 字节）", hdr.Size)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
		if err != nil {
			return fmt.Errorf("读取 manifest：%w", err)
		}
		if m, err = UnmarshalManifest(data); err != nil {
			return err
		}
		found = true
		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, Manifest{}, err
	}
	if !found {
		return nil, Manifest{}, fmt.Errorf("归档缺少 %s", ManifestName)
	}
	return &Reader{path: filePath, m: m}, m, nil
}

// Manifest 返回已缓存的包元数据。
func (r *Reader) Manifest() Manifest { return r.m }

// Verify 校验归档内容。
//
// deep=false：校验 manifest 一致性、db 大小与摘要、blobs.index 摘要与规模。
// deep=true：额外逐个校验 blob 内容与其哈希名一致（全量读取，耗时与包等大）。
func (r *Reader) Verify(deep bool) error {
	var (
		seenManifest, seenDB, seenIndex bool
		blobsSeen                       int
		blobsBytes                      int64
	)
	err := walkArchive(r.path, func(hdr *tar.Header, tr *tar.Reader) error {
		switch {
		case hdr.Name == ManifestName:
			data, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
			if err != nil {
				return fmt.Errorf("读取 manifest：%w", err)
			}
			m, err := UnmarshalManifest(data)
			if err != nil {
				return err
			}
			if m.PackageID != r.m.PackageID {
				return fmt.Errorf("manifest 前后不一致：%s / %s", m.PackageID, r.m.PackageID)
			}
			seenManifest = true
			return nil

		case hdr.Name == DBName:
			sum, size, err := hashStream(tr)
			if err != nil {
				return err
			}
			if size != r.m.DB.SizeBytes {
				return fmt.Errorf("db 大小不符：manifest %d 实际 %d", r.m.DB.SizeBytes, size)
			}
			if sum != r.m.DB.SHA256 {
				return fmt.Errorf("db 摘要不符：manifest %s 实际 %s", r.m.DB.SHA256, sum)
			}
			seenDB = true
			return nil

		case hdr.Name == BlobIndexName:
			h := sha256.New()
			idx, err := ParseBlobIndex(io.TeeReader(tr, h))
			if err != nil {
				return err
			}
			if got := hex.EncodeToString(h.Sum(nil)); got != r.m.Blobs.IndexSHA256 {
				return fmt.Errorf("blobs.index 摘要不符：manifest %s 实际 %s", r.m.Blobs.IndexSHA256, got)
			}
			if len(idx) != r.m.Blobs.Count {
				return fmt.Errorf("blob 数量不符：manifest %d 实际 %d", r.m.Blobs.Count, len(idx))
			}
			if total := idx.TotalBytes(); total != r.m.Blobs.TotalBytes {
				return fmt.Errorf("blob 总字节不符：manifest %d 实际 %d", r.m.Blobs.TotalBytes, total)
			}
			seenIndex = true
			return nil

		default:
			hash, err := parseBlobEntry(hdr.Name)
			if err != nil {
				return err
			}
			blobsSeen++
			blobsBytes += hdr.Size
			if !deep {
				return nil
			}
			sum, size, err := hashStream(tr)
			if err != nil {
				return err
			}
			if size != hdr.Size {
				return fmt.Errorf("blob %s 长度不符：声明 %d 实际 %d", hash, hdr.Size, size)
			}
			if sum != hash {
				return fmt.Errorf("blob %s 内容摘要不符：实际 %s", hash, sum)
			}
			return nil
		}
	})
	if err != nil {
		return err
	}
	if !seenManifest || !seenDB || !seenIndex {
		return fmt.Errorf("归档结构不完整（manifest=%t db=%t blobs.index=%t)", seenManifest, seenDB, seenIndex)
	}
	if blobsSeen != r.m.Blobs.Count {
		return fmt.Errorf("归档内 blob 条目数不符：manifest %d 实际 %d", r.m.Blobs.Count, blobsSeen)
	}
	if blobsBytes != r.m.Blobs.TotalBytes {
		return fmt.Errorf("归档内 blob 总字节不符：manifest %d 实际 %d", r.m.Blobs.TotalBytes, blobsBytes)
	}
	return nil
}

// BlobIndex 读取归档内的 blob 集合。
func (r *Reader) BlobIndex() (BlobIndex, error) {
	var idx BlobIndex
	found := false
	err := walkArchive(r.path, func(hdr *tar.Header, tr *tar.Reader) error {
		if hdr.Name != BlobIndexName {
			return nil
		}
		parsed, err := ParseBlobIndex(tr)
		if err != nil {
			return err
		}
		idx = parsed
		found = true
		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("归档缺少 %s", BlobIndexName)
	}
	return idx, nil
}

// Extract 把归档解包到 dstDir：db 与 blobs.index 总是落盘。
//
// onBlob 为 nil 时 blob 写入 dstDir/blobs/...；非 nil 时 blob 交给回调消费
// （供导入端直接合并进 blobstore，避免中间落地一份完整副本）。
func (r *Reader) Extract(dstDir string, onBlob func(ref BlobRef, size int64, rc io.Reader) error) error {
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("创建解包目录 %s：%w", dstDir, err)
	}
	return walkArchive(r.path, func(hdr *tar.Header, tr *tar.Reader) error {
		switch {
		case hdr.Name == ManifestName:
			return nil
		case hdr.Name == DBName:
			return writeStreamToFile(filepath.Join(dstDir, DBName), hdr.Size, tr)
		case hdr.Name == BlobIndexName:
			return writeStreamToFile(filepath.Join(dstDir, BlobIndexName), hdr.Size, tr)
		default:
			hash, err := parseBlobEntry(hdr.Name)
			if err != nil {
				return err
			}
			if onBlob != nil {
				return onBlob(BlobRef{Hash: hash, Size: hdr.Size}, hdr.Size, tr)
			}
			target := filepath.Join(dstDir, filepath.FromSlash(hdr.Name))
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("创建 blob 目录：%w", err)
			}
			return writeStreamToFile(target, hdr.Size, tr)
		}
	})
}

// walkArchive 逐条目遍历归档。
func walkArchive(filePath string, fn func(hdr *tar.Header, tr *tar.Reader) error) (err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开归档 %s：%w", filePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压归档 %s：%w", filePath, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取归档条目：%w", err)
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// parseBlobEntry 校验 blob 条目名并返回其中的哈希。
func parseBlobEntry(name string) (string, error) {
	m := blobEntryPattern.FindStringSubmatch(name)
	if m == nil {
		return "", fmt.Errorf("归档包含非法条目 %q", name)
	}
	if !strings.HasPrefix(m[3], m[1]+m[2]) {
		return "", fmt.Errorf("blob 条目分片路径与哈希不符：%q", name)
	}
	return m[3], nil
}

func hashStream(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("计算摘要：%w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeStreamToFile(target string, size int64, r io.Reader) (err error) {
	f, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("创建 %s：%w", target, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("关闭 %s：%w", target, cerr)
		}
	}()
	n, err := io.Copy(f, r)
	if err != nil {
		return fmt.Errorf("写入 %s：%w", target, err)
	}
	if n != size {
		return fmt.Errorf("%s 长度不符：声明 %d 实际 %d", target, size, n)
	}
	return nil
}
