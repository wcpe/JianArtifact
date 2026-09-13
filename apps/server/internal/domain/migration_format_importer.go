package domain

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/nugetpackage"
)

const migrationNuGetMaxBytes int64 = 128 << 20

// MigrationAsset 是迁移器交给格式服务的一项待导入制品。
type MigrationAsset struct {
	Repository string
	Format     string
	SourcePath string
	Body       io.Reader
	// SourceModified 源端最后修改时间（在线迁移保留源时间戳）；
	// 零值时各格式服务回退本地时间语义，与离线迁移一致。
	SourceModified time.Time
}

// MigrationFormatImporter 以格式语义写入迁移制品，禁止降级为裸资产写入。
type MigrationFormatImporter interface {
	ImportMigrationAsset(MigrationAsset) error
}

type migrationFormatImporter struct {
	metadata *FormatMetadataService
	cargo    *CargoService
	oci      *OCIService
}

// NewMigrationFormatImporter 组合需要格式语义的迁移写入服务。
func NewMigrationFormatImporter(metadata *FormatMetadataService, cargo *CargoService, oci ...*OCIService) MigrationFormatImporter {
	var service *OCIService
	if len(oci) > 0 {
		service = oci[0]
	}
	return &migrationFormatImporter{metadata: metadata, cargo: cargo, oci: service}
}

func (i *migrationFormatImporter) ImportMigrationAsset(asset MigrationAsset) error {
	if asset.Format == "cargo" {
		if i.cargo == nil {
			return fmt.Errorf("%w: cargo 迁移器未配置", ErrValidation)
		}
		return i.cargo.ImportMigrationAsset(asset)
	}
	if asset.Format == "docker" {
		if i.oci == nil {
			return fmt.Errorf("%w: OCI 迁移器未配置", ErrValidation)
		}
		return i.oci.ImportMigrationAsset(asset)
	}
	if i.metadata == nil {
		return fmt.Errorf("%w: 格式元数据迁移器未配置", ErrValidation)
	}
	return i.metadata.ImportMigrationAsset(asset)
}

// ImportMigrationAsset 按格式验证并写入协议索引与制品内容。
func (s *FormatMetadataService) ImportMigrationAsset(asset MigrationAsset) error {
	if asset.Body == nil {
		return fmt.Errorf("%w: 迁移制品内容不能为空", ErrValidation)
	}
	switch asset.Format {
	case "pypi":
		return s.importMigrationPyPI(asset)
	case "nuget":
		return s.importMigrationNuGet(asset)
	default:
		return fmt.Errorf("%w: 不支持格式 %q 的迁移导入", ErrValidation, asset.Format)
	}
}

func (s *FormatMetadataService) importMigrationPyPI(asset MigrationAsset) error {
	filename := path.Base(asset.SourcePath)
	project, version, err := migrationPyPINameVersion(filename)
	if err != nil {
		return err
	}
	_, err = s.PublishPyPIWithSourceTime(asset.Repository, project, version, filename, "", "", asset.Body, asset.SourceModified)
	return err
}

func (s *FormatMetadataService) importMigrationNuGet(asset MigrationAsset) error {
	file, size, err := spoolMigrationNuGet(asset.Body)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	id, version, metadata, err := nugetpackage.Parse(file, size)
	if err != nil {
		return fmt.Errorf("%w: nupkg 校验失败：%v", ErrValidation, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	filename := path.Base(asset.SourcePath)
	if !strings.HasSuffix(strings.ToLower(filename), ".nupkg") || !validPackageFilename(filename) {
		filename = id + "." + version + ".nupkg"
	}
	if !validPackageFilename(filename) {
		return fmt.Errorf("%w: nupkg 文件名非法", ErrValidation)
	}
	_, err = s.PublishNuGetWithSourceTime(asset.Repository, id, version, filename, metadata, file, asset.SourceModified)
	return err
}

func spoolMigrationNuGet(source io.Reader) (*os.File, int64, error) {
	file, err := os.CreateTemp("", "jianartifact-migration-nuget-*")
	if err != nil {
		return nil, 0, err
	}
	name := file.Name()
	cleanup := func(err error) (*os.File, int64, error) {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(name)
		}
		return file, 0, err
	}
	size, err := io.Copy(file, io.LimitReader(source, migrationNuGetMaxBytes+1))
	if err != nil {
		return cleanup(err)
	}
	if size > migrationNuGetMaxBytes {
		return cleanup(fmt.Errorf("%w: nupkg 超过大小限制", ErrValidation))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return cleanup(err)
	}
	return file, size, nil
}

func migrationPyPINameVersion(filename string) (string, string, error) {
	if !validPackageFilename(filename) {
		return "", "", fmt.Errorf("%w: PyPI 文件名非法", ErrValidation)
	}
	if base, ok := migrationPyPITrimSuffix(filename, ".whl"); ok {
		return migrationPyPIWheelNameVersion(base)
	}
	return migrationPyPISdistNameVersion(filename)
}

func migrationPyPIWheelNameVersion(base string) (string, string, error) {
	parts := strings.Split(base, "-")
	if len(parts) < 5 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: wheel 文件名非法", ErrValidation)
	}
	return parts[0], parts[1], nil
}

func migrationPyPISdistNameVersion(filename string) (string, string, error) {
	base, ok := migrationPyPITrimSuffix(filename, ".tar.gz")
	if !ok {
		base, ok = migrationPyPITrimSuffix(filename, ".zip")
	}
	if !ok {
		base, ok = migrationPyPITrimSuffix(filename, ".tar.bz2")
	}
	if !ok {
		base, ok = migrationPyPITrimSuffix(filename, ".tar.xz")
	}
	if !ok {
		return "", "", fmt.Errorf("%w: PyPI 源码包文件名非法", ErrValidation)
	}
	cut := strings.LastIndex(base, "-")
	if cut <= 0 || cut == len(base)-1 {
		return "", "", fmt.Errorf("%w: PyPI 源码包文件名非法", ErrValidation)
	}
	return base[:cut], base[cut+1:], nil
}

func migrationPyPITrimSuffix(filename, suffix string) (string, bool) {
	if !strings.HasSuffix(strings.ToLower(filename), suffix) {
		return "", false
	}
	return filename[:len(filename)-len(suffix)], true
}
