package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// buildContractFixture 从 apps/ 源码构建纯契约包；它不包含 Docker 镜像归档。
func buildContractFixture(t *testing.T, appID string) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := contract.BuildContractMPK(filepath.Join("testdata", "apps", appID), &output); err != nil {
		t.Fatalf("BuildContractMPK(%s) error = %v", appID, err)
	}
	return output.Bytes()
}

// 提交到仓库的 fixture 固定完整包格式和代表性注入面；它们刻意保持很小。
// 契约模式只审计配置和注入规则；镜像归档由真实发布包与安装阶段负责。
func TestValidateRepresentativeMPKFixtures(t *testing.T) {
	tests := []struct {
		name       string
		appID      string
		dependency bool
	}{
		{name: "basic application", appID: "basic-app-a7x2m"},
		{name: "settings and capabilities", appID: "integrated-app-a7x2m"},
		{name: "dependency injection", appID: "connected-app-a7x2m", dependency: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := buildContractFixture(t, test.appID)
			options := contract.ValidateOptions{ExpectedAppID: test.appID, ContractOnly: true}
			if test.dependency {
				options.ApplicationExist = func(dependencyID string) (bool, error) {
					return dependencyID == "core-a7x2m", nil
				}
			}
			summary, err := contract.ValidateMPK(bytes.NewReader(data), options)
			if err != nil {
				t.Fatalf("ValidateMPK(%s) error = %v", test.appID, err)
			}
			if summary.Manifest.ID != test.appID || summary.Manifest.Version != "1.0.0" {
				t.Fatalf("ValidateMPK(%s) identity = %s/%s", test.appID, summary.Manifest.ID, summary.Manifest.Version)
			}
			if test.appID == "integrated-app-a7x2m" {
				want := []string{"object-storage", "platform-api", "model-gateway", "compose-override"}
				if !slices.Equal(summary.Manifest.Capabilities, slices.Sorted(slices.Values(want))) {
					t.Fatalf("capabilities = %#v, want %v", summary.Manifest.Capabilities, want)
				}
			}
		})
	}
}

// TestValidateMPKStillRequiresImagesByDefault 防止 fixture 的测试开关泄漏到生产校验。
func TestValidateMPKStillRequiresImagesByDefault(t *testing.T) {
	data := buildContractFixture(t, "basic-app-a7x2m")
	if _, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{
		ExpectedAppID: "basic-app-a7x2m",
	}); err == nil {
		t.Fatal("ValidateMPK() = nil, want missing image archive error")
	}
}

func TestBuildContractMPKIsDeterministicAndRejectsImages(t *testing.T) {
	source := filepath.Join("testdata", "apps", "basic-app-a7x2m")
	var first bytes.Buffer
	if err := contract.BuildContractMPK(source, &first); err != nil {
		t.Fatalf("BuildContractMPK() error = %v", err)
	}
	var second bytes.Buffer
	if err := contract.BuildContractMPK(source, &second); err != nil {
		t.Fatalf("BuildContractMPK() error = %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("BuildContractMPK() output is not deterministic")
	}

	sourceWithImage := t.TempDir()
	files := map[string]string{
		"manifest.yaml":        "schema_version: 1\nid: image-app\nversion: 1.0.0\ndisplay_name: Image App\ntype: web\narch: [amd64]\ndependencies: []\nservices:\n  web:\n    endpoints: [{name: web, protocol: http, container_port: 8080}]\n",
		"compose.amd64.yaml":   "services:\n  web:\n    image: image-app/web:1.0.0\n",
		"images/amd64/web.tar": "not an image archive",
	}
	for name, data := range files {
		path := filepath.Join(sourceWithImage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := contract.BuildContractMPK(sourceWithImage, &bytes.Buffer{}); err == nil {
		t.Fatal("BuildContractMPK() = nil, want image archive error")
	}
}

// TestCoverageAppPackage 基于开发者文档固化完整包面：双架构、全部能力、
// 五类配置、三类模型插槽、依赖注入、截图、图标和 overlay。
func TestCoverageAppPackage(t *testing.T) {
	appID := "coverage-app-a7x2m"
	data := buildContractFixture(t, appID)
	summary, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{
		ExpectedAppID: appID, ContractOnly: true,
		ApplicationExist: func(dependencyID string) (bool, error) {
			return dependencyID == "required-core-a7x2m", nil
		},
	})
	if err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}
	if !slices.Equal(summary.Manifest.Capabilities, slices.Sorted(slices.Values([]string{"object-storage", "platform-api", "model-gateway", "compose-override"}))) {
		t.Fatalf("capabilities = %#v", summary.Manifest.Capabilities)
	}
	if len(summary.Manifest.Services) != 2 || len(summary.Manifest.Architectures) != 2 || len(summary.Manifest.Dependencies) != 2 {
		t.Fatalf("manifest = %+v", summary.Manifest)
	}
	if !strings.Contains(summary.AboutMD, "llm.0") {
		t.Fatalf("AboutMD = %q", summary.AboutMD)
	}
	slots, err := contract.ModelSlots(bytes.NewReader(data), contract.ArchAMD64)
	if err != nil {
		t.Fatalf("ModelSlots() error = %v", err)
	}
	if !slices.Equal(slots, []string{"embedding.0", "llm.0", "rerank.0"}) {
		t.Fatalf("ModelSlots() = %#v", slots)
	}

	names := packageMemberNames(t, data)
	for _, name := range []string{"icons/icon-64.png", "icons/icon-256.png", "screenshots/home.png", "screenshots/detail.png", "screenshots/detail.jpg", "screenshots/mobile.webp"} {
		if !slices.Contains(names, name) {
			t.Fatalf("package members %v lack %s", names, name)
		}
	}
	screenshot, err := contract.ReadMPKScreenshot(bytes.NewReader(data), "screenshots/mobile.webp")
	if err != nil || len(screenshot) == 0 {
		t.Fatalf("ReadMPKScreenshot() = %d bytes, %v", len(screenshot), err)
	}
	overlay, err := contract.ReadMPKOverlay(bytes.NewReader(data))
	if err != nil || string(overlay["etc/app/feature.conf"]) != "feature.enabled=true\n" {
		t.Fatalf("ReadMPKOverlay() = %#v, %v", overlay, err)
	}
}

// TestCoverageAppOptionalDependencyCanBeMissing 固化可选依赖和必需依赖的安装语义差异。
func TestCoverageAppOptionalDependencyCanBeMissing(t *testing.T) {
	data := buildContractFixture(t, "coverage-app-a7x2m")
	if _, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{
		ExpectedAppID: "coverage-app-a7x2m", ContractOnly: true,
		ApplicationExist: func(string) (bool, error) { return false, nil },
	}); err == nil {
		t.Fatal("ValidateMPK() = nil, want required dependency missing error")
	}
}

