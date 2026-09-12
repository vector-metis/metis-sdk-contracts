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

func metadataPackage(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
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
