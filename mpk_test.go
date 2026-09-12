package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func dockerArchive(t *testing.T, tags ...string) []byte {
	return dockerArchiveForArchitecture(t, "amd64", tags...)
}

func dockerArchiveForArchitecture(t *testing.T, architecture string, tags ...string) []byte {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	config := `{"architecture":"` + architecture + `","os":"linux"}`
	if err := writer.WriteHeader(&tar.Header{Name: "config.json", Size: int64(len(config)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	manifest := `[{"Config":"config.json","RepoTags":["` + tags[0] + `"]}]`
	if err := writer.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(manifest)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func buildMPK(t *testing.T, mutate func(map[string][]byte)) []byte {
	t.Helper()
	files := map[string][]byte{
		"manifest.yaml": []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
services:
  web:
    endpoints:
      - {name: web, protocol: http, container_port: 8080}
`),
		"compose.amd64.yaml": []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
`),
		"images/amd64/app.tar": dockerArchive(t, "demo-a7x2m/web:1.0.0"),
		"icons/icon-64.png":    pngFixture(t, 64, 64),
		"icons/icon-256.png":   pngFixture(t, 256, 256),
	}
	if mutate != nil {
		mutate(files)
	}
	var raw bytes.Buffer
	gzipWriter := gzip.NewWriter(&raw)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func pngFixture(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	picture.Set(0, 0, color.RGBA{R: 35, G: 99, B: 235, A: 255})
	if err := png.Encode(&output, picture); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return output.Bytes()
}

func validate(t *testing.T, data []byte) (*contract.PackageSummary, error) {
	t.Helper()
	return contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"})
}

func TestValidateMPKAcceptsValidPackage(t *testing.T) {
	summary, err := validate(t, buildMPK(t, nil))
	if err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}
	if summary.Manifest.Version != "1.0.0" || summary.Manifest.Type != contract.ApplicationTypeWeb || summary.Manifest.Services["web"].Endpoints[0].ContainerPort != 8080 || summary.SHA256 == "" {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

// TestValidateMPKAcceptsServiceApplication 固化 Service 应用通过具名 endpoint 暴露原始协议，
// 不需要也不得声明 Web 主入口。
func TestValidateMPKAcceptsServiceApplication(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo Service
type: service
arch: [amd64]
dependencies: []
services:
  database:
    endpoints:
      - {name: database, protocol: tcp, container_port: 5432}
  daemon:
    endpoints:
      - {name: discovery, protocol: udp, container_port: 5353}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  database:
    image: demo-a7x2m/database:1.0.0
  daemon:
    image: demo-a7x2m/daemon:1.0.0
`)
		files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/database:1.0.0")
		files["images/amd64/daemon.tar"] = dockerArchive(t, "demo-a7x2m/daemon:1.0.0")
	})

	summary, err := validate(t, data)
	if err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}
	if summary.Manifest.Type != contract.ApplicationTypeService || len(summary.Manifest.Services) != 2 {
		t.Fatalf("service manifest = %+v", summary.Manifest)
	}
	endpoint := summary.Manifest.Services["database"].Endpoints[0]
	if endpoint.Name != "database" || endpoint.Service != "database" || endpoint.Protocol != contract.EndpointProtocolTCP || endpoint.ContainerPort != 5432 {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	endpoint = summary.Manifest.Services["daemon"].Endpoints[0]
	if endpoint.Name != "discovery" || endpoint.Service != "daemon" || endpoint.Protocol != contract.EndpointProtocolUDP || endpoint.ContainerPort != 5353 {
		t.Fatalf("endpoint = %+v", endpoint)
	}
}

// TestValidateMPKRejectsServiceMissingFromOneArchitecture 要求每个声明架构都能承载全部 Service endpoint。
func TestValidateMPKRejectsServiceMissingFromOneArchitecture(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo Service
type: service
arch: [amd64, arm64]
dependencies: []
services:
  database:
    endpoints: [{name: database, protocol: tcp, container_port: 5432}]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  database:
    image: demo-a7x2m/database:1.0.0
`)
		files["compose.arm64.yaml"] = []byte(`services:
  worker:
    image: demo-a7x2m/worker:1.0.0
`)
		files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/database:1.0.0")
		files["images/arm64/app.tar"] = dockerArchiveForArchitecture(t, "arm64", "demo-a7x2m/worker:1.0.0")
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), `compose.arm64.yaml contains undeclared service "worker"`) {
		t.Fatalf("ValidateMPK() error = %v, want missing arm64 endpoint service", err)
	}
}

// TestValidateMPKRejectsNormalizedEndpointNameCollision 防止两个端点映射到同一个部署环境变量。
func TestValidateMPKRejectsNormalizedEndpointNameCollision(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo Service
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints:
      - {name: metrics-http, protocol: tcp, container_port: 9000}
      - {name: metrics_http, protocol: udp, container_port: 9001}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  daemon:
    image: demo-a7x2m/daemon:1.0.0
`)
		files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/daemon:1.0.0")
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "normalized endpoint name") {
		t.Fatalf("ValidateMPK() error = %v, want normalized endpoint name conflict", err)
	}
}

// TestValidateMPKRejectsLegacyEntranceLabels 固化 manifest 是入口类型和端口的唯一事实来源。
func TestValidateMPKRejectsLegacyEntranceLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels string
	}{
		{name: "mapping", labels: `{platform.expose: http}`},
		{name: "list with value", labels: `["platform.expose=http"]`},
		{name: "list without value", labels: `["platform.expose"]`},
		{name: "case insensitive", labels: `["Platform.Expose"]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := buildMPK(t, func(files map[string][]byte) {
				files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    labels: ` + test.labels + "\n")
			})

			_, err := validate(t, data)
			if err == nil || !strings.Contains(err.Error(), "cannot declare platform label") {
				t.Fatalf("ValidateMPK() error = %v, want legacy entrance label failure", err)
			}
		})
	}
}

func TestValidateMPKAllowsOrdinaryValuelessLabel(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    labels: ["com.example.feature"]
`)
	})

	if _, err := validate(t, data); err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}
}

