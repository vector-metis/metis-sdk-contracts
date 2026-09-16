package contract_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestInspectImageArchiveReadsManifestReferencedConfig(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	layer := strings.Repeat("x", 1024)
	layerDigest := sha256.Sum256([]byte(layer))
	writeTarEntry(t, writer, "layer.tar", layer)
	writeTarEntry(t, writer, "sha256-config.json", fmt.Sprintf(`{"architecture":"amd64","rootfs":{"type":"layers","diff_ids":["sha256:%s"]},"config":{"Env":["APP_MODE=production"],"ExposedPorts":{"8443/tcp":{},"8080/tcp":{}}}}`, hex.EncodeToString(layerDigest[:])))
	writeTarEntry(t, writer, "manifest.json", `[{"Config":"sha256-config.json","RepoTags":["demo-a7x2m/web:1.0.0"],"Layers":["layer.tar"]}]`)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	summary, err := contract.InspectImageArchive(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if summary.RepoTag != "demo-a7x2m/web:1.0.0" || summary.Architecture != contract.ArchAMD64 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.OS != "" || summary.ConfigSize == 0 || summary.CompressedSize <= summary.ConfigSize || len(summary.Layers) != 1 || summary.Layers[0].CompressedSize <= 0 {
		t.Fatalf("image facts = %#v", summary)
	}
	if len(summary.Environment) != 1 || summary.Environment[0] != "APP_MODE=production" ||
		!slices.Equal(summary.ExposedPorts, []string{"8080/tcp", "8443/tcp"}) {
		t.Fatalf("image config facts = %#v", summary)
	}
}

func TestInspectImageArchiveRejectsMissingConfig(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	writeTarEntry(t, writer, "manifest.json", `[{"Config":"missing.json","RepoTags":["demo-a7x2m/web:1.0.0"]}]`)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := contract.InspectImageArchive(bytes.NewReader(archive.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "config \"missing.json\" is missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectImageArchiveRejectsDuplicateLayerMember(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	writeTarEntry(t, writer, "layer.tar", "first")
	writeTarEntry(t, writer, "layer.tar", "second")
	writeTarEntry(t, writer, "config.json", `{"architecture":"amd64","os":"linux"}`)
	writeTarEntry(t, writer, "manifest.json", `[{"Config":"config.json","RepoTags":["demo-a7x2m/web:1.0.0"],"Layers":["layer.tar"]}]`)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := contract.InspectImageArchive(bytes.NewReader(archive.Bytes()))
	if err == nil || !strings.Contains(err.Error(), `duplicate file "layer.tar"`) {
		t.Fatalf("InspectImageArchive() error = %v, want duplicate layer rejection", err)
	}
}

func TestInspectImageArchiveAcceptsConsistentOCIIndex(t *testing.T) {
	archive := dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64)

	summary, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if summary.RepoTag != "demo-a7x2m/web:1.0.0" || summary.Architecture != contract.ArchAMD64 {
		t.Fatalf("summary = %#v", summary)
	}
}

// TestInspectImageArchiveAcceptsDockerNestedOCIIndex 固化 Docker 29 的真实保存格式：
// 顶层 index 通过引用 annotation 标识本地 tag，再指向包含多架构镜像和证明材料的子 index。
func TestInspectImageArchiveAcceptsDockerNestedOCIIndex(t *testing.T) {
	archive := dockerNestedOCIArchive(t, "demo-a7x2m/web:1.0.0", contract.ArchAMD64, 1)

	summary, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if summary.RepoTag != "demo-a7x2m/web:1.0.0" || summary.Architecture != contract.ArchAMD64 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestInspectImageArchiveRejectsAmbiguousNestedOCIIndex(t *testing.T) {
	archive := dockerNestedOCIArchive(t, "demo-a7x2m/web:1.0.0", contract.ArchAMD64, 2)

	_, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "exactly one linux/amd64 image manifest") {
		t.Fatalf("error = %v, want ambiguous nested index error", err)
	}
}

func TestInspectImageArchiveRejectsNestedOCIIndexMissingArchitecture(t *testing.T) {
	archive := dockerNestedOCIArchive(t, "demo-a7x2m/web:1.0.0", contract.ArchAMD64, 0)

	_, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "exactly one linux/amd64 image manifest") {
		t.Fatalf("error = %v, want missing nested index architecture error", err)
	}
}

// TestInspectImageArchiveRejectsOCIIndexTagMismatch 固化真实故障：Docker
// manifest 与 OCI index 声明不同应用标识时，包必须在导入 Registry 前失败。
func TestInspectImageArchiveRejectsOCIIndexTagMismatch(t *testing.T) {
	archive := dockerOCIArchive(t, "qa-market-1-wpsjl/web:1.0.0", "docker.io/real-ops-o220u/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64)

	_, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "oci index reference") {
		t.Fatalf("error = %v, want OCI index reference mismatch", err)
	}
}

func TestInspectImageArchiveRejectsOCIArchitectureMismatch(t *testing.T) {
	archive := dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchAMD64, contract.ArchARM64)

	_, err := contract.InspectImageArchive(bytes.NewReader(archive))
	if err == nil || !strings.Contains(err.Error(), "oci index architecture") {
		t.Fatalf("error = %v, want OCI index architecture mismatch", err)
	}
}

