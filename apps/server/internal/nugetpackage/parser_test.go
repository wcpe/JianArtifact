package nugetpackage

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestParseAcceptsLargePackageEntryWithinArchiveBudget(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	writeNuGetTestEntry(t, writer, "Demo.Package.nuspec", []byte("<package><metadata><id>Demo.Package</id><version>1.0.0</version></metadata></package>"))
	writeNuGetTestEntry(t, writer, "lib/net8.0/Demo.Package.dll", make([]byte, 2<<20))
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭测试 nupkg：%v", err)
	}

	if id, version, _, err := Parse(bytes.NewReader(archive.Bytes()), int64(archive.Len())); err != nil || id != "Demo.Package" || version != "1.0.0" {
		t.Fatalf("包含大二进制条目的正常 nupkg 应通过：id=%q version=%q err=%v", id, version, err)
	}
}

func TestParseRejectsArchiveBeyondTotalUncompressedBudget(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.CreateRaw(&zip.FileHeader{
		Name:               "lib/net8.0/oversized.dll",
		Method:             zip.Store,
		UncompressedSize64: maxArchiveUncompressedBytes + 1,
	})
	if err != nil {
		t.Fatalf("创建超限测试 ZIP 条目：%v", err)
	}
	if _, err := entry.Write(nil); err != nil {
		t.Fatalf("写入超限测试 ZIP 条目：%v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭超限测试 nupkg：%v", err)
	}

	if _, _, _, err := Parse(bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("超过总解压预算的 nupkg 应拒绝")
	}
}

func TestParseRejectsArchiveBeyondCompressedBudget(t *testing.T) {
	if _, _, _, err := Parse(bytes.NewReader(nil), maxArchiveBytes+1); err == nil {
		t.Fatal("超过压缩包大小预算的 nupkg 应拒绝")
	}
}

func TestParseRejectsArchiveWithTooManyEntries(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for index := 0; index <= maxArchiveEntries; index++ {
		if _, err := writer.Create(fmt.Sprintf("lib/net8.0/%d.dll", index)); err != nil {
			t.Fatalf("创建超量测试 ZIP 条目：%v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭超量测试 nupkg：%v", err)
	}

	if _, _, _, err := Parse(bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("超过条目数量预算的 nupkg 应拒绝")
	}
}

func TestParseIncludesDependencyGroupsAndTargetFramework(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	writeNuGetTestEntry(t, writer, "Demo.Package.nuspec", []byte(`<package><metadata><id>Demo.Package</id><version>1.0.0</version><dependencies><group targetFramework="net8.0"><dependency id="Child.Package" version="[2.0.0]" /></group><group targetFramework="netstandard2.0"><dependency id="Legacy.Package" version="1.5.0" /></group></dependencies></metadata></package>`))
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭测试 nupkg：%v", err)
	}

	_, _, metadata, err := Parse(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatalf("解析 nuspec：%v", err)
	}
	var parsed struct {
		DependencyGroups []struct {
			TargetFramework string `json:"targetFramework"`
			Dependencies    []struct {
				ID    string `json:"id"`
				Range string `json:"range"`
			} `json:"dependencies"`
		} `json:"dependencyGroups"`
	}
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		t.Fatalf("反序列化 nuspec 元数据：%v", err)
	}
	if len(parsed.DependencyGroups) != 2 || parsed.DependencyGroups[0].TargetFramework != "net8.0" || len(parsed.DependencyGroups[0].Dependencies) != 1 || parsed.DependencyGroups[0].Dependencies[0].ID != "Child.Package" || parsed.DependencyGroups[0].Dependencies[0].Range != "[2.0.0]" {
		t.Fatalf("依赖组或 TFM 解析错误：%s", metadata)
	}
}

func writeNuGetTestEntry(t *testing.T, writer *zip.Writer, name string, body []byte) {
	t.Helper()
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatalf("创建测试 ZIP 条目：%v", err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatalf("写入测试 ZIP 条目：%v", err)
	}
}
