package contract

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const ManifestName = "manifest.yaml"

const (
	// DefaultMaxPackageSize 是商店与平台共用的单包默认上限（可由外层上传器进一步收紧）。
	DefaultMaxPackageSize int64 = 100 << 30
)

// MaxScreenshotSize 限制单张市场截图大小，防止公开商店端点被超大包成员占满内存。
const MaxScreenshotSize = 16 << 20

// MaxIconSize 限制公开市场图标大小，避免小型资源端点读取异常大文件。
const MaxIconSize = 4 << 20

// ErrScreenshotNotFound 表示市场资源缺失或未声明，同时不泄漏商店内部错误哨兵。
var ErrScreenshotNotFound = errors.New("mpk: screenshot not found")

// ErrIconNotFound 表示包内缺少固定的 256 像素市场图标。
var ErrIconNotFound = errors.New("mpk: icon not found")

// ErrOverlayNotFound 表示包内没有 overlay 目录。
var ErrOverlayNotFound = errors.New("mpk: overlay not found")

// PackageSummary 是 MPK 校验成功后返回的制品摘要。
type PackageSummary struct {
	Manifest  Manifest
	Size      int64
	SHA256    string
	AboutMD   string
	FileCount int
}

// ValidateOptions 携带服务侧事实参与校验；校验器保持纯函数，不访问数据库或对象存储。
type ValidateOptions struct {
	// MaxPackageSize 覆盖默认单包上限；为 0 时使用 DefaultMaxPackageSize。
	MaxPackageSize   int64
	ExpectedAppID    string
	ExpectedVersion  string
	VersionExists    func(version string) (bool, error)
	ApplicationExist func(appID string) (bool, error)
	// ContractOnly 只用于离线契约 fixture：验证 manifest、settings、compose
	// 注入和 overlay 规则，但不要求镜像归档。商店上传与真实安装必须保持 false。
	ContractOnly bool
}

// ValidateMPK 是历史公开入口，实际委托统一的 GateMPK，保证 CLI、Store 和平台调用相同的 v1 门禁。
func ValidateMPK(reader io.ReadSeeker, options ValidateOptions) (*PackageSummary, error) {
	result, err := GateMPK(reader, GateOptions{ValidateOptions: options})
	if err != nil {
		return nil, err
	}
	return &PackageSummary{
		Manifest:  result.Metadata.Manifest,
		Size:      result.Size,
		SHA256:    result.SHA256,
		AboutMD:   result.Metadata.AboutMD,
		FileCount: result.Metadata.FileCount,
	}, nil
}

// BuildContractMPK 把 apps/<app-id> 下的纯契约源码打包成确定性 tar.gz。
// 该 API 刻意拒绝 images 目录：镜像制品由真实发布流程生成，契约 fixture 不携带镜像。
func BuildContractMPK(sourceDir string, output io.Writer) error {
	return buildSourceMPK(sourceDir, output, true)
}

// BuildMPK 把开发者工作目录打包成确定性 tar.gz，并拒绝符号链接、设备文件等
// 非普通文件。它允许真实镜像归档，校验规则仍由 ValidateMPK 统一执行。
func BuildMPK(sourceDir string, output io.Writer) error {
	return buildSourceMPK(sourceDir, output, false)
}

