package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

// BlobSource 按哈希提供 blob 内容。签名与 blobstore.Store.Open 一致，
// 因此 *blobstore.Store 可直接满足本接口。
type BlobSource interface {
	Open(hash string) (io.ReadCloser, int64, error)
}

// ProgressFunc 在每写入一个 blob 后回调（已完成数, 总数）。
type ProgressFunc func(done, total int)

// WriteBundle 把受控快照与 blob 集合打包成 dst 处的 tar.gz。
//
// 会就地补全并返回 manifest 的 db / blobs 段（大小、摘要、集合规模），
// 因此调用方只需提供身份与计数等自己才知道的字段。
//
// manifest 永远第一个写入归档，便于导入端快速读取元数据；代价是 db 摘要必须
// 预先读完一遍快照文件（本地顺序读，相对打包本身开销可忽略）。
// 打包过程中会流式校验每个 blob 的 sha256 与其哈希名一致，源数据损坏会在
// 这里立即暴露，而不是等到导入端。
func WriteBundle(dst string, m Manifest, dbPath string, refs BlobIndex, source BlobSource, progress ProgressFunc) (Manifest, error) {
	if source == nil {
		return Manifest{}, fmt.Errorf("blob 读取器未配置")
	}
	final, err := finalizeManifest(m, dbPath, refs)
	if err != nil {
		return Manifest{}, err
	}
	manifestData, err := MarshalManifest(final)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeArchive(dst, manifestData, dbPath, refs, source, progress); err != nil {
		return Manifest{}, err
	}
	return final, nil
}

// finalizeManifest 补全 db / blobs 段并做前置校验。
func finalizeManifest(m Manifest, dbPath string, refs BlobIndex) (Manifest, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("读取快照 %s：%w", dbPath, err)
	}
	if info.IsDir() {
		return Manifest{}, fmt.Errorf("快照路径是目录：%s", dbPath)
	}
	sum, err := fileSHA256(dbPath)
	if err != nil {
		return Manifest{}, err
	}
	m.DB = DBRef{File: DBName, SizeBytes: info.Size(), SHA256: sum}
	m.Blobs = BlobsRef{
		File:        BlobIndexName,
		Count:       len(refs),
		TotalBytes:  refs.TotalBytes(),
		IndexSHA256: refs.SHA256(),
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// writeArchive 流式写出归档；任何失败都不留下半成品文件。
func writeArchive(dst string, manifestData []byte, dbPath string, refs BlobIndex, source BlobSource, progress ProgressFunc) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建归档 %s：%w", dst, err)
	}
	committed := false
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("关闭归档 %s：%w", dst, cerr)
		}
		if !committed {
			_ = os.Remove(dst)
		}
	}()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err = writeTarBytes(tw, ManifestName, manifestData); err != nil {
		return err
	}
	if err = writeTarFile(tw, DBName, dbPath); err != nil {
		return err
	}
	if err = writeTarBytes(tw, BlobIndexName, refs.Render()); err != nil {
		return err
	}
	total := len(refs)
	for i, ref := range refs {
		if err = writeTarBlob(tw, ref, source); err != nil {
			return err
		}
		if progress != nil {
			progress(i+1, total)
		}
	}

	if err = tw.Close(); err != nil {
		return fmt.Errorf("收尾 tar：%w", err)
	}
	if err = gz.Close(); err != nil {
		return fmt.Errorf("收尾 gzip：%w", err)
	}
	committed = true
	return nil
}

func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := writeTarHeader(tw, name, int64(len(data))); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("写入 %s：%w", name, err)
	}
	return nil
}

func writeTarFile(tw *tar.Writer, name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开 %s：%w", src, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读取 %s 信息：%w", src, err)
	}
	if err := writeTarHeader(tw, name, info.Size()); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("写入 %s：%w", name, err)
	}
	return nil
}

func writeTarBlob(tw *tar.Writer, ref BlobRef, source BlobSource) error {
	if !hashPattern.MatchString(ref.Hash) {
		return fmt.Errorf("blob 哈希非法：%q", ref.Hash)
	}
	rc, size, err := source.Open(ref.Hash)
	if err != nil {
		return fmt.Errorf("读取 blob %s：%w", ref.Hash, err)
	}
	defer func() { _ = rc.Close() }()

	name := blobArchivePath(ref.Hash)
	if err := writeTarHeader(tw, name, size); err != nil {
		return err
	}
	// 边写边算，确认源 blob 与其哈希名一致；不一致说明存储层已损坏。
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, h), rc); err != nil {
		return fmt.Errorf("写入 blob %s：%w", ref.Hash, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != ref.Hash {
		return fmt.Errorf("blob %s 内容摘要不符：实际 %s", ref.Hash, got)
	}
	return nil
}

func writeTarHeader(tw *tar.Writer, name string, size int64) error {
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写入 %s 头：%w", name, err)
	}
	return nil
}

// blobArchivePath 是 blob 在归档内的相对路径，与 blobstore 的两级分片一致。
func blobArchivePath(hash string) string {
	return path.Join(BlobDir, hash[0:2], hash[2:4], hash)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 %s：%w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("计算 %s 摘要：%w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
