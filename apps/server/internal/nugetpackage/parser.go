// Package nugetpackage 提供 nupkg 元数据的统一安全解析。
package nugetpackage

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

const (
	maxArchiveBytes             = 128 << 20
	maxArchiveEntries           = 10_000
	maxArchiveUncompressedBytes = 512 << 20
	maxNuSpecUncompressedBytes  = 1 << 20
)

type nuSpec struct {
	XMLName  xml.Name `xml:"package"`
	Metadata Metadata `xml:"metadata"`
}

// Metadata 是 NuGet 恢复所需的 nuspec 投影，不保留包内的其他内容。
type Metadata struct {
	ID               string            `xml:"id" json:"id"`
	Version          string            `xml:"version" json:"version"`
	DependencyGroups []DependencyGroup `xml:"dependencies>group" json:"dependencyGroups,omitempty"`
	Dependencies     []Dependency      `xml:"dependencies>dependency" json:"-"`
}

// DependencyGroup 表示指定目标框架的一组 NuGet 依赖。
type DependencyGroup struct {
	TargetFramework string       `xml:"targetFramework,attr" json:"targetFramework,omitempty"`
	Dependencies    []Dependency `xml:"dependency" json:"dependencies"`
}

// Dependency 是 NuGet 依赖标识和版本范围。
type Dependency struct {
	ID    string `xml:"id,attr" json:"id"`
	Range string `xml:"version,attr" json:"range,omitempty"`
}

// Parse 从已验证大小的 nupkg 流中读取唯一 nuspec，返回包标识、版本和元数据 JSON。
func Parse(source io.ReaderAt, size int64) (id, version, metadata string, err error) {
	if size == 0 {
		return "", "", "", io.ErrUnexpectedEOF
	}
	if size > maxArchiveBytes {
		return "", "", "", errors.New("nupkg 压缩包大小超过安全上限")
	}
	reader, err := zip.NewReader(source, size)
	if err != nil {
		return "", "", "", err
	}
	if len(reader.File) > maxArchiveEntries {
		return "", "", "", errors.New("nupkg 条目数量超过安全上限")
	}
	found := false
	var totalUncompressed uint64
	for _, file := range reader.File {
		if strings.Contains(file.Name, "..") || strings.HasPrefix(file.Name, "/") {
			return "", "", "", errors.New("nupkg 条目路径非法")
		}
		if file.UncompressedSize64 > maxArchiveUncompressedBytes-totalUncompressed {
			return "", "", "", errors.New("nupkg 解压总大小超过安全上限")
		}
		totalUncompressed += file.UncompressedSize64
		if !strings.HasSuffix(strings.ToLower(file.Name), ".nuspec") {
			continue
		}
		if file.UncompressedSize64 > maxNuSpecUncompressedBytes {
			return "", "", "", errors.New("nupkg nuspec 超过安全上限")
		}
		rc, openErr := file.Open()
		if openErr != nil {
			return "", "", "", openErr
		}
		nuspec, readErr := io.ReadAll(io.LimitReader(rc, maxNuSpecUncompressedBytes+1))
		closeErr := rc.Close()
		if readErr != nil {
			return "", "", "", readErr
		}
		if closeErr != nil {
			return "", "", "", closeErr
		}
		if len(nuspec) > maxNuSpecUncompressedBytes {
			return "", "", "", errors.New("nupkg nuspec 超过安全上限")
		}
		var spec nuSpec
		if xmlErr := xml.Unmarshal(nuspec, &spec); xmlErr != nil || spec.Metadata.ID == "" || spec.Metadata.Version == "" {
			return "", "", "", errors.New("nupkg 缺少合法 nuspec 元数据")
		}
		if found {
			return "", "", "", errors.New("nupkg 包含多个 nuspec")
		}
		if len(spec.Metadata.Dependencies) > 0 {
			spec.Metadata.DependencyGroups = append(spec.Metadata.DependencyGroups, DependencyGroup{Dependencies: spec.Metadata.Dependencies})
		}
		encoded, _ := json.Marshal(spec.Metadata)
		id, version, metadata, found = spec.Metadata.ID, spec.Metadata.Version, string(encoded), true
	}
	if found {
		return id, version, metadata, nil
	}
	return "", "", "", errors.New("nupkg 缺少 nuspec")
}