func TestValidateMPKRejectsOCIIndexIdentityMismatch(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["images/amd64/app.tar"] = dockerOCIArchive(t, "qa-market-1-wpsjl/web:1.0.0", "docker.io/real-ops-o220u/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64)
		files["manifest.yaml"] = []byte(`schema_version: 1
id: qa-market-1-wpsjl
version: 1.0.0
display_name: QA Market
arch: [amd64]
dependencies: []
type: web
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: qa-market-1-wpsjl/web:1.0.0
    restart: unless-stopped
`)
	})

	_, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "qa-market-1-wpsjl"})
	if err == nil || !strings.Contains(err.Error(), "oci index reference") {
		t.Fatalf("error = %v, want OCI index reference mismatch", err)
	}
}

func TestValidateMPKAcceptsOCIImagesForAllDeclaredArchitectures(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
arch: [amd64, arm64]
dependencies: []
type: web
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`)
		files["compose.arm64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
`)
		files["images/amd64/app.tar"] = dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64)
		files["images/arm64/app.tar"] = dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchARM64, contract.ArchARM64)
	})

	summary, err := validate(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Manifest.Architectures) != 2 {
		t.Fatalf("ValidateMPK() architectures = %v, want amd64 and arm64", summary.Manifest.Architectures)
	}
}

// dockerOCIArchive 构造同时携带 Docker manifest 与 OCI index 的小型真实归档结构。
// blob 故意写在 index 前面，确保校验器不依赖 tar entry 排序。
func dockerOCIArchive(t *testing.T, dockerTag, ociTag, dockerArchitecture, ociArchitecture string) []byte {
	t.Helper()
	config := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux"}`, ociArchitecture))
	configDigest := sha256.Sum256(config)
	configHex := hex.EncodeToString(configDigest[:])
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%d},"layers":[]}`, configHex, len(config)))
	manifestDigest := sha256.Sum256(manifest)
	manifestHex := hex.EncodeToString(manifestDigest[:])
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%d,"platform":{"architecture":%q,"os":"linux"},"annotations":{"org.opencontainers.image.ref.name":%q}}]}`, manifestHex, len(manifest), ociArchitecture, ociTag))
	dockerConfig := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux"}`, dockerArchitecture))
	dockerManifest := []byte(fmt.Sprintf(`[{"Config":%q,"RepoTags":[%q],"Layers":[]}]`, configHex+".json", dockerTag))

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	writeTarBytes(t, writer, "blobs/sha256/"+configHex, config)
	writeTarBytes(t, writer, "blobs/sha256/"+manifestHex, manifest)
	writeTarBytes(t, writer, configHex+".json", dockerConfig)
	writeTarBytes(t, writer, "manifest.json", dockerManifest)
	writeTarBytes(t, writer, "index.json", index)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// dockerNestedOCIArchive 构造 Docker 29 保存多架构镜像时生成的两层 OCI index。
// 子 index 同时包含目标镜像、其他架构镜像和 unknown/unknown attestation。
func dockerNestedOCIArchive(t *testing.T, reference, architecture string, targetCopies int) []byte {
	t.Helper()
	config := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux"}`, architecture))
	configDigest := sha256.Sum256(config)
	configHex := hex.EncodeToString(configDigest[:])
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%d},"layers":[]}`, configHex, len(config)))
	manifestDigest := sha256.Sum256(manifest)
	manifestHex := hex.EncodeToString(manifestDigest[:])

	otherArchitecture := contract.ArchARM64
	if architecture == contract.ArchARM64 {
		otherArchitecture = contract.ArchAMD64
	}
	targetDescriptor := fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%d,"platform":{"architecture":%q,"os":"linux"}}`, manifestHex, len(manifest), architecture)
	descriptors := []string{
		fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1,"platform":{"architecture":%q,"os":"linux"}}`, otherArchitecture),
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":1,"platform":{"architecture":"unknown","os":"unknown"}}`,
	}
	for range targetCopies {
		descriptors = append(descriptors, targetDescriptor)
	}
	nestedIndex := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}`, strings.Join(descriptors, ",")))
	nestedDigest := sha256.Sum256(nestedIndex)
	nestedHex := hex.EncodeToString(nestedDigest[:])
	topIndex := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:%s","size":%d,"annotations":{"io.containerd.image.name":%q,"org.opencontainers.image.ref.name":"1.0.0"}}]}`, nestedHex, len(nestedIndex), "docker.io/"+reference))
	dockerManifest := []byte(fmt.Sprintf(`[{"Config":%q,"RepoTags":[%q],"Layers":[]}]`, "blobs/sha256/"+configHex, reference))

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	writeTarBytes(t, writer, "blobs/sha256/"+configHex, config)
	writeTarBytes(t, writer, "blobs/sha256/"+manifestHex, manifest)
	writeTarBytes(t, writer, "blobs/sha256/"+nestedHex, nestedIndex)
	writeTarBytes(t, writer, "manifest.json", dockerManifest)
	writeTarBytes(t, writer, "index.json", topIndex)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func writeTarEntry(t *testing.T, writer *tar.Writer, name, content string) {
	t.Helper()
	writeTarBytes(t, writer, name, []byte(content))
}

func writeTarBytes(t *testing.T, writer *tar.Writer, name string, data []byte) {
	t.Helper()
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
}
