package contract

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
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
	maxPackagePathSize  = 1024
	maxPackageEntries   = 10000
	maxManifestSize     = 1 << 20
	maxSettingsSize     = 1 << 20
	maxAboutSize        = 4 << 20
	maxReleaseNotesSize = 4 << 20
	maxComposeSize      = 4 << 20
	// MaxSkillFiles 和 MaxSkillSize 限制单个 MPK 中 Skill 的元数据预算。
	// Skill 正文不会进入 PackageMetadata 持久化快照。
	MaxSkillFiles    = 512
	MaxSkillSize     = 16 << 20
	maxSkillFileSize = MaxSkillSize
)

// SkillFileMetadata 是 Skill 文件的不可变内容事实；Content 不进入该结构。
type SkillFileMetadata struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ValidateStandaloneSkillFiles 校验独立 Skill ZIP 解包后的文件清单。
//
// 独立 Skill 不携带 MPK manifest，但继续使用应用 Skill 的目录、SKILL.md
// frontmatter、大小和 UTF-8 规则，保证商店、平台上传和离线导入共用同一门禁。
// files 的 key 必须是 skills/{name}/{path}，函数只返回摘要，不保留正文。
func ValidateStandaloneSkillFiles(skillName string, files map[string][]byte) ([]SkillFileMetadata, error) {
	skillName = strings.TrimSpace(skillName)
	if !providedNamePattern.MatchString(skillName) {
		return nil, fmt.Errorf("mpk: standalone skill name %q is invalid", skillName)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("mpk: standalone skill %q is empty", skillName)
	}
	count := 0
	var total int64
	result := make([]SkillFileMetadata, 0, len(files))
	hasManifest := false
	prefix := "skills/" + skillName + "/"
	for name, content := range files {
		clean := path.Clean(name)
		if clean != name || !strings.HasPrefix(name, prefix) || path.IsAbs(name) || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("mpk: standalone skill contains unsafe path %q", name)
		}
		relative := strings.TrimPrefix(name, prefix)
		if relative == "" || relative == "." || strings.HasPrefix(relative, "../") {
			return nil, fmt.Errorf("mpk: standalone skill path %q is invalid", name)
		}
		count++
		total += int64(len(content))
		if count > MaxSkillFiles || total > MaxSkillSize || int64(len(content)) > maxSkillFileSize {
			return nil, fmt.Errorf("mpk: standalone skill %q exceeds %d files or %d bytes", skillName, MaxSkillFiles, MaxSkillSize)
		}
		if relative == "SKILL.md" {
			hasManifest = true
			if err := validateSkillDocument(skillName, content); err != nil {
				return nil, fmt.Errorf("mpk: standalone skill %q: %w", skillName, err)
			}
		} else if !utf8.Valid(content) {
			return nil, fmt.Errorf("mpk: standalone skill file %q is not valid UTF-8", name)
		}
		digest := sha256.Sum256(content)
		result = append(result, SkillFileMetadata{Path: name, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])})
	}
	if !hasManifest {
		return nil, fmt.Errorf("mpk: standalone skill %q must contain SKILL.md", skillName)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

// PackageMetadata 是 Master 准备应用包时需要持久化的小型只读快照。
// Compose 按架构保存原始 YAML；镜像归档和其他大文件永远不会进入该结构。
type PackageMetadata struct {
	Manifest Manifest `json:"manifest"`
	AboutMD  string   `json:"aboutMarkdown,omitempty"`
	// ReleaseNotesMD 是当前 MPK 版本的可选更新说明，不参与安装契约校验。
	ReleaseNotesMD string              `json:"releaseNotesMarkdown,omitempty"`
	Settings       []Setting           `json:"settings"`
	Compose        map[string]string   `json:"compose"`
	SkillFiles     []SkillFileMetadata `json:"skillFiles,omitempty"`
	FileCount      int                 `json:"fileCount"`
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
	files, present, overlayPaths, overlayDirs, fileCount, scanErr := scanPackageMetadataTar(gzipReader, maxPackageSize)
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
	if err := validateCompose(manifest, files, overlayPaths, overlayDirs, true); err != nil {
		return nil, err
	}
	skillFiles, err := collectSkillFiles(manifest, files)
	if err != nil {
		return nil, err
	}
	if about := files["about.md"]; !utf8.Valid(about) {
		return nil, fmt.Errorf("mpk: about.md is not valid UTF-8")
	}
	if releaseNotes := files["release-notes.md"]; !utf8.Valid(releaseNotes) {
		return nil, fmt.Errorf("mpk: release-notes.md is not valid UTF-8")
	}

	sort.Strings(manifest.Architectures)
	sort.Strings(manifest.Capabilities)
	compose := make(map[string]string, len(manifest.Architectures))
	for _, architecture := range manifest.Architectures {
		compose[architecture] = string(files["compose."+architecture+".yaml"])
	}
	return &PackageMetadata{
		Manifest: manifest, AboutMD: string(files["about.md"]), ReleaseNotesMD: string(files["release-notes.md"]), Settings: manifest.Settings,
		Compose: compose, SkillFiles: skillFiles, FileCount: fileCount,
	}, nil
}

func scanPackageMetadataTar(reader io.Reader, maxPackageSize int64) (map[string][]byte, map[string]struct{}, map[string]struct{}, map[string]struct{}, int, error) {
	tarReader := tar.NewReader(reader)
	files := make(map[string][]byte)
	present := make(map[string]struct{})
	overlayPaths := make(map[string]struct{})
	overlayDirs := make(map[string]struct{})
	fileCount := 0
	entryCount := 0
	var skillBytes int64
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
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		entryCount++
		if entryCount > maxPackageEntries {
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: package exceeds %d entries", maxPackageEntries)
		}
		name := path.Clean(header.Name)
		if header.Name == "" || len(header.Name) > maxPackagePathSize || name == "." ||
			path.IsAbs(header.Name) || strings.HasPrefix(name, "../") || name == ".." {
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: unsafe path %q", header.Name)
		}
		if _, duplicate := present[name]; duplicate {
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: duplicate entry %q", name)
		}
		present[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if name == "overlay" || strings.HasPrefix(name, "overlay/") {
				overlayDirs[name] = struct{}{}
			}
			continue
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxPackageSize || expanded > expandedLimit-header.Size {
				return nil, nil, nil, nil, 0, fmt.Errorf("mpk: expanded package size exceeds %d bytes", expandedLimit)
			}
			expanded += header.Size
			if strings.HasPrefix(name, "skills/") {
				if header.Size > maxSkillFileSize || skillBytes > MaxSkillSize-header.Size {
					return nil, nil, nil, nil, 0, fmt.Errorf("mpk: skill files exceed %d bytes", MaxSkillSize)
				}
				skillBytes += header.Size
			}
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: forbidden non-regular node %q", name)
		default:
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: unsupported tar node %q", name)
		}
		fileCount++
		if strings.HasPrefix(name, "overlay/") {
			overlayPaths[name] = struct{}{}
		}
		limit := metadataFileLimit(name)
		if limit == 0 {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return nil, nil, nil, nil, 0, fmt.Errorf("mpk: cannot scan %q: %w", name, err)
			}
			continue
		}
		content, err := io.ReadAll(io.LimitReader(tarReader, limit+1))
		if err != nil {
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: cannot read %q: %w", name, err)
		}
		if int64(len(content)) > limit {
			return nil, nil, nil, nil, 0, fmt.Errorf("mpk: metadata file %q exceeds %d bytes", name, limit)
		}
		files[name] = content
	}
	if fileCount == 0 {
		return nil, nil, nil, nil, 0, fmt.Errorf("mpk: package is empty")
	}
	return files, present, overlayPaths, overlayDirs, fileCount, nil
}

