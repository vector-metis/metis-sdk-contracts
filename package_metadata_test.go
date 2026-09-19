package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestInspectPackageIdentityReadsManifestWithoutExpectedIdentity(t *testing.T) {
	t.Parallel()

	packageData := metadataPackage(t, map[string]string{
		"manifest.yaml": `schema_version: 1
id: offline-app-a7x2m
version: 2.3.0
display_name: Offline
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`,
	})
	appID, version, err := contract.InspectPackageIdentity(bytes.NewReader(packageData))
	if err != nil {
		t.Fatal(err)
	}
	if appID != "offline-app-a7x2m" || version != "2.3.0" {
		t.Fatalf("InspectPackageIdentity() = %q/%q", appID, version)
	}
}

// TestInspectPackageMetadataReturnsOnlyBoundedContractFiles 锁定准备阶段只返回
// 安装需要的小型契约文件，不把镜像归档正文暴露给调用方。
func TestInspectPackageMetadataReturnsOnlyBoundedContractFiles(t *testing.T) {
	t.Parallel()

	compose := `services:
  web:
    image: demo-a7x2m/web:1.0.0
`
	packageData := metadataPackage(t, map[string]string{
		"manifest.yaml": `schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`,
		"about.md":             "# Demo\n",
		"compose.amd64.yaml":   compose,
		"images/amd64/web.tar": strings.Repeat("x", 8<<20),
	})

	metadata, err := contract.InspectPackageMetadata(bytes.NewReader(packageData), "demo-a7x2m", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Manifest.ID != "demo-a7x2m" || metadata.Manifest.Version != "1.0.0" || metadata.AboutMD != "# Demo\n" {
		t.Fatalf("metadata = %+v", metadata)
	}
	if len(metadata.Settings) != 0 || metadata.Compose["amd64"] != compose || metadata.FileCount != 4 {
		t.Fatalf("metadata contract files = %+v", metadata)
	}
}

func TestInspectPackageMetadataAcceptsOverlayMountFiles(t *testing.T) {
	t.Parallel()

	packageData := metadataPackage(t, map[string]string{
		"manifest.yaml": `schema_version: 1
id: overlay-app-a7x2m
version: 1.0.0
display_name: Overlay
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    mounts:
      - {source: overlay, subpath: conf/nginx.conf, target: /etc/nginx/nginx.conf, read_only: true}
`,
		"compose.amd64.yaml":      "services:\n  web:\n    image: overlay-app-a7x2m/web:1.0.0\n",
		"overlay/conf/nginx.conf": "worker_processes 1;\n",
	})

	metadata, err := contract.InspectPackageMetadata(bytes.NewReader(packageData), "overlay-app-a7x2m", "1.0.0")
	if err != nil {
		t.Fatalf("InspectPackageMetadata() error = %v", err)
	}
	if metadata.Manifest.ID != "overlay-app-a7x2m" || metadata.Compose["amd64"] == "" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestInspectPackageMetadataRejectsMissingOverlayFile(t *testing.T) {
	t.Parallel()
	packageData := metadataPackage(t, map[string]string{
		"manifest.yaml": `schema_version: 1
id: overlay-app-a7x2m
version: 1.0.0
display_name: Overlay
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    mounts:
      - {source: overlay, subpath: missing.conf, target: /etc/missing.conf, read_only: true}
`,
		"compose.amd64.yaml": "services:\n  web:\n    image: overlay-app-a7x2m/web:1.0.0\n",
	})
	_, err := contract.InspectPackageMetadata(bytes.NewReader(packageData), "overlay-app-a7x2m", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "overlay mount") {
		t.Fatalf("InspectPackageMetadata() error = %v, want missing overlay rejection", err)
	}
}

func TestInspectPackageMetadataAcceptsOverlayDirectoryMount(t *testing.T) {
	t.Parallel()
	entries := []metadataEntry{
		{name: "manifest.yaml", content: `schema_version: 1
id: overlay-app-a7x2m
version: 1.0.0
display_name: Overlay
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    mounts:
      - {source: overlay, subpath: conf, target: /etc/app, read_only: true}
`},
		{name: "compose.amd64.yaml", content: "services:\n  web:\n    image: overlay-app-a7x2m/web:1.0.0\n"},
		{name: "overlay/conf", typeflag: tar.TypeDir},
		{name: "overlay/conf/app.conf", content: "enabled=true\n"},
	}
	if _, err := contract.InspectPackageMetadata(bytes.NewReader(metadataPackageEntries(t, entries)), "overlay-app-a7x2m", "1.0.0"); err != nil {
		t.Fatalf("InspectPackageMetadata() error = %v, want directory mount accepted", err)
	}
}

// TestInspectPackageMetadataAcceptsOverlayRootMount 固化整棵 overlay 目录作为只读挂载源时，
// 包内保留的目录层级和子文件都能通过元数据门禁。
func TestInspectPackageMetadataAcceptsOverlayRootMount(t *testing.T) {
	t.Parallel()
	entries := []metadataEntry{
		{name: "manifest.yaml", content: `schema_version: 1
id: overlay-root-app-a7x2m
version: 1.0.0
display_name: Overlay Root
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    mounts:
      - {source: overlay, target: /etc/app, read_only: true}
`},
		{name: "compose.amd64.yaml", content: "services:\n  web:\n    image: overlay-root-app-a7x2m/web:1.0.0\n"},
		{name: "overlay", typeflag: tar.TypeDir},
		{name: "overlay/app.conf", content: "enabled=true\n"},
	}
	if _, err := contract.InspectPackageMetadata(bytes.NewReader(metadataPackageEntries(t, entries)), "overlay-root-app-a7x2m", "1.0.0"); err != nil {
		t.Fatalf("InspectPackageMetadata() error = %v, want overlay root mount accepted", err)
	}
}

func TestInspectPackageMetadataRejectsOverlaySymlink(t *testing.T) {
	t.Parallel()
	entries := []metadataEntry{
		{name: "manifest.yaml", content: `schema_version: 1
id: overlay-app-a7x2m
version: 1.0.0
display_name: Overlay
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`},
		{name: "compose.amd64.yaml", content: "services:\n  web:\n    image: overlay-app-a7x2m/web:1.0.0\n"},
		{name: "overlay/app.conf", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd"},
	}
	_, err := contract.InspectPackageMetadata(bytes.NewReader(metadataPackageEntries(t, entries)), "overlay-app-a7x2m", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "forbidden non-regular node") {
		t.Fatalf("InspectPackageMetadata() error = %v, want symlink rejection", err)
	}
}

func metadataPackage(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([]metadataEntry, 0, len(files))
	for name, content := range files {
		entries = append(entries, metadataEntry{name: name, content: content})
	}
	return metadataPackageEntries(t, entries)
}

type metadataEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
}

func metadataPackageEntries(t *testing.T, entries []metadataEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.content)), Typeflag: entry.typeflag, Linkname: entry.linkname}
		if entry.typeflag == tar.TypeDir {
			header.Mode = 0o755
			header.Size = 0
		}
		if entry.typeflag == tar.TypeSymlink {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.typeflag == tar.TypeDir || entry.typeflag == tar.TypeSymlink {
			continue
		}
		if _, err := tarWriter.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
