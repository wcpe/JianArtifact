package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mapBlobSource 是测试用的内存 blob 源。
type mapBlobSource struct{ data map[string][]byte }

func (s mapBlobSource) Open(hash string) (io.ReadCloser, int64, error) {
	b, ok := s.data[hash]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newFixture 构造 db 快照与两个 blob 的测试夹具。
func newFixture(t *testing.T) (dbPath string, refs BlobIndex, source mapBlobSource, dbBytes []byte) {
	t.Helper()
	dir := t.TempDir()

	dbBytes = []byte("fake-sqlite-snapshot-payload")
	dbPath = filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		t.Fatalf("写 db 快照：%v", err)
	}

	source = mapBlobSource{data: map[string][]byte{}}
	raw := [][]byte{[]byte("blob-one"), []byte("blob-two-longer-payload")}
	list := make([]BlobRef, 0, len(raw))
	for _, b := range raw {
		h := hashOf(b)
		source.data[h] = b
		list = append(list, BlobRef{Hash: h, Size: int64(len(b))})
	}
	return dbPath, NewBlobIndex(list), source, dbBytes
}

func baseManifest() Manifest {
	return Manifest{
		SchemaVersion:   SchemaVersion,
		Kind:            Kind,
		PackageID:       "bk-test-0001",
		Mode:            ModeHot,
		NodeID:          "np-test",
		AppVersion:      "0.7.1",
		DBSchemaVersion: 33,
		CreatedAt:       time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Counts:          Counts{Users: 1, Repositories: 2, Assets: 3},
	}
}

func TestWriteBundleRoundTrip(t *testing.T) {
	dbPath, refs, source, dbBytes := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")

	var lastDone, lastTotal int
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, func(done, total int) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	if lastDone != len(refs) || lastTotal != len(refs) {
		t.Fatalf("进度回调 = %d/%d，期望 %d/%d", lastDone, lastTotal, len(refs), len(refs))
	}

	// manifest 的 db / blobs 段应被补全。
	if final.DB.File != DBName || final.DB.SizeBytes != int64(len(dbBytes)) || final.DB.SHA256 != hashOf(dbBytes) {
		t.Fatalf("db 段补全有误：%+v", final.DB)
	}
	if final.Blobs.Count != len(refs) || final.Blobs.TotalBytes != refs.TotalBytes() || final.Blobs.IndexSHA256 != refs.SHA256() {
		t.Fatalf("blobs 段补全有误：%+v", final.Blobs)
	}

	r, m, err := Open(dst)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if m.PackageID != final.PackageID || m.DB.SHA256 != final.DB.SHA256 {
		t.Fatalf("Open 读回的 manifest 与写入不符：%+v", m)
	}
	if m.IsIncremental() {
		t.Fatal("全量包不应被视为增量包")
	}
	if err := r.Verify(false); err != nil {
		t.Fatalf("Verify(false)：%v", err)
	}
	if err := r.Verify(true); err != nil {
		t.Fatalf("Verify(true)：%v", err)
	}

	idx, err := r.BlobIndex()
	if err != nil {
		t.Fatalf("BlobIndex：%v", err)
	}
	if len(idx) != len(refs) || idx.SHA256() != refs.SHA256() {
		t.Fatalf("读回的 blob 集合与写入不符")
	}

	outDir := filepath.Join(t.TempDir(), "extract")
	if err := r.Extract(outDir, nil); err != nil {
		t.Fatalf("Extract：%v", err)
	}
	gotDB, err := os.ReadFile(filepath.Join(outDir, DBName))
	if err != nil {
		t.Fatalf("读取解包 db：%v", err)
	}
	if !bytes.Equal(gotDB, dbBytes) {
		t.Fatal("解包 db 内容与原始不符")
	}
	for _, ref := range refs {
		got, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(blobArchivePath(ref.Hash))))
		if err != nil {
			t.Fatalf("读取解包 blob %s：%v", ref.Hash, err)
		}
		if !bytes.Equal(got, source.data[ref.Hash]) {
			t.Fatalf("解包 blob %s 内容不符", ref.Hash)
		}
	}
}

