package contract_test

import (
	"bytes"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// TestPlanInstallRendersRepresentativePackage 固化平台侧安装计划：
// 契约负责包校验、配置校验、Compose 展平和资源默认值，平台不重复实现规则。
func TestPlanInstallRendersRepresentativePackage(t *testing.T) {
	data := buildContractFixture(t, "integrated-app-a7x2m")
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/var/lib/metis/apps/integrated", ContractOnly: true,
		PublicHost: "platform.example.invalid", MasterIP: "10.0.0.10",
		Settings: map[string]string{"wiki-name": "Real Wiki"},
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://127.0.0.1", "METIS_APP_TOKEN": "token",
			"METIS_S3_ENDPOINT": "http://127.0.0.1:9002", "METIS_S3_REGION": "local",
			"METIS_S3_ACCESS_KEY": "key", "METIS_S3_SECRET_KEY": "secret", "METIS_S3_BUCKET": "app-bucket",
			"METIS_S3_SHARED_BUCKETS": "",
		},
		ModelBindings: map[string]contract.ModelSlotBinding{"llm.0": {
			Endpoint: "http://gateway", Model: "gpt", APIKey: "sk",
			CardParams: map[string]string{
				"CONTEXT_WINDOW": "32768", "MAX_INPUT_TOKENS": "24576", "MAX_OUTPUT_TOKENS": "8192",
			},
		}},
		ComposeOverrides: map[string][]contract.ComposeOverride{
			"web": {{ID: 1, Name: "non-standard resources", Body: "cpus: 4\nmem_limit: 8GiB\n"}},
		},
		AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	rendered := string(plan.Compose)
	for _, want := range []string{"cpus: 4", "mem_limit: 8GiB"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("install plan does not contain %q\n%s", want, rendered)
		}
	}
	for _, want := range []string{
		"METIS_DIR_CONFIG: /var/lib/metis/apps/integrated/config",
		"METIS_ENTRY_PORT: \"19081\"",
		"METIS_S3_ENDPOINT: http://127.0.0.1:9002",
		"METIS_PLATFORM_ENDPOINT: http://127.0.0.1",
		"METIS_APP_TOKEN: token",
		"METIS_LLM_0_ENDPOINT: http://gateway",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("install plan does not inject %q\n%s", want, rendered)
		}
	}
	if plan.Environment["METIS_ENTRY_PORT"] != "19081" || plan.Environment["METIS_SETTING_WIKI_NAME"] != "Real Wiki" ||
		plan.Environment["METIS_DIR_DATA"] != "/var/lib/metis/apps/integrated/data" {
		t.Fatalf("install environment = %#v", plan.Environment)
	}
}

// TestPlanInstallPreservesAllowedComposeFields 固化普通 Compose 属性在人工审核通过后
// 会进入最终部署副本；平台只替换其负责生成的 services 运行事实。
func TestPlanInstallPreservesAllowedComposeFields(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`name: reviewed-application
services:
  web:
    image: demo-a7x2m/web:1.0.0
    networks: [backend]
networks:
  backend:
    driver: bridge
x-review-note: manually-approved
`)
	})

	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	rendered := string(plan.Compose)
	for _, want := range []string{"name: reviewed-application", "networks:", "backend:", "x-review-note: manually-approved"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered compose lost allowed field %q:\n%s", want, rendered)
		}
	}
}

