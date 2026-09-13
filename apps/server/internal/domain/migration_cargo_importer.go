package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ImportMigrationAsset 以 Cargo crate 与 sparse index 的语义导入迁移资产。
func (s *CargoService) ImportMigrationAsset(asset MigrationAsset) error {
	if asset.Body == nil {
		return fmt.Errorf("%w: Cargo 迁移制品内容不能为空", ErrValidation)
	}
	if name, version, ok := cargoMigrationCratePath(asset.SourcePath); ok {
		return s.importMigrationCrate(asset.Repository, name, version, asset.Body, asset.SourceModified)
	}
	if name, ok := cargoMigrationIndexName(asset.SourcePath); ok {
		return s.importMigrationIndex(asset.Repository, name, asset.Body)
	}
	return fmt.Errorf("%w: Cargo 迁移路径不受支持", ErrValidation)
}

func (s *CargoService) importMigrationCrate(repo, name, version string, body io.Reader, sourceModified time.Time) error {
	file, size, err := spoolMigrationCargo(body)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	frame, err := cargoMigrationFrame(name, version, file, size)
	if err != nil {
		return err
	}
	_, err = s.publishCargo(context.Background(), repo, frame, nil, sourceModified)
	return err
}

func (s *CargoService) importMigrationIndex(repo, name string, body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, cargoMaxMetadataBytes+1))
	if err != nil || len(data) > cargoMaxMetadataBytes {
		return fmt.Errorf("%w: Cargo 索引内容非法", ErrValidation)
	}
	if err := s.validateMigrationIndex(repo, name, data); err != nil {
		return err
	}
	merged, changed, err := s.mergeMigrationIndex(repo, name, data)
	if err != nil || !changed {
		return err
	}
	asset, err := s.assets.StageBlob(bytes.NewReader(merged), "application/json")
	if err != nil {
		return err
	}
	asset.Path = cargoHostedIndexAssetPath(name)
	_, err = s.assets.PublishAssets(repo, []*repository.Asset{asset})
	if err != nil {
		return s.assets.cleanupFailedWrite(asset.BlobHash, err)
	}
	return nil
}

func (s *CargoService) mergeMigrationIndex(repo, name string, incoming []byte) ([]byte, bool, error) {
	path := cargoHostedIndexAssetPath(name)
	existing, err := s.readCargoIndex(repo, path)
	if err != nil {
		return nil, false, err
	}
	oldLines, err := mergeCargoLines(nil, existing)
	if err != nil && len(existing) > 0 {
		return nil, false, err
	}
	incomingLines, err := mergeCargoLines(nil, incoming)
	if err != nil {
		return nil, false, err
	}
	oldChecksums := cargoIndexChecksums(oldLines)
	replacements := make(map[string]string, len(incomingLines))
	for _, line := range incomingLines {
		version, checksum, err := cargoIndexVersionChecksum(line)
		if err != nil {
			return nil, false, err
		}
		if old, ok := oldChecksums[version]; ok {
			if old != checksum {
				return nil, false, fmt.Errorf("%w: Cargo 索引版本校验和冲突", ErrConflict)
			}
		}
		replacements[version] = line
	}
	mergedInput := make([]string, 0, len(oldLines)+len(incomingLines))
	for _, line := range oldLines {
		version, _, _ := cargoIndexVersionChecksum(line)
		if replacement, ok := replacements[version]; ok {
			mergedInput = append(mergedInput, replacement)
			delete(replacements, version)
			continue
		}
		mergedInput = append(mergedInput, line)
	}
	for _, line := range replacements {
		mergedInput = append(mergedInput, line)
	}
	merged, err := mergeCargoLines(nil, []byte(strings.Join(mergedInput, "\n")))
	if err != nil {
		return nil, false, err
	}
	mergedData := []byte(strings.Join(merged, "\n") + "\n")
	return mergedData, !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(mergedData)), nil
}

func (s *CargoService) readCargoIndex(repo, path string) ([]byte, error) {
	_, rc, err := s.assets.Get(repo, path)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, cargoMaxMetadataBytes+1))
}

func cargoIndexChecksums(lines []string) map[string]string {
	checksums := make(map[string]string, len(lines))
	for _, line := range lines {
		version, checksum, err := cargoIndexVersionChecksum(line)
		if err == nil {
			checksums[version] = checksum
		}
	}
	return checksums
}