// TestExtractHonoursOnBlob 验证回调模式下 blob 交由调用方消费而非落盘。
func TestExtractHonoursOnBlob(t *testing.T) {
	dbPath, refs, source, _ := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if _, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil); err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	r, _, err := Open(dst)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}

	outDir := filepath.Join(t.TempDir(), "extract")
	seen := map[string]string{}
	if err := r.Extract(outDir, func(ref BlobRef, size int64, rc io.Reader) error {
		body, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		if int64(len(body)) != size {
			t.Fatalf("回调收到 %d 字节，声明 %d", len(body), size)
		}
		seen[ref.Hash] = string(body)
		return nil
	}); err != nil {
		t.Fatalf("Extract：%v", err)
	}
	if len(seen) != len(refs) {
		t.Fatalf("回调收到 %d 个 blob，期望 %d", len(seen), len(refs))
	}
	// 回调模式下不应把 blob 落到磁盘。
	if _, err := os.Stat(filepath.Join(outDir, BlobDir)); !os.IsNotExist(err) {
		t.Fatal("回调模式下不应创建 blobs 目录")
	}
}

type tarEntry struct {
	name string
	body []byte
}

func buildArchive(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建归档：%v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写条目头 %s：%v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("写条目 %s：%v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("收尾 tar：%v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("收尾 gzip：%v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭归档：%v", err)
	}
}

// TestVerifyDetectsWrongDBDigest 验证 db 摘要不符会被拒绝。
func TestVerifyDetectsWrongDBDigest(t *testing.T) {
	dbPath, refs, source, _ := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil)
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	// 篡改 manifest 里的 db 摘要后重打包，Verify 必须发现内容与声明不符。
	final.DB.SHA256 = strings.Repeat("0", 64)
	data, err := MarshalManifest(final)
	if err != nil {
		t.Fatalf("MarshalManifest：%v", err)
	}
	tampered := filepath.Join(t.TempDir(), "tampered.tar.gz")
	buildArchive(t, tampered, []tarEntry{
		{name: ManifestName, body: data},
		{name: DBName, body: mustRead(t, dbPath)},
		{name: BlobIndexName, body: refs.Render()},
	})

	r, _, err := Open(tampered)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if err := r.Verify(false); err == nil {
		t.Fatal("db 摘要不符时 Verify 应当报错")
	}
}

// TestVerifyDetectsCorruptedBlob 验证 blob 内容与哈希不符时，
// 浅校验放过、深校验必须拦下。
func TestVerifyDetectsCorruptedBlob(t *testing.T) {
	dbPath, refs, source, dbBytes := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil)
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	data, err := MarshalManifest(final)
	if err != nil {
		t.Fatalf("MarshalManifest：%v", err)
	}
	corrupt := refs[0]
	// 同长度、不同内容：这样大小类检查（浅校验）无法察觉，只有逐 blob 摘要才能发现。
	orig := source.data[corrupt.Hash]
	tamperedBody := make([]byte, len(orig))
	copy(tamperedBody, orig)
	tamperedBody[0] ^= 0xFF

	entries := []tarEntry{
		{name: ManifestName, body: data},
		{name: DBName, body: dbBytes},
		{name: BlobIndexName, body: refs.Render()},
		{name: blobArchivePath(corrupt.Hash), body: tamperedBody},
	}
	for _, ref := range refs[1:] {
		entries = append(entries, tarEntry{name: blobArchivePath(ref.Hash), body: source.data[ref.Hash]})
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	buildArchive(t, bad, entries)

	r, _, err := Open(bad)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if err := r.Verify(false); err != nil {
		t.Fatalf("浅校验应放过（只查索引与 db）：%v", err)
	}
	if err := r.Verify(true); err == nil {
		t.Fatal("blob 内容损坏时深校验应当报错")
	}
}

