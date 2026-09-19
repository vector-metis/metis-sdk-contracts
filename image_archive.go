package contract

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
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
	Volumes        []string              `json:"volumes,omitempty"`
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
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind image archive: %w", err)
	}
	return InspectImageArchiveStream(reader)
}

// InspectImageArchiveStream 纯流式扫描 Docker save / OCI 镜像归档，不产生任何临时文件。
// 仅提取 manifest.json、config.json 和 index.json 等关键元数据，
// 并从 tar header 中提取镜像层大小，跳过层内容正文。
func InspectImageArchiveStream(reader io.Reader) (*ImageArchiveSummary, error) {
	if reader == nil {
		return nil, fmt.Errorf("mpk: image reader is required")
	}
	files := make(map[string][]byte)
	layerSizes := make(map[string]int64)
	seen := make(map[string]struct{})
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			if err := consumeImageArchivePadding(reader); err != nil {
				return nil, err
			}
			break
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
		layerSizes[cleaned] = header.Size
		if isImageMetadataCandidate(cleaned, header.Size) {
			content := make([]byte, header.Size)
			if _, err := io.ReadFull(tarReader, content); err != nil {
				return nil, fmt.Errorf("mpk: read image metadata %q: %w", cleaned, err)
			}
			files[cleaned] = content
		} else {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return nil, fmt.Errorf("mpk: skip image archive member %q: %w", cleaned, err)
			}
		}
	}
	manifestData, exists := files["manifest.json"]
	if !exists {
		return nil, fmt.Errorf("mpk: image archive must contain one manifest, tag, and config")
	}
	var manifests []dockerSaveManifest
	if err := json.Unmarshal(manifestData, &manifests); err != nil || len(manifests) != 1 || len(manifests[0].RepoTags) != 1 || manifests[0].Config == "" {
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
	configData, exists := files[configName]
	if !exists {
		return nil, fmt.Errorf("mpk: image archive config %q is missing", configName)
	}
	dockerArchitecture, err := imageConfigArchitecture(configData)
	if err != nil {
		return nil, fmt.Errorf("mpk: image archive architecture is invalid")
	}
	if indexData, exists := files["index.json"]; exists {
		if err := validateOCIImageMetadataMemory(files, indexData, dockerReference, dockerArchitecture); err != nil {
			return nil, err
		}
	}
	return summarizeImageArchiveMemory(files, layerSizes, dockerReference, manifests[0].RepoTags[0], dockerArchitecture, manifests[0])
}

func consumeImageArchivePadding(reader io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		read, err := reader.Read(buffer)
		for _, value := range buffer[:read] {
			if value != 0 {
				return fmt.Errorf("mpk: image archive has non-zero trailing data")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mpk: read image archive padding: %w", err)
		}
	}
}

func isImageMetadataCandidate(name string, size int64) bool {
	if size < 0 || size > maxImageMetadataFileSize {
		return false
	}
	if name == "manifest.json" || name == "index.json" || strings.HasSuffix(name, ".json") {
		return true
	}
	base := path.Base(name)
	if strings.HasPrefix(base, "sha256:") && len(base) == len("sha256:")+sha256.Size*2 {
		_, err := hex.DecodeString(strings.TrimPrefix(base, "sha256:"))
		return err == nil
	}
	if strings.HasPrefix(name, "blobs/sha256/") && !strings.HasSuffix(name, ".tar") && !strings.HasSuffix(name, ".tar.gz") {
		return true
	}
	return false
}

