package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var (
	gateComposePathPattern = regexp.MustCompile(`compose\.(?:amd64|arm64)\.yaml`)
	gateImagePathPattern   = regexp.MustCompile(`images/(?:amd64|arm64)/[^\s"']+\.tar`)
	gateAssetPathPattern   = regexp.MustCompile(`(?:asset|screenshot) "([^"]+)"`)
	gateServicePattern     = regexp.MustCompile(`service "([^"]+)"`)
)

// GateFinding 是所有 MPK 消费方共享的稳定门禁结果。RuleID 面向机器和审核界面，Message 面向开发者。
type GateFinding struct {
	RuleID  string `json:"rule_id" yaml:"rule_id"`
	Path    string `json:"path,omitempty" yaml:"path,omitempty"`
	Context string `json:"context,omitempty" yaml:"context,omitempty"`
	Message string `json:"message" yaml:"message"`
}

// GateError 保留原始错误，同时提供一个可序列化的首条门禁结果。
type GateError struct {
	Finding GateFinding
	Err     error
}

func (e *GateError) Error() string { return e.Finding.Error() }
func (e *GateError) Unwrap() error { return e.Err }

func gateError(err error) error {
	if err == nil {
		return nil
	}
	var existing *GateError
	if errors.As(err, &existing) {
		return err
	}
	message := err.Error()
	rule := "MPK-MANIFEST"
	switch {
	case strings.Contains(message, "package size") || strings.Contains(message, "expanded package size"):
		rule = "MPK-SIZE"
	case strings.Contains(message, "settings.yaml"):
		rule = "MPK-MANIFEST-LEGACY-SETTINGS"
	case strings.Contains(message, "dependency application"):
		rule = "MPK-MANIFEST-DEPENDENCY"
	case strings.Contains(message, "required asset") || strings.Contains(message, "screenshot") || strings.Contains(message, "icon"):
		rule = "MPK-ASSET"
	case strings.Contains(message, "cannot declare ports"):
		rule = "MPK-COMPOSE-PORTS"
	case strings.Contains(message, "host network"):
		rule = "MPK-COMPOSE-HOST-NETWORK"
	case strings.Contains(message, "cannot declare volumes") || strings.Contains(message, "volumes_from") || strings.Contains(message, "tmpfs"):
		rule = "MPK-COMPOSE-VOLUMES"
	case strings.Contains(message, "cannot declare environment") || strings.Contains(message, "env_file"):
		rule = "MPK-COMPOSE-ENVIRONMENT"
	case strings.Contains(message, "cannot declare extra_hosts"):
		rule = "MPK-COMPOSE-EXTRA-HOSTS"
	case strings.Contains(message, "interpolation") || strings.Contains(message, "variable substitution"):
		rule = "MPK-COMPOSE-INTERPOLATION"
	case strings.Contains(message, "platform label"):
		rule = "MPK-COMPOSE-PLATFORM-LABEL"
	case strings.Contains(message, "service sets differ") || strings.Contains(message, "undeclared service") || strings.Contains(message, "missing declared service"):
		rule = "MPK-MANIFEST-SERVICE-MISMATCH"
	case strings.Contains(message, "overlay"):
		rule = "MPK-MANIFEST-OVERLAY"
	case strings.Contains(message, "image") && strings.Contains(message, "architecture") && strings.Contains(message, "does not match"):
		rule = "MPK-IMAGE-ARCH-MISMATCH"
	case strings.Contains(message, "image"):
		rule = "MPK-IMAGE"
	case strings.Contains(message, "tar") || strings.Contains(message, "unsafe path"):
		rule = "MPK-TAR"
	case strings.Contains(message, "compose."):
		rule = "MPK-COMPOSE"
	}
	return &GateError{Finding: GateFinding{
		RuleID: rule, Path: gateFindingPath(message), Context: gateFindingContext(message), Message: message,
	}, Err: err}
}

func gateFindingPath(message string) string {
	if matched := gateImagePathPattern.FindString(message); matched != "" {
		return matched
	}
	if matched := gateComposePathPattern.FindString(message); matched != "" {
		return matched
	}
	if matched := gateAssetPathPattern.FindStringSubmatch(message); len(matched) == 2 {
		return matched[1]
	}
	for _, name := range []string{ManifestName, "settings.yaml", "about.md"} {
		if strings.Contains(message, name) || name == ManifestName && strings.Contains(message, "manifest:") {
			return name
		}
	}
	return ""
}