func TestValidateMPKRejectsInterpolationOutsideServices(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`name: ${PROJECT_NAME}
services:
  web:
    image: demo-a7x2m/web:1.0.0
`)
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "cannot use Compose interpolation") {
		t.Fatalf("ValidateMPK() error = %v, want top-level interpolation failure", err)
	}
}

func TestValidateMPKAllowsEscapedComposeDollar(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`name: $${PROJECT_NAME}
services:
  web:
    image: demo-a7x2m/web:1.0.0
`)
	})

	if _, err := validate(t, data); err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}
}

// TestValidateMPKRejectsLegacyDependencyPlaceholders 固化依赖只能通过统一应用身份和 Runtime SDK 发现。
func TestValidateMPKRejectsLegacyDependencyPlaceholders(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies:
  - {id: wiki-b3k9q, alias: wiki, required: true}
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    environment:
      WIKI_TOKEN: ${METIS_APP_WIKI_SERVICE_TOKEN}
`)
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "cannot declare environment") {
		t.Fatalf("ValidateMPK() error = %v, want legacy dependency placeholder failure", err)
	}
}

// TestValidateMPKRejectsHostNetworkMode 防止应用绕过平台分配的回环端口和 Agent 隧道。
func TestValidateMPKRejectsHostNetworkMode(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    network_mode: host
`)
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "host network") {
		t.Fatalf("ValidateMPK() error = %v, want host network failure", err)
	}
}

// TestValidateMPKRejectsExtraHosts 保证 Master host 映射只能由平台生成。
func TestValidateMPKRejectsExtraHosts(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    extra_hosts: ["example.invalid:192.0.2.10"]
`)
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "cannot declare extra_hosts") {
		t.Fatalf("ValidateMPK() error = %v, want extra_hosts failure", err)
	}
}

// TestValidateMPKRejectsInvalidApplicationShape 覆盖 Web 与 Service 互斥字段和 endpoint 闭集。
func TestValidateMPKRejectsInvalidApplicationShape(t *testing.T) {
	serviceCompose := `services:
  daemon:
    image: demo-a7x2m/daemon:1.0.0
`
	tests := []struct {
		name      string
		manifest  string
		service   bool
		wantError string
	}{
		{
			name: "missing application type",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
arch: [amd64]
dependencies: []
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`,
			wantError: "type",
		},
		{
			name: "web declares service endpoints",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
services:
  web:
    endpoints: [{name: web, protocol: tcp, container_port: 8080}]
`,
			wantError: "can only declare http endpoints",
		},
		{
			name: "service declares web entrance",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints: [{name: daemon, protocol: http, container_port: 9000}]
`,
			service: true, wantError: "must use tcp or udp",
		},
		{
			name: "service has no endpoints",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: service
arch: [amd64]
dependencies: []
services:
  daemon: {}
`,
			service: true, wantError: "requires at least one endpoint",
		},
		{
			name: "endpoint protocol is outside closed set",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints: [{name: daemon, protocol: http, container_port: 9000}]
`,
			service: true, wantError: "must use tcp or udp",
		},
		{
			name: "endpoint port is invalid",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints: [{name: daemon, protocol: udp, container_port: 0}]
`,
			service: true, wantError: "container_port must be between",
		},
		{
			name: "endpoint service does not exist",
			manifest: `id: demo-a7x2m
schema_version: 1
version: 1.0.0
display_name: Demo
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints: [{name: missing, service: missing, protocol: tcp, container_port: 9000}]
`,
			service: true, wantError: "must belong to its declaring service",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := buildMPK(t, func(files map[string][]byte) {
				files["manifest.yaml"] = []byte(test.manifest)
				if test.service {
					files["compose.amd64.yaml"] = []byte(serviceCompose)
					files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/daemon:1.0.0")
				}
			})
			_, err := validate(t, data)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ValidateMPK() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestValidateMPKRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
	}{
		{"missing manifest", func(files map[string][]byte) { delete(files, "manifest.yaml") }},
		{"duplicate version callback", func(files map[string][]byte) {}},
		{"unsafe integration url", func(files map[string][]byte) {
			files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
integration_docs_url: http://example.com/docs
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`)
		}},
		{"reserved image tag", func(files map[string][]byte) {
			files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:latest
`)
			files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/web:latest")
		}},
		{"source compose publishes host port", func(files map[string][]byte) {
			files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    ports: ["127.0.0.1:28080:8080"]
`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "duplicate version callback" {
				_, err := contract.ValidateMPK(bytes.NewReader(buildMPK(t, test.mutate)), contract.ValidateOptions{
					ExpectedAppID: "demo-a7x2m",
					VersionExists: func(string) (bool, error) { return true, nil },
				})
				if err == nil {
					t.Fatal("ValidateMPK() = nil, want duplicate version error")
				}
				return
			}
			_, err := validate(t, buildMPK(t, test.mutate))
			if err == nil {
				t.Fatalf("ValidateMPK() = nil, want error")
			}
		})
	}
}