// TestPlanCoverageAppInstallsBothArchitectures 覆盖安装计划中的全部运行时注入面。
func TestPlanCoverageAppInstallsBothArchitectures(t *testing.T) {
	data := buildContractFixture(t, "coverage-app-a7x2m")
	modelBindings := map[string]contract.ModelSlotBinding{
		"llm.0": {Endpoint: "http://gateway/llm", Model: "gw-llm", APIKey: "llm-key", CardParams: map[string]string{
			"CONTEXT_WINDOW": "32768", "MAX_INPUT_TOKENS": "24576", "MAX_OUTPUT_TOKENS": "8192",
		}},
		"embedding.0": {Endpoint: "http://gateway/embeddings", Model: "gw-embedding", APIKey: "embedding-key", CardParams: map[string]string{
			"MAX_INPUT_TOKENS": "8192", "DIMENSIONS": "1536", "NORMALIZED": "true",
		}},
		"rerank.0": {Endpoint: "http://gateway/rerank", Model: "gw-rerank", APIKey: "rerank-key", CardParams: map[string]string{
			"MAX_INPUT_TOKENS": "8192", "MAX_DOCUMENTS": "64",
		}},
	}
	extra := map[string]string{
		"METIS_PLATFORM_ENDPOINT": "http://127.0.0.1", "METIS_APP_TOKEN": "api-token",
		"METIS_S3_ENDPOINT": "http://127.0.0.1:9002", "METIS_S3_REGION": "local",
		"METIS_S3_ACCESS_KEY": "access", "METIS_S3_SECRET_KEY": "secret", "METIS_S3_BUCKET": "coverage",
		"METIS_S3_SHARED_BUCKETS": `["transcripts"]`,
	}
	for _, architecture := range []string{contract.ArchAMD64, contract.ArchARM64} {
		plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
			Architecture: architecture, BaseDir: "/var/lib/metis/apps/coverage", ContractOnly: true,
			PublicHost: "metis.internal", MasterIP: "10.0.0.10",
			Settings: map[string]string{
				"site_name": "Coverage", "log_level": "debug", "max_upload_mb": "64",
				"enable_audit": "false", "webhook_token": "webhook",
			},
			ModelBindings: modelBindings, ExtraEnvironment: extra,
			ComposeOverrides: map[string][]contract.ComposeOverride{
				"infer": {{ID: 1, Name: "non-standard resources", Body: "cpus: 4\nmem_limit: 8GiB\n"}},
			},
			AssignPort: func(name string) (int, error) {
				if name == "METIS_ENTRY_PORT" {
					return 19082, nil
				}
				return 0, fmt.Errorf("unexpected port %s", name)
			},
		})
		if err != nil {
			t.Fatalf("PlanInstall(%s) error = %v", architecture, err)
		}
		for _, item := range []struct{ key, value string }{
			{"METIS_APP_ID", "coverage-app-a7x2m"}, {"METIS_APP_NAME", "Contract Coverage"}, {"METIS_APP_VERSION", "1.0.0"},
			{"METIS_ENTRY_PORT", "19082"}, {"METIS_SETTING_SITE_NAME", "Coverage"},
			{"METIS_LLM_0_CONTEXT_WINDOW", "32768"}, {"METIS_EMBEDDING_0_DIMENSIONS", "1536"},
			{"METIS_EMBEDDING_0_NORMALIZED", "true"}, {"METIS_RERANK_0_MAX_DOCUMENTS", "64"},
		} {
			if plan.Environment[item.key] != item.value {
				t.Fatalf("Environment[%s] = %q, want %q", item.key, plan.Environment[item.key], item.value)
			}
		}
		if len(plan.Settings) != 6 {
			t.Fatalf("settings = %#v", plan.Settings)
		}
		if plan.Environment["METIS_SETTING_OPTIONAL_CONSOLE_TOKEN"] != "" {
			t.Fatalf("optional secret = %q, want empty", plan.Environment["METIS_SETTING_OPTIONAL_CONSOLE_TOKEN"])
		}
		if !bytes.Contains(plan.Compose, []byte("mem_limit: 8GiB")) {
			t.Fatalf("PlanInstall(%s) compose lacks override resource limit:\n%s", architecture, plan.Compose)
		}
	}
}

func packageMemberNames(t *testing.T, data []byte) []string {
	t.Helper()
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	names := make([]string, 0)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}