func buildSourceMPK(sourceDir string, output io.Writer, contractOnly bool) error {
	info, err := os.Stat(sourceDir)
	if err != nil {
		return fmt.Errorf("mpk: stat source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mpk: source %q is not a directory", sourceDir)
	}

	type sourceEntry struct {
		name string
		path string
		size int64
		dir  bool
	}
	var entries []sourceEntry
	err = filepath.WalkDir(sourceDir, func(itemPath string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			// 空的 overlay 目录也是有意义的挂载源，必须保留目录条目。
			relative, err := filepath.Rel(sourceDir, itemPath)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(relative)
			if name == "overlay" || strings.HasPrefix(name, "overlay/") {
				entries = append(entries, sourceEntry{name: strings.TrimSuffix(name, "/") + "/", path: itemPath, dir: true})
			}
			return nil
		}
		if !item.Type().IsRegular() {
			return fmt.Errorf("mpk: contract source %q contains a non-regular file", itemPath)
		}
		relative, err := filepath.Rel(sourceDir, itemPath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if contractOnly && name == "README.md" {
			return nil
		}
		if contractOnly && (name == "images" || strings.HasPrefix(name, "images/")) {
			return fmt.Errorf("mpk: contract source must not contain image archives")
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		entries = append(entries, sourceEntry{name: name, path: itemPath, size: info.Size()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("mpk: read source: %w", err)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].name < entries[right].name
	})

	gzipWriter := gzip.NewWriter(output)
	tarWriter := tar.NewWriter(gzipWriter)
	appendFile := func(item sourceEntry) error {
		if err := tarWriter.WriteHeader(&tar.Header{Name: item.name, Size: item.size, Mode: 0o644}); err != nil {
			return err
		}
		file, err := os.Open(item.path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(tarWriter, file, item.size)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	}
	if manifestIndex := slices.IndexFunc(entries, func(item sourceEntry) bool { return item.name == ManifestName }); manifestIndex >= 0 {
		manifest := entries[manifestIndex]
		entries = slices.Delete(entries, manifestIndex, manifestIndex+1)
		if err := appendFile(manifest); err != nil {
			return err
		}
	}
	for _, item := range entries {
		if item.dir {
			if err := tarWriter.WriteHeader(&tar.Header{Name: item.name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return err
			}
			continue
		}
		if err := appendFile(item); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	return gzipWriter.Close()
}

// ReadMPKScreenshot 从包内提取一个已声明且安全的截图路径。
// 白名单刻意显式：未声明成员不会因为存在于包内就变成公开市场资源。
func ReadMPKScreenshot(reader io.ReadSeeker, name string) ([]byte, error) {
	if err := ValidateScreenshotPath(name); err != nil {
		return nil, err
	}
	return readMPKMember(reader, name, MaxScreenshotSize, ErrScreenshotNotFound)
}

// ReadMPKIcon 从包内提取固定路径的 256 像素 PNG 市场图标。
func ReadMPKIcon(reader io.ReadSeeker) ([]byte, error) {
	return readMPKMember(reader, "icons/icon-256.png", MaxIconSize, ErrIconNotFound)
}

// readMPKMember 只读取一个已知安全的普通文件，并统一限制公开资源大小。
func readMPKMember(reader io.ReadSeeker, name string, maxSize int64, notFound error) ([]byte, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind package: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%w: %q", notFound, name)
		}
		if err != nil {
			return nil, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		cleaned := path.Clean(header.Name)
		if path.IsAbs(header.Name) || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
			return nil, fmt.Errorf("mpk: unsafe path %q", header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("mpk: forbidden non-regular node %q", cleaned)
		}
		if cleaned != name {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(tarReader, maxSize+1))
		if err != nil {
			return nil, fmt.Errorf("mpk: cannot read asset %q: %w", name, err)
		}
		if int64(len(content)) > maxSize {
			return nil, fmt.Errorf("mpk: asset %q exceeds %d bytes", name, maxSize)
		}
		return content, nil
	}
}

// OverlayTree 是包内 overlay 的逻辑目录树，目录和文件分开保存，因而空目录
// 也能作为合法的目录挂载源传递到 Agent。
type OverlayTree struct {
	Files       map[string][]byte
	Directories map[string]struct{}
}

// ReadMPKOverlay 提取包内 overlay 下的全部普通文件；目录信息请使用 ReadMPKOverlayTree。
func ReadMPKOverlay(reader io.ReadSeeker) (map[string][]byte, error) {
	tree, err := ReadMPKOverlayTree(reader)
	if err != nil {
		return nil, err
	}
	return tree.Files, nil
}

// ReadMPKOverlayTree 提取包内 overlay 的文件和目录（包括空目录）。
func ReadMPKOverlayTree(reader io.ReadSeeker) (OverlayTree, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return OverlayTree{}, fmt.Errorf("mpk: rewind package: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return OverlayTree{}, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	defer gzipReader.Close()
	tree := OverlayTree{Files: make(map[string][]byte), Directories: make(map[string]struct{})}
	found := false
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return OverlayTree{}, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		cleaned := path.Clean(header.Name)
		if path.IsAbs(header.Name) || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
			return OverlayTree{}, fmt.Errorf("mpk: unsafe overlay path %q", header.Name)
		}
		if cleaned != "overlay" && !strings.HasPrefix(cleaned, "overlay/") {
			continue
		}
		cleaned = strings.TrimPrefix(cleaned, "overlay/")
		if cleaned == "" || cleaned == "." {
			continue
		}
		if !strings.HasPrefix(header.Name, "overlay/") {
			continue
		}
		found = true
		switch header.Typeflag {
		case tar.TypeDir:
			tree.Directories[cleaned] = struct{}{}
			continue
		case tar.TypeReg:
			// continue below
		default:
			return OverlayTree{}, fmt.Errorf("mpk: forbidden non-regular node %q", cleaned)
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			return OverlayTree{}, fmt.Errorf("mpk: read overlay file %q: %w", cleaned, err)
		}
		tree.Files[cleaned] = content
	}
	if !found {
		return OverlayTree{}, ErrOverlayNotFound
	}
	return tree, nil
}

// ValidateScreenshotPath 应用与上传一致的市场规则：包内相对路径必须位于 screenshots/ 且类型安全。
func ValidateScreenshotPath(name string) error {
	if !strings.HasPrefix(name, "screenshots/") || name == "screenshots/" {
		return fmt.Errorf("mpk: screenshot %q must be under screenshots/", name)
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".webp":
	default:
		return fmt.Errorf("mpk: screenshot %q must be png, jpg, or webp", name)
	}
	return nil
}

func validateIntegrationURL(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" {
		return fmt.Errorf("mpk: integration_docs_url must be an absolute https URL")
	}
	return nil
}

func validateScreenshots(manifest []string, files map[string][]byte) error {
	if len(manifest) > MaxScreenshotCount {
		return fmt.Errorf("mpk: screenshots exceed %d entries", MaxScreenshotCount)
	}
	seen := make(map[string]struct{}, len(manifest))
	for _, screenshot := range manifest {
		if err := ValidateScreenshotPath(screenshot); err != nil {
			return err
		}
		if _, duplicate := seen[screenshot]; duplicate {
			return fmt.Errorf("mpk: duplicate screenshot %q", screenshot)
		}
		seen[screenshot] = struct{}{}
		if _, exists := files[screenshot]; !exists {
			return fmt.Errorf("mpk: screenshot %q does not exist", screenshot)
		}
	}
	return nil
}