func TestValidateMPKPropagatesServiceErrors(t *testing.T) {
	want := errors.New("database unavailable")
	_, err := contract.ValidateMPK(bytes.NewReader(buildMPK(t, nil)), contract.ValidateOptions{
		ExpectedAppID: "demo-a7x2m",
		VersionExists: func(string) (bool, error) { return false, want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("ValidateMPK() error = %v, want %v", err, want)
	}
}

// buildHeaderedMPK 构造可精确控制 tar 节点类型的包，安全用例由调用方保证。
func buildHeaderedMPK(t *testing.T, headers []tar.Header) []byte {
	t.Helper()
	var raw bytes.Buffer
	gzipWriter := gzip.NewWriter(&raw)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, header := range headers {
		if err := tarWriter.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tarWriter.Write([]byte("manifest")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

// 验证包体解析层拒绝绝对路径、逃逸路径、链接和特殊设备节点。
func TestValidateMPKRejectsUnsafeTarNodes(t *testing.T) {
	tests := []struct {
		name   string
		unsafe func(*tar.Header)
	}{
		{"absolute path", func(header *tar.Header) { header.Name = "/etc/metis" }},
		{"parent path", func(header *tar.Header) { header.Name = "../metis" }},
		{"symlink", func(header *tar.Header) {
			header.Name, header.Typeflag, header.Linkname = "link", tar.TypeSymlink, "manifest.yaml"
		}},
		{"hardlink", func(header *tar.Header) {
			header.Name, header.Typeflag, header.Linkname = "link", tar.TypeLink, "manifest.yaml"
		}},
		{"character device", func(header *tar.Header) { header.Name, header.Typeflag = "device", tar.TypeChar }},
		{"block device", func(header *tar.Header) { header.Name, header.Typeflag = "device", tar.TypeBlock }},
		{"fifo", func(header *tar.Header) { header.Name, header.Typeflag = "pipe", tar.TypeFifo }},
		{"socket-like node", func(header *tar.Header) { header.Name, header.Typeflag = "socket", '?' }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsafe := tar.Header{Name: "unsafe", Size: 0}
			test.unsafe(&unsafe)
			data := buildHeaderedMPK(t, []tar.Header{
				{Name: "manifest.yaml", Typeflag: tar.TypeReg, Size: 8},
				unsafe,
			})
			if _, err := validate(t, data); err == nil {
				t.Fatalf("ValidateMPK() = nil, want unsafe node error")
			}
		})
	}
}

// 验证同名成员不会被 map 去重遮蔽，防止构造二义包根。
func TestValidateMPKRejectsDuplicateTarMembers(t *testing.T) {
	data := buildHeaderedMPK(t, []tar.Header{
		{Name: "manifest.yaml", Typeflag: tar.TypeReg, Size: 8},
		{Name: "manifest.yaml", Typeflag: tar.TypeReg, Size: 8},
	})
	if _, err := validate(t, data); err == nil {
		t.Fatal("ValidateMPK() = nil, want duplicate member error")
	}
}

func TestReadMPKScreenshotOnlyAllowsDeclaredSafeMembers(t *testing.T) {
	screenshotData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
screenshots: [screenshots/home.png]
`)
		files["screenshots/home.png"] = screenshotData
	})
	content, readErr := contract.ReadMPKScreenshot(bytes.NewReader(data), "screenshots/home.png")
	if readErr != nil || !bytes.Equal(content, screenshotData) {
		t.Fatalf("ReadMPKScreenshot() = %d bytes, %v", len(content), err)
	}
	if _, err := contract.ReadMPKScreenshot(bytes.NewReader(data), "manifest.yaml"); err == nil {
		t.Fatal("ReadMPKScreenshot(manifest.yaml) = nil, want error")
	}
	if err := contract.ValidateScreenshotPath("screenshots/home.svg"); err == nil {
		t.Fatal("ValidateScreenshotPath(svg) = nil, want error")
	}
}