// TestVerifyDetectsBlobSizeMismatch 验证 blob 条目长度与索引声明不符时，
// 浅校验即可发现（无需逐块比对内容）。
func TestVerifyDetectsBlobSizeMismatch(t *testing.T) {
	dbPath, refs, source, dbBytes := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil)
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	data, err := MarshalManifest(final)
	if err != nil {
		t.Fatalf("MarshalManifest：%v", err)
	}
	entries := []tarEntry{
		{name: ManifestName, body: data},
		{name: DBName, body: dbBytes},
		{name: BlobIndexName, body: refs.Render()},
	}
	for i, ref := range refs {
		body := source.data[ref.Hash]
		if i == 0 {
			body = append([]byte("x"), body...) // 长度改变
		}
		entries = append(entries, tarEntry{name: blobArchivePath(ref.Hash), body: body})
	}
	bad := filepath.Join(t.TempDir(), "sizemismatch.tar.gz")
	buildArchive(t, bad, entries)

	r, _, err := Open(bad)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if err := r.Verify(false); err == nil {
		t.Fatal("blob 长度不符时浅校验应当报错")
	}
}

// TestOpenRejectsForeignKind 验证不是节点备份包（例如 Nexus 迁移 bundle）会被拒绝。
func TestOpenRejectsForeignKind(t *testing.T) {
	m := baseManifest()
	m.Kind = "nexus-migration-bundle"
	data, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest：%v", err)
	}
	p := filepath.Join(t.TempDir(), "foreign.tar.gz")
	buildArchive(t, p, []tarEntry{{name: ManifestName, body: data}})

	if _, _, err := Open(p); err == nil {
		t.Fatal("异种包应当被 Open 拒绝")
	}
}

// TestExtractRejectsTraversalEntry 验证归档内非法条目名（路径穿越）被拒绝。
func TestExtractRejectsTraversalEntry(t *testing.T) {
	dbPath, refs, source, _ := newFixture(t)
	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil)
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	data, err := MarshalManifest(final)
	if err != nil {
		t.Fatalf("MarshalManifest：%v", err)
	}
	evil := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, evil, []tarEntry{
		{name: ManifestName, body: data},
		{name: DBName, body: mustRead(t, dbPath)},
		{name: BlobIndexName, body: refs.Render()},
		{name: "blobs/../../etc/passwd", body: []byte("pwned")},
	})

	r, _, err := Open(evil)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if err := r.Extract(t.TempDir(), nil); err == nil {
		t.Fatal("路径穿越条目应当被拒绝")
	}
}

func TestBlobSetDiff(t *testing.T) {
	mk := func(parts ...string) BlobIndex {
		refs := make([]BlobRef, 0, len(parts))
		for _, p := range parts {
			refs = append(refs, BlobRef{Hash: hashOf([]byte(p)), Size: int64(len(p))})
		}
		return NewBlobIndex(refs)
	}
	base := mk("a", "b")
	cur := mk("a", "b", "c", "d")

	diff := BlobSetDiff(base, cur)
	if len(diff) != 2 {
		t.Fatalf("差集大小 = %d，期望 2", len(diff))
	}
	wantC, wantD := hashOf([]byte("c")), hashOf([]byte("d"))
	if !diff.Contains(wantC) || !diff.Contains(wantD) {
		t.Fatalf("差集内容有误：%+v", diff)
	}

	// 反向：base 里所有内容都已在 cur 中缺失检测。
	if missing := base.MissingFrom(cur); len(missing) != 0 {
		t.Fatalf("base 全部存在于 cur，缺失集应为空，实际 %d", len(missing))
	}
	if missing := cur.MissingFrom(base); len(missing) != 2 {
		t.Fatalf("缺失集大小 = %d，期望 2", len(missing))
	}
}

func TestBlobIndexDeterministicAndDedup(t *testing.T) {
	a := hashOf([]byte("a"))
	b := hashOf([]byte("b"))
	idx := NewBlobIndex([]BlobRef{{Hash: b, Size: 2}, {Hash: a, Size: 1}, {Hash: b, Size: 2}})
	if len(idx) != 2 {
		t.Fatalf("去重后大小 = %d，期望 2", len(idx))
	}
	if idx[0].Hash > idx[1].Hash {
		t.Fatal("索引未按哈希升序排序")
	}
	// 同一集合的不同输入顺序必须产生相同摘要。
	reordered := NewBlobIndex([]BlobRef{{Hash: a, Size: 1}, {Hash: b, Size: 2}})
	if idx.SHA256() != reordered.SHA256() {
		t.Fatal("索引摘要对输入顺序不稳定")
	}
}

func TestManifestValidateRejectsIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"异种 kind", func(m *Manifest) { m.Kind = "other" }},
		{"未知版本", func(m *Manifest) { m.SchemaVersion = 99 }},
		{"缺 packageId", func(m *Manifest) { m.PackageID = "" }},
		{"未知模式", func(m *Manifest) { m.Mode = "weird" }},
		{"缺 db 文件", func(m *Manifest) { m.DB.File = "" }},
		{"db 摘要非法", func(m *Manifest) { m.DB.SHA256 = "nothex" }},
		{"db 大小为负", func(m *Manifest) { m.DB.SizeBytes = -1 }},
		{"缺 blob 索引", func(m *Manifest) { m.Blobs.File = "" }},
		{"blob 计数为负", func(m *Manifest) { m.Blobs.Count = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseManifest()
			m.DB = DBRef{File: DBName, SizeBytes: 1, SHA256: strings.Repeat("a", 64)}
			m.Blobs = BlobsRef{File: BlobIndexName}
			tc.mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("非法 manifest 应当被拒绝")
			}
		})
	}

	valid := baseManifest()
	valid.DB = DBRef{File: DBName, SizeBytes: 1, SHA256: strings.Repeat("a", 64)}
	valid.Blobs = BlobsRef{File: BlobIndexName}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 manifest 不应报错：%v", err)
	}
}

func TestParseBlobIndexRejectsMalformed(t *testing.T) {
	cases := []string{
		"deadbeef 12\n",
		hashOf([]byte("x")) + " notanumber\n",
		hashOf([]byte("x")) + "\n",
		hashOf([]byte("x")) + " -5\n",
	}
	for _, c := range cases {
		if _, err := ParseBlobIndex(strings.NewReader(c)); err == nil {
			t.Fatalf("非法索引行应报错：%q", c)
		}
	}
	ok := hashOf([]byte("x")) + " 1\n\n  \n"
	idx, err := ParseBlobIndex(strings.NewReader(ok))
	if err != nil {
		t.Fatalf("合法索引解析失败：%v", err)
	}
	if len(idx) != 1 {
		t.Fatalf("解析出 %d 项，期望 1", len(idx))
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s：%v", p, err)
	}
	return b
}

// TestBlobIndexSummaryDeterministic 覆盖摘要与输入顺序无关：同一集合无论
// 求差先后都得到稳定摘要，是增量核对（并集 == Expected）的基础。
func TestBlobIndexSummaryDeterministic(t *testing.T) {
	make := func(order []string) BlobIndex {
		refs := make(BlobIndex, 0, len(order))
		for _, h := range order {
			refs = append(refs, BlobRef{Hash: h, Size: int64(len(h))})
		}
		return NewBlobIndex(refs)
	}
	a := make([]string{"cc", "aa", "bb"})
	b := make([]string{"bb", "cc", "aa"}) // 故意打乱顺序、且含重复
	s1 := a.Summary()
	s2 := b.Summary()
	if s1 != s2 {
		t.Fatalf("同一集合摘要应稳定：%v != %v", s1, s2)
	}
	if s1.Count != 3 {
		t.Fatalf("count = %d，期望 3", s1.Count)
	}
	if s1.IndexSHA256 == "" {
		t.Fatal("摘要索引哈希不应为空")
	}
	if len(s1.IndexSHA256) != 64 {
		t.Fatalf("摘要索引哈希长度 = %d，期望 64", len(s1.IndexSHA256))
	}
}