func summarizeImageArchiveMemory(
	files map[string][]byte,
	layerSizes map[string]int64,
	reference name.Tag,
	repoTag, architecture string,
	manifest dockerSaveManifest,
) (*ImageArchiveSummary, error) {
	configName, err := safeImageArchivePath(manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("mpk: image archive config path is unsafe")
	}
	rawConfig, exists := files[configName]
	if !exists {
		return nil, fmt.Errorf("mpk: image archive config %q is missing", configName)
	}
	var config v1.ConfigFile
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return nil, fmt.Errorf("mpk: read image config: %w", err)
	}
	if config.Architecture != architecture {
		return nil, fmt.Errorf("mpk: loaded image architecture %q does not match docker config architecture %q", config.Architecture, architecture)
	}
	expectedLayers := len(manifest.Layers)
	if len(config.RootFS.DiffIDs) != expectedLayers {
		return nil, fmt.Errorf("mpk: docker manifest has %d layers but image config has %d diff IDs", expectedLayers, len(config.RootFS.DiffIDs))
	}
	configHashBytes := sha256.Sum256(rawConfig)
	configDigest := "sha256:" + hex.EncodeToString(configHashBytes[:])
	configHash, err := v1.NewHash(configDigest)
	if err != nil {
		return nil, fmt.Errorf("mpk: calculate image config digest: %w", err)
	}
	descriptors := make([]v1.Descriptor, 0, expectedLayers)
	layers := make([]ImageLayerSummary, 0, expectedLayers)
	compressedSize := int64(len(rawConfig))
	for index, layerPath := range manifest.Layers {
		cleaned, err := safeImageArchivePath(layerPath)
		if err != nil {
			return nil, fmt.Errorf("mpk: unsafe layer path: %w", err)
		}
		size, exists := layerSizes[cleaned]
		if !exists {
			return nil, fmt.Errorf("mpk: layer %q is missing from image archive", cleaned)
		}
		diffID := config.RootFS.DiffIDs[index]
		mediaType := "application/vnd.docker.image.rootfs.diff.tar"
		layers = append(layers, ImageLayerSummary{
			Digest:         diffID.String(),
			CompressedSize: size,
			MediaType:      mediaType,
		})
		compressedSize += size
		descriptors = append(descriptors, v1.Descriptor{
			MediaType: types.DockerUncompressedLayer,
			Size:      size,
			Digest:    diffID,
		})
	}
	dockerManifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.DockerManifestSchema2,
		Config: v1.Descriptor{
			MediaType: types.DockerConfigJSON,
			Size:      int64(len(rawConfig)),
			Digest:    configHash,
		},
		Layers: descriptors,
	}
	manifestBytes, err := json.Marshal(dockerManifest)
	if err != nil {
		return nil, fmt.Errorf("mpk: marshal image manifest: %w", err)
	}
	manifestHash := sha256.Sum256(manifestBytes)
	imageDigest := "sha256:" + hex.EncodeToString(manifestHash[:])

	exposedPorts := make([]string, 0, len(config.Config.ExposedPorts))
	for port := range config.Config.ExposedPorts {
		exposedPorts = append(exposedPorts, port)
	}
	sort.Strings(exposedPorts)
	volumes := make([]string, 0, len(config.Config.Volumes))
	for volume := range config.Config.Volumes {
		if volume == "" || !strings.HasPrefix(volume, "/") || path.Clean(volume) != volume || volume == "/" {
			return nil, fmt.Errorf("mpk: image config volume target %q is invalid", volume)
		}
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)
	history := make([]ImageHistorySummary, 0, len(config.History))
	for _, item := range config.History {
		history = append(history, ImageHistorySummary{
			Created: imageTime(item.Created), CreatedBy: item.CreatedBy, EmptyLayer: item.EmptyLayer,
		})
	}
	return &ImageArchiveSummary{
		RepoTag: repoTag, OS: config.OS, Architecture: architecture,
		ImageDigest: imageDigest, ConfigDigest: configDigest, ConfigSize: int64(len(rawConfig)),
		CompressedSize: compressedSize, MediaType: string(types.DockerManifestSchema2), Created: imageTime(config.Created),
		Entrypoint: config.Config.Entrypoint, Cmd: config.Config.Cmd, WorkingDir: config.Config.WorkingDir,
		Environment: config.Config.Env, ExposedPorts: exposedPorts, Volumes: volumes, Layers: layers, History: history,
	}, nil
}

func imageTime(value v1.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

func validateOCIImageMetadataMemory(files map[string][]byte, indexData []byte, dockerReference name.Tag, dockerArchitecture string) error {
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

	manifestData, err := resolveOCIImageManifestMemory(files, descriptor, dockerArchitecture)
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
	configData, exists := files[configName]
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

func resolveOCIImageManifestMemory(files map[string][]byte, descriptor ociDescriptor, dockerArchitecture string) ([]byte, error) {
	switch descriptor.MediaType {
	case ociImageManifestMediaType:
		return readVerifiedOCIBlobMemory(files, descriptor, "oci manifest")
	case ociImageIndexMediaType:
		indexData, err := readVerifiedOCIBlobMemory(files, descriptor, "oci nested index")
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
		return readVerifiedOCIBlobMemory(files, matches[0], "oci manifest")
	default:
		return nil, fmt.Errorf("mpk: oci index descriptor media type %q is unsupported", descriptor.MediaType)
	}
}

func readVerifiedOCIBlobMemory(files map[string][]byte, descriptor ociDescriptor, description string) ([]byte, error) {
	blobName, err := ociBlobPath(descriptor.Digest)
	if err != nil {
		return nil, fmt.Errorf("mpk: %s digest is invalid: %w", description, err)
	}
	blobData, exists := files[blobName]
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

func safeImageArchivePath(value string) (string, error) {
	cleaned := path.Clean(value)
	if path.IsAbs(value) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe path")
	}
	return cleaned, nil
}
