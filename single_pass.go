package contract

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// PackageFile 是 MPK 顶层目录的一条有界事实。
// Content 只在调用方通过 FileContentLimit 请求且文件没有超过限制时提供；
// 镜像正文永远不会进入该结构。
type PackageFile struct {
	Path        string
	Size        int64
	Kind        string
	Content     []byte
	ContentRead bool
}

// MPKScanOptions 描述一次性 MPK 扫描的可选副作用。
// ImageVisitor 和 FileVisitor 都必须在返回前完成当前条目的消费；扫描器不保存大文件正文。
type MPKScanOptions struct {
	ValidateOptions
	ImageVisitor func(PackageImageArchive) (*ImageArchiveSummary, error)
	// FileContentLimit 返回指定文件是否需要提取内容以及最大字节数。
	// 返回 false 时文件仍会被完整消费，但不会进入内存。
	FileContentLimit func(path string, size int64) (limit int64, capture bool)
	FileVisitor      func(PackageFile) error
}

// MPKScanResult 是一次顺序扫描产生的完整门禁事实。
// Files 仅包含顶层成员描述，GateResult 复用现有 Store、CLI 和平台消费结构。
type MPKScanResult struct {
	GateResult
	Files []PackageFile
}

// 单趟扫描在 manifest 尚未出现时无法提前知道哪些截图会被公开使用，
// 因此对潜在资源设置独立的内存预算，避免恶意包通过大量 screenshots/*
// 条目把整个 100 GB 包的内容搬进内存。
const maxSinglePassAssetCaptureBytes = int64(MaxScreenshotCount)*MaxScreenshotSize + MaxIcon64Size + MaxIcon256Size

// MPKScanError 保留一次扫描已经读取到的包摘要，便于商店在硬拒绝时
// 记录审核哈希，而无需再次打开 staging 对象计算摘要。
type MPKScanError struct {
	Err     error
	Size    int64
	SHA256  string
	AppID   string
	Version string
}

func (failure *MPKScanError) Error() string { return failure.Err.Error() }
func (failure *MPKScanError) Unwrap() error { return failure.Err }