func gateFindingContext(message string) string {
	if matched := gateServicePattern.FindStringSubmatch(message); len(matched) == 2 {
		return "service=" + matched[1]
	}
	return ""
}

// FindingsFromError converts an arbitrary validation error into the stable result shape.
func FindingsFromError(err error) []GateFinding {
	if err == nil {
		return nil
	}
	var finding *GateError
	if errors.As(err, &finding) {
		return []GateFinding{finding.Finding}
	}
	return []GateFinding{{RuleID: "MPK-VALIDATION", Message: err.Error()}}
}

func (f GateFinding) Error() string {
	if f.Path == "" {
		return fmt.Sprintf("%s: %s", f.RuleID, f.Message)
	}
	return fmt.Sprintf("%s (%s): %s", f.RuleID, f.Path, f.Message)
}

// GateOptions 是所有 MPK 消费方共享的门禁输入。
// ContractOnly 仅用于没有镜像归档的契约 fixture；Store、在线准备、离线导入和生产 CLI
// 必须保持 false。ImageVisitor 只在生产门禁中接收当前镜像归档，调用方必须在返回前完整读取 Body。
type GateOptions struct {
	ValidateOptions
	ImageVisitor func(PackageImageArchive) (*ImageArchiveSummary, error)
}

// GateResult 是 MPK v1 门禁通过后的统一结果。Metadata 只包含有界契约快照，镜像正文不会进入
// 结果；ImageSummaries 由调用方的 ImageVisitor 产生，适合直接持久化审核快照或继续导入 Registry。
type GateResult struct {
	Metadata       PackageMetadata
	Images         []PackageImage
	ImageSummaries map[string]*ImageArchiveSummary
	Assets         []PackageAsset
	Size           int64
	SHA256         string
}

// GateMPK 顺序执行 v1 的完整硬门禁，并统一返回稳定的 GateFinding 错误。
// 实现最多需要几次顺序扫描，但任何一次扫描都不会把完整 MPK 或镜像归档读入内存。
func GateMPK(reader io.ReadSeeker, options GateOptions) (result *GateResult, err error) {
	defer func() { err = gateError(err) }()
	if reader == nil {
		return nil, fmt.Errorf("mpk: package reader is required")
	}
	size, digest, err := hashPackage(reader)
	if err != nil {
		return nil, err
	}
	maxPackageSize := options.MaxPackageSize
	if maxPackageSize <= 0 {
		maxPackageSize = DefaultMaxPackageSize
	}
	if size > maxPackageSize {
		return nil, fmt.Errorf("mpk: package size %d exceeds %d bytes", size, maxPackageSize)
	}
	metadata, err := inspectPackageMetadata(reader, options.ExpectedAppID, options.ExpectedVersion, maxPackageSize)
	if err != nil {
		return nil, err
	}
	manifest := metadata.Manifest
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
	result = &GateResult{Metadata: *metadata, Size: size, SHA256: digest}
	if options.ContractOnly {
		return result, nil
	}
	visitor := options.ImageVisitor
	if visitor == nil {
		visitor = validateImageArchive
	}
	result.ImageSummaries = make(map[string]*ImageArchiveSummary)
	result.Images, err = InspectPackageImages(reader, *metadata, func(archive PackageImageArchive) (*ImageArchiveSummary, error) {
		summary, visitErr := visitor(archive)
		if visitErr == nil {
			result.ImageSummaries[archive.Path] = summary
		}
		return summary, visitErr
	})
	if err != nil {
		return nil, err
	}
	result.Assets, err = InspectPackageAssets(reader, manifest)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func hashPackage(reader io.ReadSeeker) (int64, string, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, "", fmt.Errorf("mpk: rewind package: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return 0, "", fmt.Errorf("mpk: hash package: %w", err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// validateImageArchive 是 CLI 和没有额外副作用的校验调用方使用的默认 visitor。
// 镜像 tar 只落到临时文件，避免 100 GiB MPK 进入 Go 堆。
func validateImageArchive(archive PackageImageArchive) (*ImageArchiveSummary, error) {
	temporary, err := os.CreateTemp("", "metis-gate-image-*.tar")
	if err != nil {
		return nil, fmt.Errorf("mpk: create image inspection file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := io.Copy(temporary, archive.Body); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("mpk: receive image archive: %w", err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("mpk: rewind image archive: %w", err)
	}
	summary, inspectErr := InspectImageArchive(temporary)
	closeErr := temporary.Close()
	if inspectErr != nil || closeErr != nil {
		return nil, errors.Join(inspectErr, closeErr)
	}
	return summary, nil
}
