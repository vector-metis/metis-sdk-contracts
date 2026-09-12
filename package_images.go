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

	"gopkg.in/yaml.v3"
)

// PackageImageArchive 是 MPK 顺序扫描时当前可读的单个镜像归档。
// Body 只在 visitor 调用期间有效；调用方必须完整读取，不能保存后异步使用。
type PackageImageArchive struct {
	Path         string
	Architecture string
	Size         int64
	Body         io.Reader
}

// PackageImage 是完成归档身份校验后的稳定镜像事实。
type PackageImage struct {
	Path         string `json:"path"`
	Source       string `json:"source"`
	Architecture string `json:"architecture"`
}

// InspectPackageImages 顺序访问 MPK 中全部 images/{arch}/*.tar，并把归档元数据与
// Compose、目录架构和其他声明架构交叉校验。函数不会物化镜像正文；visitor 每次只接收
// 当前 tar member，适合调用方流式写入一个受控临时文件。
func InspectPackageImages(
	reader io.ReadSeeker,
	metadata PackageMetadata,
	visitor func(PackageImageArchive) (*ImageArchiveSummary, error),
) ([]PackageImage, error) {
	if reader == nil || visitor == nil {
		return nil, fmt.Errorf("mpk: package image reader and visitor are required")
	}
	expected, err := expectedComposeImages(metadata)
	if err != nil {
		return nil, err
	}
	if err := validateArchitectureImageSets(metadata.Manifest.Architectures, expected); err != nil {
		return nil, err
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind package images: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()

	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	images, scanErr := inspectPackageImageTar(tar.NewReader(gzipReader), expected, visitor)
	closeErr := gzipReader.Close()
	if scanErr != nil || closeErr != nil {
		return nil, errors.Join(scanErr, closeErr)
	}
	for _, architecture := range metadata.Manifest.Architectures {
		if len(expected[architecture]) != countArchitectureImages(images, architecture) {
			return nil, fmt.Errorf("mpk: compose.%s.yaml image references and images/%s archives do not correspond one-to-one", architecture, architecture)
		}
	}
	sort.Slice(images, func(left, right int) bool {
		if images[left].Architecture != images[right].Architecture {
			return images[left].Architecture < images[right].Architecture
		}
		return images[left].Path < images[right].Path
	})
	return images, nil
}

func expectedComposeImages(metadata PackageMetadata) (map[string]map[string]struct{}, error) {
	result := make(map[string]map[string]struct{}, len(metadata.Manifest.Architectures))
	for _, architecture := range metadata.Manifest.Architectures {
		data, exists := metadata.Compose[architecture]
		if !exists {
			return nil, fmt.Errorf("mpk: missing compose.%s.yaml for declared architecture", architecture)
		}
		var document composeDocument
		if err := yaml.Unmarshal([]byte(data), &document); err != nil {
			return nil, fmt.Errorf("mpk: compose.%s.yaml is invalid YAML: %w", architecture, err)
		}
		images := make(map[string]struct{}, len(document.Services))
		for serviceName, service := range document.Services {
			if service.Image == "" {
				return nil, fmt.Errorf("mpk: compose.%s.yaml service %q has no image", architecture, serviceName)
			}
			if _, err := parseExplicitImageTag(service.Image); err != nil {
				return nil, fmt.Errorf("mpk: compose.%s.yaml service %q image is invalid: %w", architecture, serviceName, err)
			}
			images[service.Image] = struct{}{}
		}
		if len(images) == 0 {
			return nil, fmt.Errorf("mpk: compose.%s.yaml has no images", architecture)
		}
		result[architecture] = images
	}
	return result, nil
}

func validateArchitectureImageSets(architectures []string, expected map[string]map[string]struct{}) error {
	if len(architectures) == 0 {
		return fmt.Errorf("mpk: at least one architecture is required")
	}
	baseline := expected[architectures[0]]
	for _, architecture := range architectures[1:] {
		images := expected[architecture]
		if len(images) != len(baseline) {
			return fmt.Errorf("mpk: compose image references differ between architectures %q and %q", architectures[0], architecture)
		}
		for source := range baseline {
			if _, exists := images[source]; !exists {
				return fmt.Errorf("mpk: image reference %q is not shared by architectures %q and %q", source, architectures[0], architecture)
			}
		}
	}
	return nil
}

func inspectPackageImageTar(
	tarReader *tar.Reader,
	expected map[string]map[string]struct{},
	visitor func(PackageImageArchive) (*ImageArchiveSummary, error),
) ([]PackageImage, error) {
	images := make([]PackageImage, 0)
	seenPaths := make(map[string]struct{})
	seenSources := make(map[string]map[string]struct{}, len(expected))
	for architecture := range expected {
		seenSources[architecture] = make(map[string]struct{})
	}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return images, nil
		}
		if err != nil {
			return nil, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		cleaned := path.Clean(header.Name)
		if header.Name == "" || path.IsAbs(header.Name) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("mpk: unsafe path %q", header.Name)
		}
		if _, duplicate := seenPaths[cleaned]; duplicate {
			return nil, fmt.Errorf("mpk: duplicate entry %q", cleaned)
		}
		seenPaths[cleaned] = struct{}{}
		if !strings.HasPrefix(cleaned, "images/") || header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			return nil, fmt.Errorf("mpk: image archive %q is not a regular file", cleaned)
		}
		parts := strings.Split(cleaned, "/")
		if len(parts) < 3 || path.Ext(cleaned) != ".tar" {
			return nil, fmt.Errorf("mpk: image archive %q must be images/{arch}/*.tar", cleaned)
		}
		architecture := parts[1]
		architectureImages, declared := expected[architecture]
		if !declared {
			return nil, fmt.Errorf("mpk: image archive %q uses undeclared architecture %q", cleaned, architecture)
		}
		if header.Size < 0 {
			return nil, fmt.Errorf("mpk: image archive %q has invalid size", cleaned)
		}
		body := &io.LimitedReader{R: tarReader, N: header.Size}
		summary, err := visitor(PackageImageArchive{
			Path: cleaned, Architecture: architecture, Size: header.Size, Body: body,
		})
		if err != nil {
			return nil, fmt.Errorf("mpk: inspect image archive %q: %w", cleaned, err)
		}
		if body.N != 0 {
			return nil, fmt.Errorf("mpk: image archive visitor did not consume %q", cleaned)
		}
		if summary == nil || summary.Architecture != architecture {
			return nil, fmt.Errorf("mpk: image archive %q architecture does not match directory architecture %q", cleaned, architecture)
		}
		if _, exists := architectureImages[summary.RepoTag]; !exists {
			return nil, fmt.Errorf("mpk: image archive %q tag %q is not referenced by compose.%s.yaml", cleaned, summary.RepoTag, architecture)
		}
		if _, duplicate := seenSources[architecture][summary.RepoTag]; duplicate {
			return nil, fmt.Errorf("mpk: images/%s contains duplicate image tag %q", architecture, summary.RepoTag)
		}
		seenSources[architecture][summary.RepoTag] = struct{}{}
		images = append(images, PackageImage{Path: cleaned, Source: summary.RepoTag, Architecture: architecture})
	}
}

func countArchitectureImages(images []PackageImage, architecture string) int {
	count := 0
	for _, image := range images {
		if image.Architecture == architecture {
			count++
		}
	}
	return count
}