// TestPlanInstallInjectsOneApplicationIdentityIntoEveryService 锁定应用身份与 capability 无关，
// 并且同一 Compose 的所有 service 都获得完全相同的三项身份。
func TestPlanInstallInjectsOneApplicationIdentityIntoEveryService(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: identity-service-a7x2m
version: 1.0.0
display_name: Identity Service
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints:
      - {name: tcp, protocol: tcp, container_port: 9000}
  helper: {}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  daemon:
    image: identity-service-a7x2m/daemon:1.0.0
  helper:
    image: identity-service-a7x2m/helper:1.0.0
`)
		delete(files, "images/amd64/app.tar")
		files["images/amd64/daemon.tar"] = dockerArchive(t, "identity-service-a7x2m/daemon:1.0.0")
		files["images/amd64/helper.tar"] = dockerArchive(t, "identity-service-a7x2m/helper:1.0.0")
	})
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/identity-service-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
			"METIS_APP_TOKEN":         "shared-token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	rendered := string(plan.Compose)
	for _, service := range []string{"daemon", "helper"} {
		marker := service + ":"
		if !strings.Contains(rendered, marker) {
			t.Fatalf("rendered compose lacks service %q\n%s", service, rendered)
		}
	}
	for _, want := range []string{
		"METIS_APP_ID: identity-service-a7x2m",
		"METIS_PLATFORM_ENDPOINT: http://platform.example.invalid",
		"METIS_APP_TOKEN: shared-token",
		"- platform.example.invalid:10.0.0.10",
	} {
		if count := strings.Count(rendered, want); count != 2 {
			t.Fatalf("rendered compose contains %q %d times, want 2\n%s", want, count, rendered)
		}
	}
	if strings.Contains(rendered, "METIS_API_") {
		t.Fatalf("rendered compose contains legacy application identity\n%s", rendered)
	}
}

// TestPlanInstallRendersServiceEndpoints 验证同一个 Compose service 可以同时承载 TCP 和 UDP endpoint。
func TestPlanInstallRendersServiceEndpoints(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo Service
type: service
arch: [amd64]
dependencies: []
services:
  daemon:
    endpoints:
      - {name: database, protocol: tcp, container_port: 5432}
      - {name: discovery, protocol: udp, container_port: 5353}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  daemon:
    image: demo-a7x2m/daemon:1.0.0
`)
		files["images/amd64/app.tar"] = dockerArchive(t, "demo-a7x2m/daemon:1.0.0")
	})
	ports := map[string]int{
		"METIS_ENDPOINT_DATABASE_PORT":  22001,
		"METIS_ENDPOINT_DISCOVERY_PORT": 22002,
	}
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(name string) (int, error) {
			return ports[name], nil
		},
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	for _, want := range []string{
		"published: ${METIS_ENDPOINT_DATABASE_PORT}",
		"published: ${METIS_ENDPOINT_DISCOVERY_PORT}",
		"protocol: tcp",
		"protocol: udp",
	} {
		if !strings.Contains(string(plan.Compose), want) {
			t.Fatalf("install plan does not contain %q\n%s", want, plan.Compose)
		}
	}
	if plan.Environment["METIS_ENDPOINT_DATABASE_PORT"] != "22001" || plan.Environment["METIS_ENDPOINT_DISCOVERY_PORT"] != "22002" {
		t.Fatalf("install environment = %#v", plan.Environment)
	}
}