// TestBlobIndexUnionDeterministic 覆盖并集：去重、确定性排序、与输入顺序无关。
func TestBlobIndexUnionDeterministic(t *testing.T) {
	set := func(hashes ...string) BlobIndex {
		refs := make(BlobIndex, 0, len(hashes))
		for _, h := range hashes {
			refs = append(refs, BlobRef{Hash: h, Size: int64(len(h))})
		}
		return NewBlobIndex(refs)
	}
	base := set("aa", "bb")
	cur1 := set("bb", "cc", "dd") // 交错顺序
	cur2 := set("dd", "cc", "bb") // 另一种顺序
	u1 := base.Union(cur1)
	u2 := base.Union(cur2)
	if len(u1) != 4 {
		t.Fatalf("并集长度 = %d，期望 4", len(u1))
	}
	if !reflect.DeepEqual(u1, u2) {
		t.Fatalf("并集应与输入顺序无关：%v != %v", u1, u2)
	}
	// 结果必须按哈希升序（确定性）。
	for i := 1; i < len(u1); i++ {
		if u1[i-1].Hash > u1[i].Hash {
			t.Fatalf("并集未按哈希排序：%v", u1)
		}
	}
	// 摘要同样稳定。
	if u1.Summary() != u2.Summary() {
		t.Fatalf("并集摘要应稳定：%v != %v", u1.Summary(), u2.Summary())
	}
}

// TestManifestExpectedRoundTrip 覆盖全量包（Expected=nil）与差包（Expected 非空）的
// 序列化与校验：全量包不写 Expected；差包 Expected 合法时 Validate 通过；非法 Expected 被拦。
func TestManifestExpectedRoundTrip(t *testing.T) {
	// 构造一个 Validate 可直接通过的 manifest（DB/Blobs 段齐全）。
	valid := func() Manifest {
		m := baseManifest()
		m.DB = DBRef{File: DBName, SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
		m.Blobs = BlobsRef{File: BlobIndexName, Count: 1, TotalBytes: 10, IndexSHA256: strings.Repeat("b", 64)}
		return m
	}

	// 全量包：Expected=nil，必须被 omitempty 省略，且旧读取逻辑不受影响。
	full := valid()
	if full.Expected != nil {
		t.Fatal("全量包 Expected 应为 nil")
	}
	data, err := MarshalManifest(full)
	if err != nil {
		t.Fatalf("序列化全量包：%v", err)
	}
	if bytes.Contains(data, []byte("\"expected\"")) {
		t.Fatalf("全量包不应序列化 expected 字段：%s", data)
	}
	if _, err := UnmarshalManifest(data); err != nil {
		t.Fatalf("全量包往返：%v", err)
	}

	// 差包：Expected 为完整集合摘要，Validate 应通过。
	sum := BlobSetSummary{Count: 3, TotalBytes: 42, IndexSHA256: strings.Repeat("a", 64)}
	delta := valid()
	delta.PackageID = "bk-delta-1"
	delta.BasePackageID = "bk-base-1"
	delta.Expected = &sum
	if !delta.IsIncremental() {
		t.Fatal("差包应被识别为增量")
	}
	if _, err := UnmarshalManifest(mustMarshal(t, delta)); err != nil {
		t.Fatalf("差包（Expected 合法）应可往返：%v", err)
	}

	// 非法 Expected：IndexSHA256 非 64 位小写 hex → Validate 拦下。
	bad := delta
	badIdx := strings.Repeat("A", 64) // 大写非法
	bad.Expected = &BlobSetSummary{Count: 1, IndexSHA256: badIdx}
	if _, err := UnmarshalManifest(mustMarshal(t, bad)); err == nil {
		t.Fatal("非法 expected.indexSha256 应被 Validate 拦下")
	}
}

func mustMarshal(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("序列化 manifest：%v", err)
	}
	return b
}

// TestManifestExpectedOmittedOnFullPackage 是"新增 Expected 字段后全量包读写向后兼容"的
// 回归点：全量包不设 Expected，既有的 WriteBundleRoundTrip 类用例不应受影响——这里直接断言
// 全量包写入后读回的 Expected 仍为 nil。
func TestManifestExpectedOmittedOnFullPackage(t *testing.T) {
	dbPath, refs, source, _ := newFixture(t)
	dst := filepath.Join(t.TempDir(), "full.tar.gz")
	final, err := WriteBundle(dst, baseManifest(), dbPath, refs, source, nil)
	if err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	if final.Expected != nil {
		t.Fatalf("全量包不应有 Expected：%v", final.Expected)
	}
	r, m, err := Open(dst)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	if m.Expected != nil {
		t.Fatalf("全量包读回 Expected 应为 nil：%v", m.Expected)
	}
	if err := r.Verify(false); err != nil {
		t.Fatalf("全量包校验：%v", err)
	}
}
