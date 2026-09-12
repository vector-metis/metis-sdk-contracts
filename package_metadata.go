package contract

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxPackagePathSize = 1024
	maxPackageEntries  = 10000
	maxManifestSize    = 1 << 20
	maxSettingsSize    = 1 << 20
	maxAboutSize       = 4 << 20
	maxComposeSize     = 4 << 20
)

// PackageMetadata 是 Master 准备应用包时需要持久化的小型只读快照。
// Compose 按架构保存原始 YAML；镜像归档和其他大文件永远不会进入该结构。
type PackageMetadata struct {
	Manifest  Manifest          `json:"manifest"`
	AboutMD   string            `json:"aboutMarkdown,omitempty"`
	Settings  []Setting         `json:"settings"`
	Compose   map[string]string `json:"compose"`
	FileCount int               `json:"fileCount"`
}

// InspectPackageIdentity 按 tar 顺序读取受限的 manifest，不物化镜像等其他包内容。
func InspectPackageIdentity(reader io.Reader) (string, string, error) {
	if reader == nil {
		return "", "", fmt.Errorf("mpk: package reader is required")
	}
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return "", "", fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return "", "", fmt.Errorf("mpk: package root must contain %s", ManifestName)
		}
		if nextErr != nil {
			return "", "", fmt.Errorf("mpk: tar format is invalid: %w", nextErr)
		}
		if path.Clean(header.Name) != ManifestName {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maxManifestSize {
			return "", "", fmt.Errorf("mpk: %s must be a regular file no larger than %d bytes", ManifestName, maxManifestSize)
		}
		content, readErr := io.ReadAll(io.LimitReader(tarReader, maxManifestSize+1))
		if readErr != nil {
			return "", "", fmt.Errorf("mpk: cannot read %s: %w", ManifestName, readErr)
		}
		if int64(len(content)) > maxManifestSize {
			return "", "", fmt.Errorf("mpk: %s exceeds %d bytes", ManifestName, maxManifestSize)
		}
		var manifest Manifest
		if err = yaml.Unmarshal(content, &manifest); err != nil {
			return "", "", fmt.Errorf("mpk: %s is invalid YAML: %w", ManifestName, err)
		}
		if err = manifest.validateIdentity(); err != nil {
			return "", "", err
		}
		return manifest.ID, manifest.Version, nil
	}
}

// InspectPackageMetadata 顺序扫描完整 gzip/tar，并只物化受限的契约文件。
// expectedAppID 把对象键身份绑定到包内 manifest；expectedVersion 为空时由包内版本决定新键，
// 非空时同时校验版本身份。
func InspectPackageMetadata(reader io.Reader, expectedAppID, expectedVersion string) (*PackageMetadata, error) {
	return inspectPackageMetadata(reader, expectedAppID, expectedVersion, DefaultMaxPackageSize)
}

