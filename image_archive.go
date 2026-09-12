package contract

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const maxImageMetadataFileSize = 4 << 20

const (
	ociRefNameAnnotation      = "org.opencontainers.image.ref.name"
	containerdNameAnnotation  = "io.containerd.image.name"
	ociImageIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	ociImageManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
)

// ImageLayerSummary 是 Docker save 归档中一个镜像层的可审查事实。
type ImageLayerSummary struct {
	Digest         string `json:"digest"`
	CompressedSize int64  `json:"compressedSize"`
	MediaType      string `json:"mediaType"`
}

// ImageHistorySummary 保留 config.history 中不含层正文的构建历史。
type ImageHistorySummary struct {
	Created    string `json:"created,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	EmptyLayer bool   `json:"emptyLayer,omitempty"`
}

// ImageArchiveSummary 是审核页和安装门禁使用的镜像结构化事实。
type ImageArchiveSummary struct {
	RepoTag        string                `json:"repoTag"`
	OS             string                `json:"os,omitempty"`
	Architecture   string                `json:"architecture"`
	ImageDigest    string                `json:"imageDigest,omitempty"`
	ConfigDigest   string                `json:"configDigest,omitempty"`
	ConfigSize     int64                 `json:"configSize,omitempty"`
	CompressedSize int64                 `json:"compressedSize,omitempty"`
	MediaType      string                `json:"mediaType,omitempty"`
	Created        string                `json:"created,omitempty"`
	Entrypoint     []string              `json:"entrypoint,omitempty"`
	Cmd            []string              `json:"cmd,omitempty"`
	WorkingDir     string                `json:"workingDir,omitempty"`
	Environment    []string              `json:"environment,omitempty"`
	ExposedPorts   []string              `json:"exposedPorts,omitempty"`
	Layers         []ImageLayerSummary   `json:"layers,omitempty"`
	History        []ImageHistorySummary `json:"history,omitempty"`
}

type dockerSaveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
	Platform    struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

// InspectImageArchive 顺序扫描 Docker save tar，只读取身份校验需要的小型
// manifest、index 和 config。OCI 元数据存在时必须与 Docker 元数据一致。
func InspectImageArchive(reader io.ReadSeeker) (*ImageArchiveSummary, error) {
	rootFiles, err := readImageArchiveFiles(reader, "manifest.json", "index.json")
	if err != nil {
		return nil, err
	}
	var manifests []dockerSaveManifest
	if err := json.Unmarshal(rootFiles["manifest.json"], &manifests); err != nil || len(manifests) != 1 || len(manifests[0].RepoTags) != 1 || manifests[0].Config == "" {
		return nil, fmt.Errorf("mpk: image archive must contain one manifest, tag, and config")
	}
	dockerReference, err := parseExplicitImageTag(manifests[0].RepoTags[0])
	if err != nil {
		return nil, fmt.Errorf("mpk: docker manifest image reference is invalid: %w", err)
	}

	configName, err := safeImageArchivePath(manifests[0].Config)
	if err != nil {
		return nil, fmt.Errorf("mpk: image archive config path is unsafe")
	}
	configFiles, err := readImageArchiveFiles(reader, configName)
	if err != nil {
		return nil, err
	}
	configData, exists := configFiles[configName]
	if !exists {
		return nil, fmt.Errorf("mpk: image archive config %q is missing", configName)
	}
	dockerArchitecture, err := imageConfigArchitecture(configData)
	if err != nil {
		return nil, fmt.Errorf("mpk: image archive architecture is invalid")
	}

	if indexData, exists := rootFiles["index.json"]; exists {
		if err := validateOCIImageMetadata(reader, indexData, dockerReference, dockerArchitecture); err != nil {
			return nil, err
		}
	}
	return summarizeImageArchive(reader, dockerReference, manifests[0].RepoTags[0], dockerArchitecture, len(manifests[0].Layers))
}

func summarizeImageArchive(reader io.ReadSeeker, reference name.Tag, repoTag, architecture string, expectedLayers int) (*ImageArchiveSummary, error) {
	opener, cleanup, err := imageArchiveOpener(reader)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	image, err := tarball.Image(opener, &reference)
	if err != nil {
		return nil, fmt.Errorf("mpk: load image archive: %w", err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("mpk: read image config: %w", err)
	}
	if config.Architecture != architecture {
		return nil, fmt.Errorf("mpk: loaded image architecture %q does not match docker config architecture %q", config.Architecture, architecture)
	}
	rawConfig, err := image.RawConfigFile()
	if err != nil {
		return nil, fmt.Errorf("mpk: read raw image config: %w", err)
	}
	imageDigest, err := image.Digest()
	if err != nil {
		return nil, fmt.Errorf("mpk: calculate image digest: %w", err)
	}
	configDigest, err := image.ConfigName()
	if err != nil {
		return nil, fmt.Errorf("mpk: calculate image config digest: %w", err)
	}
	mediaType, err := image.MediaType()
	if err != nil {
		return nil, fmt.Errorf("mpk: read image media type: %w", err)
	}
	imageLayers, err := image.Layers()
	if err != nil {
		return nil, fmt.Errorf("mpk: read image layers: %w", err)
	}
	if len(imageLayers) != expectedLayers {
		return nil, fmt.Errorf("mpk: docker manifest has %d layers but image config has %d diff IDs", expectedLayers, len(imageLayers))
	}
	layers := make([]ImageLayerSummary, 0, len(imageLayers))
	compressedSize := int64(len(rawConfig))
	for index, layer := range imageLayers {
		digest, digestErr := layer.Digest()
		size, sizeErr := layer.Size()
		layerMediaType, mediaTypeErr := layer.MediaType()
		if err := errors.Join(digestErr, sizeErr, mediaTypeErr); err != nil {
			return nil, fmt.Errorf("mpk: inspect image layer %d: %w", index, err)
		}
		layers = append(layers, ImageLayerSummary{
			Digest: digest.String(), CompressedSize: size, MediaType: string(layerMediaType),
		})
		compressedSize += size
	}
	exposedPorts := make([]string, 0, len(config.Config.ExposedPorts))
	for port := range config.Config.ExposedPorts {
		exposedPorts = append(exposedPorts, port)
	}
	sort.Strings(exposedPorts)
	history := make([]ImageHistorySummary, 0, len(config.History))
	for _, item := range config.History {
		history = append(history, ImageHistorySummary{
			Created: imageTime(item.Created), CreatedBy: item.CreatedBy, EmptyLayer: item.EmptyLayer,
		})
	}
	return &ImageArchiveSummary{
		RepoTag: repoTag, OS: config.OS, Architecture: architecture,
		ImageDigest: imageDigest.String(), ConfigDigest: configDigest.String(), ConfigSize: int64(len(rawConfig)),
		CompressedSize: compressedSize, MediaType: string(mediaType), Created: imageTime(config.Created),
		Entrypoint: config.Config.Entrypoint, Cmd: config.Config.Cmd, WorkingDir: config.Config.WorkingDir,
		Environment: config.Config.Env, ExposedPorts: exposedPorts, Layers: layers, History: history,
	}, nil
}

func imageTime(value v1.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// imageArchiveOpener 为 go-containerregistry 的每次扫描提供独立 reader。
// 文件和 bytes.Reader 直接复用 ReaderAt；不支持 ReaderAt 的少见 seeker 只落一次临时文件。
func imageArchiveOpener(reader io.ReadSeeker) (tarball.Opener, func(), error) {
	if readerAt, ok := reader.(io.ReaderAt); ok {
		size, err := reader.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, nil, fmt.Errorf("mpk: size image archive: %w", err)
		}
		return func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(readerAt, 0, size)), nil
		}, func() {}, nil
	}
	temporary, err := os.CreateTemp("", "metis-image-reader-*.tar")
	if err != nil {
		return nil, nil, fmt.Errorf("mpk: create image reader file: %w", err)
	}
	name := temporary.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		_ = temporary.Close()
		cleanup()
		return nil, nil, fmt.Errorf("mpk: rewind image archive: %w", err)
	}
	if _, err := io.Copy(temporary, reader); err != nil {
		_ = temporary.Close()
		cleanup()
		return nil, nil, fmt.Errorf("mpk: copy image archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("mpk: close image reader file: %w", err)
	}
	return func() (io.ReadCloser, error) { return os.Open(name) }, cleanup, nil
}

func validateOCIImageMetadata(reader io.ReadSeeker, indexData []byte, dockerReference name.Tag, dockerArchitecture string) error {
	var index ociIndex
	if err := json.Unmarshal(indexData, &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return fmt.Errorf("mpk: oci index must contain one image manifest")
	}
	descriptor := index.Manifests[0]
	if err := validateOCIReference(descriptor.Annotations, dockerReference); err != nil {
		return err
	}
	if descriptor.Platform.Architecture != "" && descriptor.Platform.Architecture != dockerArchitecture {
		return fmt.Errorf("mpk: oci index architecture %q does not match docker config architecture %q", descriptor.Platform.Architecture, dockerArchitecture)
	}

	manifestData, err := resolveOCIImageManifest(reader, descriptor, dockerArchitecture)
	if err != nil {
		return err
	}
	var manifest struct {
		SchemaVersion int           `json:"schemaVersion"`
		Config        ociDescriptor `json:"config"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.Config.Digest == "" {
		return fmt.Errorf("mpk: oci image manifest is invalid")
	}

	configName, err := ociBlobPath(manifest.Config.Digest)
	if err != nil {
		return fmt.Errorf("mpk: oci config digest is invalid: %w", err)
	}
	configFiles, err := readImageArchiveFiles(reader, configName)
	if err != nil {
		return err
	}
	configData, exists := configFiles[configName]
	if !exists {
		return fmt.Errorf("mpk: oci config blob %q is missing", configName)
	}
	if err := verifyOCIDescriptor(manifest.Config, configData); err != nil {
		return fmt.Errorf("mpk: oci config blob: %w", err)
	}
	ociArchitecture, err := imageConfigArchitecture(configData)
	if err != nil {
		return fmt.Errorf("mpk: oci config architecture is invalid")
	}
	if ociArchitecture != dockerArchitecture {
		return fmt.Errorf("mpk: oci config architecture %q does not match docker config architecture %q", ociArchitecture, dockerArchitecture)
	}
	return nil
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// resolveOCIImageManifest 兼容 OCI layout 的两种受控结构：顶层 descriptor
// 可以直接指向 image manifest，也可以像 Docker 29 一样再指向一层多架构 index。
// 子 index 只按 Docker config 已声明的 linux 架构唯一选取，不进行无界递归。
func resolveOCIImageManifest(reader io.ReadSeeker, descriptor ociDescriptor, dockerArchitecture string) ([]byte, error) {
	switch descriptor.MediaType {
	case ociImageManifestMediaType:
		return readVerifiedOCIBlob(reader, descriptor, "oci manifest")
	case ociImageIndexMediaType:
		indexData, err := readVerifiedOCIBlob(reader, descriptor, "oci nested index")
		if err != nil {
			return nil, err
		}
		var index ociIndex
		if err := json.Unmarshal(indexData, &index); err != nil || index.SchemaVersion != 2 || index.MediaType != ociImageIndexMediaType {
			return nil, fmt.Errorf("mpk: nested oci index is invalid")
		}
		var matches []ociDescriptor
		for _, candidate := range index.Manifests {
			if candidate.MediaType == ociImageManifestMediaType &&
				candidate.Platform.OS == "linux" && candidate.Platform.Architecture == dockerArchitecture {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("mpk: nested oci index must contain exactly one linux/%s image manifest", dockerArchitecture)
		}
		return readVerifiedOCIBlob(reader, matches[0], "oci manifest")
	default:
		return nil, fmt.Errorf("mpk: oci index descriptor media type %q is unsupported", descriptor.MediaType)
	}
}

func readVerifiedOCIBlob(reader io.ReadSeeker, descriptor ociDescriptor, description string) ([]byte, error) {
	blobName, err := ociBlobPath(descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("mpk: %s digest is invalid: %w", description, err)
	}
	blobFiles, err := readImageArchiveFiles(reader, blobName)
	if err != nil {
		return nil, err
	}
	blobData, exists := blobFiles[blobName]
	if !exists {
		return nil, fmt.Errorf("mpk: %s blob %q is missing", description, blobName)
	}
	if err := verifyOCIDescriptor(descriptor, blobData); err != nil {
		return nil, fmt.Errorf("mpk: %s blob: %w", description, err)
	}
	return blobData, nil
}

func validateOCIReference(annotations map[string]string, dockerReference name.Tag) error {
	if fullName := annotations[containerdNameAnnotation]; fullName != "" {
		ociReference, err := parseExplicitImageTag(fullName)
		if err != nil {
			return fmt.Errorf("mpk: oci index reference is invalid: %w", err)
		}
		if ociReference.Name() != dockerReference.Name() {
			return fmt.Errorf("mpk: oci index reference %q does not match docker manifest reference %q", fullName, dockerReference.Name())
		}
		return nil
	}
	refName := annotations[ociRefNameAnnotation]
	if refName == "" {
		return fmt.Errorf("mpk: oci index image reference is missing")
	}
	// OCI layout 常只在 ref.name 保存 tag；完整引用则必须与 Docker 名称一致。
	if refName == dockerReference.Identifier() {
		return nil
	}
	ociReference, err := parseExplicitImageTag(refName)
	if err != nil {
		return fmt.Errorf("mpk: oci index reference is invalid: %w", err)
	}
	if ociReference.Name() != dockerReference.Name() {
		return fmt.Errorf("mpk: oci index reference %q does not match docker manifest reference %q", refName, dockerReference.Name())
	}
	return nil
}

func imageConfigArchitecture(data []byte) (string, error) {
	var config struct {
		Architecture string `json:"architecture"`
	}
	if err := json.Unmarshal(data, &config); err != nil || (config.Architecture != ArchAMD64 && config.Architecture != ArchARM64) {
		return "", fmt.Errorf("unsupported image architecture")
	}
	return config.Architecture, nil
}

func parseExplicitImageTag(value string) (name.Tag, error) {
	lastSlash := strings.LastIndexByte(value, '/')
	lastColon := strings.LastIndexByte(value, ':')
	if value == "" || strings.ContainsRune(value, '@') || lastColon <= lastSlash || lastColon == len(value)-1 {
		return name.Tag{}, fmt.Errorf("image reference %q must contain an explicit tag", value)
	}
	// MPK 源引用允许省略 Registry；WeakValidation 仍由库解析 repository/tag，
	// 但会按 Docker 规则补全默认 Registry，便于与 OCI 完整名称比较。
	reference, err := name.NewTag(value, name.WeakValidation)
	if err != nil {
		return name.Tag{}, fmt.Errorf("parse image reference %q: %w", value, err)
	}
	return reference, nil
}

func ociBlobPath(value string) (string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != sha256.Size*2 {
		return "", fmt.Errorf("unsupported digest %q", value)
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", fmt.Errorf("decode digest %q: %w", value, err)
	}
	return "blobs/sha256/" + parts[1], nil
}

func verifyOCIDescriptor(descriptor ociDescriptor, data []byte) error {
	if descriptor.Size != int64(len(data)) {
		return fmt.Errorf("declared size %d does not match content size %d", descriptor.Size, len(data))
	}
	digest := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if descriptor.Digest != actual {
		return fmt.Errorf("declared digest %q does not match content digest %q", descriptor.Digest, actual)
	}
	return nil
}

// readImageArchiveFiles 每次完整扫描所有 header，从而不依赖 tar 成员顺序，
// 并在读取指定元数据的同时拒绝路径逃逸、链接和设备节点。
func readImageArchiveFiles(reader io.ReadSeeker, names ...string) (map[string][]byte, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind image archive: %w", err)
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	result := make(map[string][]byte, len(names))
	seen := make(map[string]struct{})
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("mpk: image archive is invalid: %w", err)
		}
		cleaned, err := safeImageArchivePath(header.Name)
		if err != nil {
			return nil, fmt.Errorf("mpk: image archive contains unsafe path %q", header.Name)
		}
		if _, duplicate := seen[cleaned]; duplicate {
			return nil, fmt.Errorf("mpk: image archive contains duplicate file %q", cleaned)
		}
		seen[cleaned] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, 0:
		default:
			return nil, fmt.Errorf("mpk: image archive contains unsupported node %q", header.Name)
		}
		if _, exists := wanted[cleaned]; !exists {
			continue
		}
		if _, duplicate := result[cleaned]; duplicate {
			return nil, fmt.Errorf("mpk: image archive contains duplicate file %q", cleaned)
		}
		if header.Size < 0 || header.Size > maxImageMetadataFileSize {
			return nil, fmt.Errorf("mpk: image metadata %q exceeds %d bytes", cleaned, maxImageMetadataFileSize)
		}
		content := make([]byte, header.Size)
		if _, err := io.ReadFull(tarReader, content); err != nil {
			return nil, fmt.Errorf("mpk: read image metadata %q: %w", cleaned, err)
		}
		result[cleaned] = content
	}
}

func safeImageArchivePath(value string) (string, error) {
	cleaned := path.Clean(value)
	if path.IsAbs(value) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe path")
	}
	return cleaned, nil
}