// TestPlanInstallScopesCapabilityEnvironmentToDeclaringService 防止模型凭据和能力变量
// 被错误广播到不需要该能力的 Compose service。
func TestPlanInstallScopesCapabilityEnvironmentToDeclaringService(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: scoped-app-a7x2m
version: 1.0.0
display_name: Scoped
type: web
arch: [amd64]
dependencies: []
models:
  llm.0: {interface: openai.chat.completions}
services:
  web:
    endpoints: [{name: chat, protocol: http, container_port: 8080}]
    capabilities:
      model-gateway: {slots: [llm.0]}
  worker: {}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: scoped-app-a7x2m/web:1.0.0
  worker:
    image: scoped-app-a7x2m/worker:1.0.0
`)
		delete(files, "images/amd64/app.tar")
		files["images/amd64/web.tar"] = dockerArchive(t, "scoped-app-a7x2m/web:1.0.0")
		files["images/amd64/worker.tar"] = dockerArchive(t, "scoped-app-a7x2m/worker:1.0.0")
	})
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/var/lib/metis/apps/scoped", PublicHost: "platform.example.invalid", MasterIP: "10.0.0.10",
		ModelBindings: map[string]contract.ModelSlotBinding{"llm.0": {
			Endpoint: "http://gateway", Model: "gw-chat", APIKey: "secret",
			CardParams: map[string]string{"CONTEXT_WINDOW": "32768", "MAX_INPUT_TOKENS": "24576", "MAX_OUTPUT_TOKENS": "8192"},
		}},
		ExtraEnvironment: map[string]string{"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid", "METIS_APP_TOKEN": "token"},
		AssignPort:       func(string) (int, error) { return 22001, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	compose := string(plan.Compose)
	webStart, workerStart := strings.Index(compose, "web:"), strings.Index(compose, "worker:")
	if webStart < 0 || workerStart < 0 || webStart > workerStart {
		t.Fatalf("unexpected service order:\n%s", compose)
	}
	webSection, workerSection := compose[webStart:workerStart], compose[workerStart:]
	if !strings.Contains(webSection, "METIS_LLM_0_API_KEY: secret") || strings.Contains(workerSection, "METIS_LLM_0_API_KEY") {
		t.Fatalf("model environment was not scoped:\n%s", compose)
	}
}

func TestPlanPreparedInstallRewritesEveryImageReference(t *testing.T) {
	t.Parallel()

	metadata := contract.PackageMetadata{
		Manifest: contract.Manifest{SchemaVersion: 1, ID: "prepared-app-a7x2m", Version: "1.0.0", DisplayName: "Prepared",
			Architectures: []string{contract.ArchAMD64}, Dependencies: []contract.Dependency{}, Type: contract.ApplicationTypeWeb,
			Services: map[string]contract.ManifestService{"web": {Endpoints: []contract.ServiceEndpoint{{Name: "web", Service: "web", Protocol: contract.EndpointProtocolHTTP, ContainerPort: 8080}}}}},
		Settings: []contract.Setting{},
		Compose: map[string]string{contract.ArchAMD64: `services:
  web:
    image: prepared-app-a7x2m/web:1.0.0
  worker:
    image: prepared-app-a7x2m/worker:1.0.0
`},
	}
	plan, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/prepared-app-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ImageReferences: map[string]string{
			"prepared-app-a7x2m/web:1.0.0":    "platform.example.invalid/prepared-app-a7x2m/web:1.0.0",
			"prepared-app-a7x2m/worker:1.0.0": "platform.example.invalid/prepared-app-a7x2m/worker:1.0.0",
		},
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid", "METIS_APP_TOKEN": "token",
		},
		AssignPort: func(string) (int, error) { return 20001, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(plan.Compose)
	for _, target := range []string{
		"platform.example.invalid/prepared-app-a7x2m/web:1.0.0",
		"platform.example.invalid/prepared-app-a7x2m/worker:1.0.0",
	} {
		if !strings.Contains(rendered, target) {
			t.Fatalf("rendered compose does not contain %q:\n%s", target, rendered)
		}
	}
	if strings.Contains(rendered, "image: prepared-app-a7x2m/") {
		t.Fatalf("rendered compose still contains source image:\n%s", rendered)
	}
}

func TestPlanPreparedInstallRejectsIncompleteImageMapping(t *testing.T) {
	t.Parallel()

	metadata := contract.PackageMetadata{
		Manifest: contract.Manifest{SchemaVersion: 1, ID: "prepared-app-a7x2m", Version: "1.0.0", DisplayName: "Prepared",
			Architectures: []string{contract.ArchAMD64}, Dependencies: []contract.Dependency{}, Type: contract.ApplicationTypeWeb,
			Services: map[string]contract.ManifestService{"web": {Endpoints: []contract.ServiceEndpoint{{Name: "web", Service: "web", Protocol: contract.EndpointProtocolHTTP, ContainerPort: 8080}}}}},
		Settings: []contract.Setting{},
		Compose: map[string]string{contract.ArchAMD64: `services:
  web:
    image: prepared-app-a7x2m/web:1.0.0
`},
	}
	_, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/tmp/prepared-app-a7x2m",
		AssignPort: func(string) (int, error) { return 20001, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "image mapping") {
		t.Fatalf("PlanPreparedInstall() error = %v, want image mapping failure", err)
	}
}

// TestModelSlotsReadsSelectedArchitecture 固化默认模型绑定前的插槽发现契约。
func TestModelSlotsReadsSelectedArchitecture(t *testing.T) {
	data := buildContractFixture(t, "integrated-app-a7x2m")
	slots, err := contract.ModelSlots(bytes.NewReader(data), contract.ArchAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0] != "llm.0" {
		t.Fatalf("ModelSlots() = %#v, want [llm.0]", slots)
	}
}

// TestPlanInstallRejectsMissingRequiredSetting 防止安装进入无法安全启动的半配置状态。
func TestPlanInstallRejectsMissingRequiredSetting(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
settings:
  required-token: {label: Token, type: string, required: true}
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    environment: {TOKEN: {setting: required-token}}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
`)
	})
	_, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/tmp/app", AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "setting \"required-token\" is required") {
		t.Fatalf("PlanInstall() error = %v, want required setting failure", err)
	}
}