func cargoIndexVersionChecksum(line string) (string, string, error) {
	var item struct {
		Vers  string `json:"vers"`
		Cksum string `json:"cksum"`
	}
	if err := json.Unmarshal([]byte(line), &item); err != nil || item.Vers == "" || len(item.Cksum) != 64 {
		return "", "", fmt.Errorf("%w: Cargo 索引记录非法", ErrValidation)
	}
	return item.Vers, strings.ToLower(item.Cksum), nil
}

func (s *CargoService) validateMigrationIndex(repo, expectedName string, data []byte) error {
	lines, err := mergeCargoLines(nil, data)
	if err != nil || len(lines) == 0 {
		return fmt.Errorf("%w: Cargo 索引记录非法", ErrValidation)
	}
	for _, line := range lines {
		var item struct {
			Name  string `json:"name"`
			Vers  string `json:"vers"`
			Cksum string `json:"cksum"`
		}
		if json.Unmarshal([]byte(line), &item) != nil || item.Name != expectedName || item.Vers == "" || len(item.Cksum) != 64 {
			return fmt.Errorf("%w: Cargo 索引记录非法", ErrValidation)
		}
		asset, rc, getErr := s.assets.Get(repo, cargoAssetPath(item.Name, item.Vers))
		if getErr == nil {
			_ = rc.Close()
		}
		if getErr != nil || !strings.EqualFold(asset.BlobHash, item.Cksum) {
			return fmt.Errorf("%w: Cargo 索引缺少匹配 crate", ErrValidation)
		}
	}
	return nil
}

func cargoMigrationCratePath(source string) (string, string, bool) {
	parts := strings.Split(strings.Trim(source, "/"), "/")
	if len(parts) == 5 && parts[0] == "cargo" && parts[1] == "crates" {
		return validMigrationCargoCrate(parts[2], parts[3], parts[4])
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "crates" && parts[5] == "download" {
		return validMigrationCargoCrate(parts[3], parts[4], "")
	}
	if len(parts) == 4 && parts[0] == "crates" && parts[3] == "download" {
		return validMigrationCargoCrate(parts[1], parts[2], "")
	}
	return "", "", false
}

func validMigrationCargoCrate(name, version, filename string) (string, string, bool) {
	normalized, err := normalizeCargoName(name)
	if err != nil || version == "" || strings.ContainsAny(version, "/\\") {
		return "", "", false
	}
	if filename != "" && filename != normalized+"-"+version+".crate" {
		return "", "", false
	}
	return normalized, version, true
}

func cargoMigrationIndexName(source string) (string, bool) {
	path := strings.Trim(source, "/")
	if path == "config.json" {
		return "", false
	}
	path = strings.TrimPrefix(path, "cargo/index/")
	parts := strings.Split(path, "/")
	name, err := normalizeCargoName(parts[len(parts)-1])
	if err != nil || cargoIndexPath(name) != path {
		return "", false
	}
	return name, true
}

func spoolMigrationCargo(source io.Reader) (*os.File, int64, error) {
	file, err := os.CreateTemp("", "jianartifact-migration-cargo-")
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
	size, err := io.Copy(file, io.LimitReader(source, cargoMaxCrateBytes+1))
	if err != nil {
		return cleanup(err)
	}
	if size > cargoMaxCrateBytes {
		return cleanup(fmt.Errorf("%w: Cargo crate 超过大小限制", ErrValidation))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return cleanup(err)
	}
	return file, size, nil
}

func cargoMigrationFrame(name, version string, crate *os.File, size int64) (io.Reader, error) {
	metadata, err := json.Marshal(map[string]any{"name": name, "vers": version, "deps": []any{}, "features": map[string]any{}})
	if err != nil || size <= 0 || size > int64(^uint32(0)) {
		return nil, fmt.Errorf("%w: Cargo crate 元数据非法", ErrValidation)
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.LittleEndian, uint32(len(metadata)))
	_, _ = frame.Write(metadata)
	_ = binary.Write(&frame, binary.LittleEndian, uint32(size))
	return io.MultiReader(&frame, crate), nil
}