// InspectMPK 一次顺序读取 MPK，同时完成原始包摘要、契约校验、镜像归档检查和资源提取。
// 与历史 GateMPK 不同，此入口接收 io.Reader，不会调用 Seek，也不会重新打开输入对象。
func InspectMPK(reader io.Reader, options MPKScanOptions) (result *MPKScanResult, err error) {
	var hasher *countingHashReader
	var identityAppID, identityVersion string
	defer func() {
		if err != nil && hasher != nil {
			err = &MPKScanError{Err: err, Size: hasher.size, SHA256: hex.EncodeToString(hasher.hash.Sum(nil)), AppID: identityAppID, Version: identityVersion}
		}
		err = gateError(err)
	}()
	if reader == nil {
		return nil, fmt.Errorf("mpk: package reader is required")
	}
	maxPackageSize := options.MaxPackageSize
	if maxPackageSize <= 0 {
		maxPackageSize = DefaultMaxPackageSize
	}
	hasher = &countingHashReader{reader: reader, hash: sha256.New()}
	gzipReader, err := gzip.NewReader(hasher)
	if err != nil {
		return nil, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	files := make(map[string][]byte)
	present := make(map[string]struct{})
	overlayPaths := make(map[string]struct{})
	overlayDirs := make(map[string]struct{})
	packageFiles := make([]PackageFile, 0)
	images := make([]PackageImage, 0)
	summaries := make(map[string]*ImageArchiveSummary)
	seenPaths := make(map[string]struct{})
	seenImageTags := make(map[string]struct{})
	uncapturedAssetPaths := make(map[string]struct{})
	entryCount := 0
	fileCount := 0
	assetCaptureEntries := 0
	assetCaptureBytes := int64(0)
	var expanded int64
	expandedLimit := maxPackageSize * 2
	if maxPackageSize > int64(^uint64(0)>>1)/2 {
		expandedLimit = int64(^uint64(0) >> 1)
	}
	tarReader := tar.NewReader(gzipReader)
	visitor := options.ImageVisitor
	if visitor == nil {
		visitor = validateImageArchive
	}
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: tar format is invalid: %w", nextErr)
		}
		entryCount++
		if entryCount > maxPackageEntries {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: package exceeds %d entries", maxPackageEntries)
		}
		name := path.Clean(header.Name)
		if header.Name == "" || len(header.Name) > maxPackagePathSize || name == "." ||
			path.IsAbs(header.Name) || strings.HasPrefix(name, "../") || name == ".." {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: unsafe path %q", header.Name)
		}
		if _, duplicate := seenPaths[name]; duplicate {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: duplicate entry %q", name)
		}
		seenPaths[name] = struct{}{}
		present[name] = struct{}{}
		if header.Typeflag == tar.TypeDir {
			if name == "overlay" || strings.HasPrefix(name, "overlay/") {
				overlayDirs[name] = struct{}{}
			}
			entry := PackageFile{Path: name, Size: header.Size, Kind: "directory"}
			packageFiles = append(packageFiles, entry)
			if options.FileVisitor != nil {
				if visitErr := options.FileVisitor(entry); visitErr != nil {
					_ = gzipReader.Close()
					return nil, visitErr
				}
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: forbidden non-regular node %q", name)
		}
		if header.Size < 0 || header.Size > maxPackageSize || expanded > expandedLimit-header.Size {
			_ = gzipReader.Close()
			return nil, fmt.Errorf("mpk: expanded package size exceeds %d bytes", expandedLimit)
		}
		expanded += header.Size
		fileCount++
		if strings.HasPrefix(name, "overlay/") {
			overlayPaths[name] = struct{}{}
		}
		if strings.HasPrefix(name, "images/") {
			if !strings.HasPrefix(name, "images/") || path.Ext(name) != ".tar" {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: image archive %q must be images/{arch}/*.tar", name)
			}
			parts := strings.Split(name, "/")
			if len(parts) != 3 || parts[1] == "" {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: image archive %q must be images/{arch}/*.tar", name)
			}
			body := &io.LimitedReader{R: tarReader, N: header.Size}
			summary, visitErr := visitor(PackageImageArchive{Path: name, Architecture: parts[1], Size: header.Size, Body: body})
			if visitErr != nil {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: inspect image archive %q: %w", name, visitErr)
			}
			if body.N != 0 {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: image archive visitor did not consume %q", name)
			}
			if summary == nil {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: image archive %q returned no summary", name)
			}
			if summary.Architecture != parts[1] {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: image archive %q architecture does not match directory architecture %q", name, parts[1])
			}
			if _, duplicate := seenImageTags[parts[1]+"\x00"+summary.RepoTag]; duplicate {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: images/%s contains duplicate image tag %q", parts[1], summary.RepoTag)
			}
			seenImageTags[parts[1]+"\x00"+summary.RepoTag] = struct{}{}
			files[name] = nil
			images = append(images, PackageImage{Path: name, Source: summary.RepoTag, Architecture: parts[1]})
			summaries[name] = summary
			entry := PackageFile{Path: name, Size: header.Size, Kind: "file"}
			packageFiles = append(packageFiles, entry)
			if options.FileVisitor != nil {
				if visitErr := options.FileVisitor(entry); visitErr != nil {
					_ = gzipReader.Close()
					return nil, visitErr
				}
			}
			continue
		}
		limit, capture := singlePassCaptureLimit(name, header.Size, options.FileContentLimit)
		if capture && isPackageAssetPath(name) {
			if assetCaptureEntries >= MaxScreenshotCount+2 || header.Size > maxSinglePassAssetCaptureBytes-assetCaptureBytes {
				capture = false
				limit = 0
				uncapturedAssetPaths[name] = struct{}{}
			} else {
				assetCaptureEntries++
				assetCaptureBytes += header.Size
			}
		}
		var content []byte
		contentRead := false
		if capture {
			if header.Size > limit {
				if metadataFileLimit(name) > 0 || isPackageAssetPath(name) {
					_ = gzipReader.Close()
					return nil, fmt.Errorf("mpk: file %q exceeds %d bytes", name, limit)
				}
			} else {
				content, nextErr = readTarEntry(tarReader, header.Size)
				if nextErr != nil {
					_ = gzipReader.Close()
					return nil, fmt.Errorf("mpk: read %q: %w", name, nextErr)
				}
				contentRead = true
			}
		}
		if !contentRead {
			if _, nextErr = io.Copy(io.Discard, tarReader); nextErr != nil {
				_ = gzipReader.Close()
				return nil, fmt.Errorf("mpk: cannot scan %q: %w", name, nextErr)
			}
		}
		// 尽早保存 manifest 身份，使后续镜像归档硬拒绝也能直接
		// 生成审核事实，不必为了读取 app/version 再回退 MPK 流。
		if name == ManifestName && contentRead {
			var identity Manifest
			if yaml.Unmarshal(content, &identity) == nil && identity.validateIdentity() == nil {
				identityAppID, identityVersion = identity.ID, identity.Version
			}
		}
		files[name] = content
		entry := PackageFile{Path: name, Size: header.Size, Kind: "file", Content: content, ContentRead: contentRead}
		packageFiles = append(packageFiles, entry)
		if options.FileVisitor != nil {
			if visitErr := options.FileVisitor(entry); visitErr != nil {
				_ = gzipReader.Close()
				return nil, visitErr
			}
		}
	}
	if fileCount == 0 {
		_ = gzipReader.Close()
		return nil, fmt.Errorf("mpk: package is empty")
	}
	if _, err = io.Copy(io.Discard, gzipReader); err != nil {
		_ = gzipReader.Close()
		return nil, fmt.Errorf("mpk: cannot read gzip trailer: %w", err)
	}
	if err = gzipReader.Close(); err != nil {
		return nil, fmt.Errorf("mpk: close gzip package: %w", err)
	}
	if hasher.size > maxPackageSize {
		return nil, fmt.Errorf("mpk: package size %d exceeds %d bytes", hasher.size, maxPackageSize)
	}
	manifestData, exists := files[ManifestName]
	if !exists || manifestData == nil {
		return nil, fmt.Errorf("mpk: package root must contain %s", ManifestName)
	}
	var manifest Manifest
	if err = unmarshalSinglePassManifest(manifestData, &manifest); err != nil {
		return nil, err
	}
	if err = manifest.validateIdentity(); err != nil {
		return nil, err
	}
	// 与历史 GateMPK 保持稳定投影顺序，避免审核快照因 tar 成员顺序变化而漂移。
	sort.Strings(manifest.Architectures)
	sort.Strings(manifest.Capabilities)
	identityAppID, identityVersion = manifest.ID, manifest.Version
	if _, exists = present["settings.yaml"]; exists {
		return nil, fmt.Errorf("mpk: settings.yaml is not allowed; declare settings in manifest.yaml")
	}
	if options.ExpectedAppID != "" && manifest.ID != options.ExpectedAppID || options.ExpectedVersion != "" && manifest.Version != options.ExpectedVersion {
		return nil, fmt.Errorf("mpk: manifest identity %q/%q does not match application package %q/%q", manifest.ID, manifest.Version, options.ExpectedAppID, options.ExpectedVersion)
	}
	if err = validateIntegrationURL(manifest.IntegrationDocsURL); err != nil {
		return nil, err
	}
	for _, assetPath := range append([]string{icon64Name, icon256Name}, manifest.Screenshots...) {
		if _, skipped := uncapturedAssetPaths[assetPath]; skipped {
			return nil, fmt.Errorf("mpk: asset %q could not be captured within the single-pass resource budget", assetPath)
		}
	}
	if err = validateScreenshots(manifest.Screenshots, files); err != nil {
		return nil, err
	}
	if err = validateComposeSinglePass(manifest, files, overlayPaths, overlayDirs, options.ContractOnly); err != nil {
		return nil, err
	}
	about := files["about.md"]
	if !utf8.Valid(about) {
		return nil, fmt.Errorf("mpk: about.md is not valid UTF-8")
	}
	releaseNotes := files["release-notes.md"]
	if !utf8.Valid(releaseNotes) {
		return nil, fmt.Errorf("mpk: release-notes.md is not valid UTF-8")
	}
	if options.VersionExists != nil {
		exists, checkErr := options.VersionExists(manifest.Version)
		if checkErr != nil {
			return nil, checkErr
		}
		if exists {
			return nil, fmt.Errorf("mpk: version %q already exists", manifest.Version)
		}
	}
	if options.ApplicationExist != nil {
		for _, dependency := range manifest.Dependencies {
			if !dependency.Required {
				continue
			}
			found, checkErr := options.ApplicationExist(dependency.ID)
			if checkErr != nil {
				return nil, checkErr
			}
			if !found {
				return nil, fmt.Errorf("mpk: dependency application %q does not exist", dependency.ID)
			}
		}
	}
	compose := make(map[string]string, len(manifest.Architectures))
	for _, architecture := range manifest.Architectures {
		compose[architecture] = string(files["compose."+architecture+".yaml"])
	}
	metadata := PackageMetadata{Manifest: manifest, AboutMD: string(about), ReleaseNotesMD: string(releaseNotes), Settings: manifest.Settings, Compose: compose, FileCount: fileCount}
	if !options.ContractOnly {
		if err = validateScannedImages(metadata, images, summaries); err != nil {
			return nil, err
		}
	}
	var assets []PackageAsset
	if !options.ContractOnly {
		assets, err = assetsFromSinglePassFiles(manifest, files)
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(packageFiles, func(left, right int) bool { return packageFiles[left].Path < packageFiles[right].Path })
	sort.Slice(images, func(left, right int) bool {
		if images[left].Architecture != images[right].Architecture {
			return images[left].Architecture < images[right].Architecture
		}
		return images[left].Path < images[right].Path
	})
	result = &MPKScanResult{
		GateResult: GateResult{Metadata: metadata, Images: images, ImageSummaries: summaries, Assets: assets, Size: hasher.size, SHA256: hex.EncodeToString(hasher.hash.Sum(nil))},
		Files:      packageFiles,
	}
	if !options.ContractOnly {
		if _, err = ImageVolumeBindings(metadata, images, summaries); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type countingHashReader struct {
	reader io.Reader
	hash   hash.Hash
	size   int64
}

func (reader *countingHashReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		_, _ = reader.hash.Write(buffer[:count])
		reader.size += int64(count)
	}
	return count, err
}

func readTarEntry(reader io.Reader, size int64) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid negative entry size")
	}
	content := make([]byte, size)
	if _, err := io.ReadFull(reader, content); err != nil {
		return nil, err
	}
	return content, nil
}

func singlePassCaptureLimit(name string, size int64, custom func(string, int64) (int64, bool)) (int64, bool) {
	if limit := metadataFileLimit(name); limit > 0 {
		return limit, true
	}
	if isPackageAssetPath(name) {
		return packageAssetMaxSize(name), true
	}
	if custom != nil {
		return custom(name, size)
	}
	return 0, false
}

func isPackageAssetPath(name string) bool {
	return name == "icons/icon-64.png" || name == "icons/icon-256.png" || strings.HasPrefix(name, "screenshots/")
}

func packageAssetMaxSize(name string) int64 {
	switch name {
	case "icons/icon-64.png":
		return MaxIcon64Size
	case "icons/icon-256.png":
		return MaxIcon256Size
	default:
		return MaxScreenshotSize
	}
}

func unmarshalSinglePassManifest(data []byte, manifest *Manifest) error {
	if err := yaml.Unmarshal(data, manifest); err != nil {
		return fmt.Errorf("mpk: %s is invalid YAML: %w", ManifestName, err)
	}
	return nil
}

func validateScannedImages(metadata PackageMetadata, images []PackageImage, summaries map[string]*ImageArchiveSummary) error {
	expected, err := expectedComposeImages(metadata)
	if err != nil {
		return err
	}
	if err := validateArchitectureImageSets(metadata.Manifest.Architectures, expected); err != nil {
		return err
	}
	counts := make(map[string]int, len(expected))
	for _, image := range images {
		summary := summaries[image.Path]
		if summary == nil {
			return fmt.Errorf("mpk: image archive %q has no summary", image.Path)
		}
		if _, exists := expected[image.Architecture][summary.RepoTag]; !exists {
			return fmt.Errorf("mpk: image archive %q tag %q is not referenced by compose.%s.yaml", image.Path, summary.RepoTag, image.Architecture)
		}
		counts[image.Architecture]++
	}
	for _, architecture := range metadata.Manifest.Architectures {
		if counts[architecture] != len(expected[architecture]) {
			return fmt.Errorf("mpk: compose.%s.yaml image references and images/%s archives do not correspond one-to-one", architecture, architecture)
		}
	}
	return nil
}
