package domain

import (
	"fmt"
	"io"
	"strings"
)

// ImportMigrationAsset 按 OCI blob、manifest、tag 的依赖顺序导入迁移资产。
func (s *OCIService) ImportMigrationAsset(asset MigrationAsset) (int64, error) {
	path := strings.Trim(asset.SourcePath, "/")
	if digest, ok := strings.CutPrefix(path, "oci/blobs/sha256/"); ok {
		staged, err := s.putBlob(asset.Repository, "sha256:"+digest, asset.Body, asset.SourceModified, true)
		if err != nil {
			return 0, err
		}
		return staged.ID, nil
	}
	if _, digest, ok := migrationNexusOCIBlob(path); ok {
		staged, err := s.putBlob(asset.Repository, "sha256:"+digest, asset.Body, asset.SourceModified, true)
		if err != nil {
			return 0, err
		}
		return staged.ID, nil
	}
	if image, digest, ok := migrationOCIPath(path, "oci/manifests/"); ok {
		body, err := io.ReadAll(io.LimitReader(asset.Body, maxOCIManifestBytes+1))
		if err != nil || len(body) > maxOCIManifestBytes {
			return 0, ErrValidation
		}
		res, err := s.putManifest(asset.Repository, image, "sha256:"+digest, body, "application/vnd.oci.image.manifest.v1+json", asset.SourceModified, true)
		if err != nil {
			return 0, err
		}
		return res.AssetID, nil
	}
	if image, tag, ok := migrationOCIPath(path, "oci/tags/"); ok {
		digestBytes, err := io.ReadAll(io.LimitReader(asset.Body, 80))
		if err != nil {
			return 0, err
		}
		digest := strings.TrimSpace(string(digestBytes))
		if _, ok := validOCIDigest(digest); !ok {
			return 0, ErrValidation
		}
		_, rc, err := s.assets.Get(asset.Repository, ociManifestPath(image, digest))
		if err != nil {
			return 0, err
		}
		defer func() { _ = rc.Close() }()
		body, err := io.ReadAll(io.LimitReader(rc, maxOCIManifestBytes+1))
		if err != nil || len(body) > maxOCIManifestBytes {
			return 0, ErrValidation
		}
		res, err := s.putManifest(asset.Repository, image, tag, body, "application/vnd.oci.image.manifest.v1+json", asset.SourceModified, true)
		if err != nil {
			return 0, err
		}
		return res.AssetID, nil
	}
	if rest, ok := strings.CutPrefix(path, "v2/"); ok {
		if at := strings.Index(rest, "/manifests/"); at > 0 {
			image, reference := rest[:at], rest[at+len("/manifests/"):]
			if !validOCIImage(image) || !validOCIReference(reference) {
				return 0, ErrValidation
			}
			body, err := io.ReadAll(io.LimitReader(asset.Body, maxOCIManifestBytes+1))
			if err != nil || len(body) > maxOCIManifestBytes {
				return 0, ErrValidation
			}
			res, err := s.putManifest(asset.Repository, image, reference, body, "application/vnd.oci.image.manifest.v1+json", asset.SourceModified, true)
			if err != nil {
				return 0, err
			}
			return res.AssetID, nil
		}
	}
	return 0, fmt.Errorf("%w: OCI 迁移路径不受支持", ErrValidation)
}

func migrationNexusOCIBlob(source string) (string, string, bool) {
	rest, ok := strings.CutPrefix(source, "v2/")
	if !ok {
		return "", "", false
	}
	marker := "/blobs/"
	at := strings.Index(rest, marker)
	if at < 1 {
		return "", "", false
	}
	image, digest := rest[:at], strings.TrimPrefix(rest[at+len(marker):], "sha256/")
	digest = strings.TrimPrefix(digest, "sha256:")
	if !validOCIImage(image) || !ociDigestPattern.MatchString("sha256:"+digest) {
		return "", "", false
	}
	return image, digest, true
}

func migrationOCIPath(source, prefix string) (string, string, bool) {
	rest, ok := strings.CutPrefix(source, prefix)
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	image, reference := strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1]
	if !validOCIImage(image) || reference == "" {
		return "", "", false
	}
	return image, reference, true
}