func inspectPackageMetadata(reader io.Reader, expectedAppID, expectedVersion string, maxPackageSize int64) (*PackageMetadata, error) {
	if reader == nil {
		return nil, fmt.Errorf("mpk: package reader is required")
	}
	if seeker, ok := reader.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("mpk: rewind package metadata: %w", err)
		}
		defer func() { _, _ = seeker.Seek(0, io.SeekStart) }()
	}
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	files, present, fileCount, scanErr := scanPackageMetadataTar(gzipReader, maxPackageSize)
	if scanErr == nil {
		_, scanErr = io.Copy(io.Discard, gzipReader)
	}
	closeErr := gzipReader.Close()
	if scanErr != nil || closeErr != nil {
		return nil, errors.Join(scanErr, closeErr)
	}

	manifestData, exists := files[ManifestName]
	if !exists {
		return nil, fmt.Errorf("mpk: package root must contain %s", ManifestName)
	}
	var manifest Manifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("mpk: %s is invalid YAML: %w", ManifestName, err)
	}
	if err := manifest.validateIdentity(); err != nil {
		return nil, err
	}
	if _, exists := present["settings.yaml"]; exists {
		return nil, fmt.Errorf("mpk: settings.yaml is not allowed; declare settings in manifest.yaml")
	}
	if expectedAppID != "" && manifest.ID != expectedAppID || expectedVersion != "" && manifest.Version != expectedVersion {
		return nil, fmt.Errorf(
			"mpk: manifest identity %q/%q does not match application package %q/%q",
			manifest.ID, manifest.Version, expectedAppID, expectedVersion,
		)
	}
	if err := validateIntegrationURL(manifest.IntegrationDocsURL); err != nil {
		return nil, err
	}
	for _, screenshot := range manifest.Screenshots {
		if _, exists := present[screenshot]; exists {
			files[screenshot] = nil
		}
	}
	if err := validateScreenshots(manifest.Screenshots, files); err != nil {
		return nil, err
	}
	if err := validateCompose(manifest, files, true); err != nil {
		return nil, err
	}
	if about := files["about.md"]; !utf8.Valid(about) {
		return nil, fmt.Errorf("mpk: about.md is not valid UTF-8")
	}

	sort.Strings(manifest.Architectures)
	sort.Strings(manifest.Capabilities)
	compose := make(map[string]string, len(manifest.Architectures))
	for _, architecture := range manifest.Architectures {
		compose[architecture] = string(files["compose."+architecture+".yaml"])
	}
	return &PackageMetadata{
		Manifest: manifest, AboutMD: string(files["about.md"]), Settings: manifest.Settings,
		Compose: compose, FileCount: fileCount,
	}, nil
}

func scanPackageMetadataTar(reader io.Reader, maxPackageSize int64) (map[string][]byte, map[string]struct{}, int, error) {
	tarReader := tar.NewReader(reader)
	files := make(map[string][]byte)
	present := make(map[string]struct{})
	fileCount := 0
	entryCount := 0
	var expanded int64
	expandedLimit := maxPackageSize * 2
	if maxPackageSize > int64(^uint64(0)>>1)/2 {
		expandedLimit = int64(^uint64(0) >> 1)
	}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, 0, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		entryCount++
		if entryCount > maxPackageEntries {
			return nil, nil, 0, fmt.Errorf("mpk: package exceeds %d entries", maxPackageEntries)
		}
		name := path.Clean(header.Name)
		if header.Name == "" || len(header.Name) > maxPackagePathSize || name == "." ||
			path.IsAbs(header.Name) || strings.HasPrefix(name, "../") || name == ".." {
			return nil, nil, 0, fmt.Errorf("mpk: unsafe path %q", header.Name)
		}
		if _, duplicate := present[name]; duplicate {
			return nil, nil, 0, fmt.Errorf("mpk: duplicate entry %q", name)
		}
		present[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxPackageSize || expanded > expandedLimit-header.Size {
				return nil, nil, 0, fmt.Errorf("mpk: expanded package size exceeds %d bytes", expandedLimit)
			}
			expanded += header.Size
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return nil, nil, 0, fmt.Errorf("mpk: forbidden non-regular node %q", name)
		default:
			return nil, nil, 0, fmt.Errorf("mpk: unsupported tar node %q", name)
		}
		fileCount++
		limit := metadataFileLimit(name)
		if limit == 0 {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return nil, nil, 0, fmt.Errorf("mpk: cannot scan %q: %w", name, err)
			}
			continue
		}
		content, err := io.ReadAll(io.LimitReader(tarReader, limit+1))
		if err != nil {
			return nil, nil, 0, fmt.Errorf("mpk: cannot read %q: %w", name, err)
		}
		if int64(len(content)) > limit {
			return nil, nil, 0, fmt.Errorf("mpk: metadata file %q exceeds %d bytes", name, limit)
		}
		files[name] = content
	}
	if fileCount == 0 {
		return nil, nil, 0, fmt.Errorf("mpk: package is empty")
	}
	return files, present, fileCount, nil
}

func metadataFileLimit(name string) int64 {
	switch {
	case name == ManifestName:
		return maxManifestSize
	case name == "about.md":
		return maxAboutSize
	case name == "settings.yaml":
		return maxSettingsSize
	case name == "compose.amd64.yaml" || name == "compose.arm64.yaml":
		return maxComposeSize
	default:
		return 0
	}
}
