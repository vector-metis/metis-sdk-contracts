package contract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
)

// MaxScreenshotCount 限制单个版本可公开展示的截图数量。
const MaxScreenshotCount = 8

const (
	icon64Name  = "icons/icon-64.png"
	icon256Name = "icons/icon-256.png"
)

// PackageAsset 是从 MPK 提取的单个有界展示资源。
type PackageAsset struct {
	Name      string
	MediaType string
	Content   []byte
}

type packageAssetRule struct {
	maxSize        int64
	expectedFormat string
	width          int
	height         int
}

// InspectPackageAssets 顺序扫描 MPK，只提取并校验固定图标和 manifest 声明的截图。
// 返回顺序固定为 64 图标、256 图标、manifest 截图顺序。
func InspectPackageAssets(reader io.ReadSeeker, manifest Manifest) ([]PackageAsset, error) {
	if reader == nil {
		return nil, fmt.Errorf("mpk: package asset reader is required")
	}
	if len(manifest.Screenshots) > MaxScreenshotCount {
		return nil, fmt.Errorf("mpk: screenshots exceed %d entries", MaxScreenshotCount)
	}

	rules := map[string]packageAssetRule{
		icon64Name:  {maxSize: MaxIcon64Size, expectedFormat: "png", width: 64, height: 64},
		icon256Name: {maxSize: MaxIcon256Size, expectedFormat: "png", width: 256, height: 256},
	}
	for _, name := range manifest.Screenshots {
		if err := ValidateScreenshotPath(name); err != nil {
			return nil, err
		}
		if _, duplicate := rules[name]; duplicate {
			return nil, fmt.Errorf("mpk: duplicate screenshot %q", name)
		}
		rules[name] = packageAssetRule{
			maxSize:        MaxScreenshotSize,
			expectedFormat: screenshotFormat(name),
		}
	}

	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind package assets: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("mpk: gzip format is invalid: %w", err)
	}
	assets, scanErr := inspectPackageAssetTar(tar.NewReader(gzipReader), rules)
	if scanErr == nil {
		_, scanErr = io.Copy(io.Discard, gzipReader)
	}
	closeErr := gzipReader.Close()
	if scanErr != nil || closeErr != nil {
		return nil, errors.Join(scanErr, closeErr)
	}

	orderedNames := make([]string, 0, len(rules))
	orderedNames = append(orderedNames, icon64Name, icon256Name)
	orderedNames = append(orderedNames, manifest.Screenshots...)
	result := make([]PackageAsset, 0, len(orderedNames))
	for _, name := range orderedNames {
		asset, exists := assets[name]
		if !exists {
			return nil, fmt.Errorf("mpk: required asset %q does not exist", name)
		}
		result = append(result, asset)
	}
	return result, nil
}

func inspectPackageAssetTar(reader *tar.Reader, rules map[string]packageAssetRule) (map[string]PackageAsset, error) {
	assets := make(map[string]PackageAsset, len(rules))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return assets, nil
		}
		if err != nil {
			return nil, fmt.Errorf("mpk: tar format is invalid: %w", err)
		}
		name := path.Clean(header.Name)
		rule, wanted := rules[name]
		if !wanted {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("mpk: asset %q is not a regular file", name)
		}
		if _, duplicate := assets[name]; duplicate {
			return nil, fmt.Errorf("mpk: duplicate asset %q", name)
		}
		if header.Size < 0 || header.Size > rule.maxSize {
			return nil, fmt.Errorf("mpk: asset %q exceeds %d bytes", name, rule.maxSize)
		}
		content, err := io.ReadAll(io.LimitReader(reader, rule.maxSize+1))
		if err != nil {
			return nil, fmt.Errorf("mpk: read asset %q: %w", name, err)
		}
		if int64(len(content)) > rule.maxSize {
			return nil, fmt.Errorf("mpk: asset %q exceeds %d bytes", name, rule.maxSize)
		}
		configuration, format, err := image.DecodeConfig(bytes.NewReader(content))
		if err != nil {
			return nil, fmt.Errorf("mpk: asset %q is not a supported image: %w", name, err)
		}
		if format != rule.expectedFormat {
			return nil, fmt.Errorf("mpk: asset %q format %q does not match %q", name, format, rule.expectedFormat)
		}
		if rule.width > 0 && (configuration.Width != rule.width || configuration.Height != rule.height) {
			return nil, fmt.Errorf(
				"mpk: asset %q dimensions are %dx%d, want %dx%d",
				name,
				configuration.Width,
				configuration.Height,
				rule.width,
				rule.height,
			)
		}
		if strings.HasPrefix(name, "icons/") {
			decoded, _, err := image.Decode(bytes.NewReader(content))
			if err != nil {
				return nil, fmt.Errorf("mpk: asset %q cannot be decoded: %w", name, err)
			}
			switch decoded.(type) {
			case *image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64:
				// Accepted PNG formats with an explicit alpha channel.
			default:
				return nil, fmt.Errorf("mpk: asset %q must be RGBA PNG with alpha", name)
			}
		}
		assets[name] = PackageAsset{
			Name:      name,
			MediaType: "image/" + rule.expectedFormat,
			Content:   content,
		}
	}
}

func screenshotFormat(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg":
		return "jpeg"
	case ".webp":
		return "webp"
	default:
		return "png"
	}
}