func metadataFileLimit(name string) int64 {
	switch {
	case name == ManifestName:
		return maxManifestSize
	case name == "about.md":
		return maxAboutSize
	case name == "release-notes.md":
		return maxReleaseNotesSize
	case name == "settings.yaml":
		return maxSettingsSize
	case name == "compose.amd64.yaml" || name == "compose.arm64.yaml":
		return maxComposeSize
	case isSkillFilePath(name):
		return maxSkillFileSize
	default:
		return 0
	}
}

func isSkillFilePath(name string) bool {
	return strings.HasPrefix(name, "skills/") && strings.TrimPrefix(name, "skills/") != ""
}

// collectSkillFiles 校验 manifest 声明与包内 skills/ 文件的一一对应关系，并生成稳定清单。
func collectSkillFiles(manifest Manifest, files map[string][]byte) ([]SkillFileMetadata, error) {
	declared := make(map[string]struct{}, len(manifest.Provides.Skills))
	for _, name := range manifest.Provides.Skills {
		declared[name] = struct{}{}
	}
	counts := make(map[string]int, len(declared))
	bytesBySkill := make(map[string]int64, len(declared))
	hasManifest := make(map[string]bool, len(declared))
	result := make([]SkillFileMetadata, 0)
	for name, content := range files {
		if !isSkillFilePath(name) {
			continue
		}
		relative := strings.TrimPrefix(name, "skills/")
		parts := strings.SplitN(relative, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("mpk: skill file %q must be skills/{name}/{path}", name)
		}
		skillName, relativePath := parts[0], parts[1]
		if _, ok := declared[skillName]; !ok {
			return nil, fmt.Errorf("mpk: skill %q is not declared in manifest.provides.skills", skillName)
		}
		counts[skillName]++
		bytesBySkill[skillName] += int64(len(content))
		if counts[skillName] > MaxSkillFiles || bytesBySkill[skillName] > MaxSkillSize {
			return nil, fmt.Errorf("mpk: skill %q exceeds %d files or %d bytes", skillName, MaxSkillFiles, MaxSkillSize)
		}
		if relativePath == "SKILL.md" {
			hasManifest[skillName] = true
			if err := validateSkillDocument(skillName, content); err != nil {
				return nil, fmt.Errorf("mpk: skill %q: %w", skillName, err)
			}
		}
		digest := sha256.Sum256(content)
		result = append(result, SkillFileMetadata{Path: name, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])})
	}
	for _, skillName := range manifest.Provides.Skills {
		if !hasManifest[skillName] {
			return nil, fmt.Errorf("mpk: skill %q must contain SKILL.md", skillName)
		}
	}
	if len(result) == 0 && len(manifest.Provides.Skills) > 0 {
		return nil, fmt.Errorf("mpk: manifest declares skills but package contains no skill files")
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

func validateSkillDocument(skillName string, content []byte) error {
	if !utf8.Valid(content) {
		return fmt.Errorf("SKILL.md is not valid UTF-8")
	}
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return fmt.Errorf("SKILL.md frontmatter is required")
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	frontmatter := text[4 : 4+end]
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(frontmatter), &node); err != nil {
		return fmt.Errorf("SKILL.md frontmatter is invalid YAML: %w", err)
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("SKILL.md frontmatter must be a mapping")
	}
	allowed := map[string]struct{}{"name": {}, "description": {}, "license": {}, "compatibility": {}, "metadata": {}}
	var nameValue, descriptionValue string
	for _, pair := range mappingPairs(node.Content[0]) {
		if _, ok := allowed[pair.key]; !ok {
			return fmt.Errorf("SKILL.md frontmatter has unknown field %q", pair.key)
		}
		if pair.key == "name" || pair.key == "description" || pair.key == "license" || pair.key == "compatibility" {
			if pair.value.Kind != yaml.ScalarNode || pair.value.Tag != "!!str" {
				return fmt.Errorf("SKILL.md frontmatter field %q must be a string", pair.key)
			}
			switch pair.key {
			case "name":
				nameValue = pair.value.Value
			case "description":
				descriptionValue = pair.value.Value
			}
		}
		if pair.key == "metadata" {
			if pair.value.Kind != yaml.MappingNode {
				return fmt.Errorf("SKILL.md frontmatter metadata must be a mapping")
			}
			for _, metadataPair := range mappingPairs(pair.value) {
				if metadataPair.value.Kind != yaml.ScalarNode || metadataPair.value.Tag != "!!str" {
					return fmt.Errorf("SKILL.md frontmatter metadata values must be strings")
				}
			}
		}
	}
	if nameValue != skillName || !providedNamePattern.MatchString(nameValue) {
		return fmt.Errorf("SKILL.md name must equal directory name %q", skillName)
	}
	if descriptionValue == "" || len(descriptionValue) > 1024 {
		return fmt.Errorf("SKILL.md description is required and must be at most 1024 bytes")
	}
	return nil
}
