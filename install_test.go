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
		PublicHost: "metis.internal", MasterIP: "10.0.0.10",
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
		"METIS_ENTRY_PORT: \"19081\"",
		"METIS_S3_ENDPOINT: http://127.0.0.1:9002",
		"METIS_PLATFORM_ENDPOINT: http://127.0.0.1",
		"METIS_APP_TOKEN: token",
		"METIS_LLM_0_ENDPOINT: http://gateway",
		"source: ./config",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("install plan does not inject %q\n%s", want, rendered)
		}
	}
	if plan.Environment["METIS_ENTRY_PORT"] != "19081" || plan.Environment["METIS_SETTING_WIKI_NAME"] != "Real Wiki" {
		t.Fatalf("install environment = %#v", plan.Environment)
	}
	for name := range plan.Environment {
		if strings.HasPrefix(name, "METIS_DIR_") {
			t.Fatalf("install environment contains removed directory variable %q", name)
		}
	}
	if strings.Contains(rendered, "METIS_DIR_") || strings.Contains(rendered, "/var/lib/metis/") {
		t.Fatalf("install plan contains host path or removed directory variable:\n%s", rendered)
	}
}

// TestPlanInstallKeepsCanonicalOverlaySources 验证 manifest 已声明的
// overlay source 与 subpath 在最终 Compose 中渲染为 scope 相对路径。
func TestPlanInstallKeepsCanonicalOverlaySources(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		manifest := string(files["manifest.yaml"])
		manifest = strings.Replace(manifest,
			"      - {name: web, protocol: http, container_port: 8080}\n",
			"      - {name: web, protocol: http, container_port: 8080}\n    mounts:\n      - {source: overlay, subpath: config.yaml, target: /etc/app.yaml, read_only: true}\n      - {source: overlay, subpath: static, target: /usr/share/app, read_only: true}\n",
			1)
		files["manifest.yaml"] = []byte(manifest)
		files["overlay/config.yaml"] = []byte("enabled: true\n")
		files["overlay/static/index.html"] = []byte("<main>overlay</main>\n")
	})
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/overlay-a7x2m",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	rendered := string(plan.Compose)
	for _, want := range []string{"source: ./overlay/config.yaml", "source: ./overlay/static"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("install plan does not contain canonical overlay source %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "././overlay") {
		t.Fatalf("install plan contains duplicated overlay prefix:\n%s", rendered)
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
    restart: unless-stopped
    networks: [backend]
    healthcheck:
      test: [CMD-SHELL, "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
networks:
  backend:
    driver: bridge
x-review-note: manually-approved
`)
	})

	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-a7x2m",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}
	rendered := string(plan.Compose)
	for _, want := range []string{"name: reviewed-application", "networks:", "backend:", "x-review-note: manually-approved", "$${POSTGRES_USER}", "$${POSTGRES_DB}"} {
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
    restart: unless-stopped
  helper:
    image: identity-service-a7x2m/helper:1.0.0
    restart: unless-stopped
`)
		delete(files, "images/amd64/app.tar")
		files["images/amd64/daemon.tar"] = dockerArchive(t, "identity-service-a7x2m/daemon:1.0.0")
		files["images/amd64/helper.tar"] = dockerArchive(t, "identity-service-a7x2m/helper:1.0.0")
	})
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/identity-service-a7x2m",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
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
		"METIS_PLATFORM_ENDPOINT: http://metis.internal",
		"METIS_APP_TOKEN: shared-token",
		"- metis.internal:10.0.0.10",
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
    restart: unless-stopped
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
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
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
    restart: unless-stopped
  worker:
    image: scoped-app-a7x2m/worker:1.0.0
    restart: unless-stopped
`)
		delete(files, "images/amd64/app.tar")
		files["images/amd64/web.tar"] = dockerArchive(t, "scoped-app-a7x2m/web:1.0.0")
		files["images/amd64/worker.tar"] = dockerArchive(t, "scoped-app-a7x2m/worker:1.0.0")
	})
	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/var/lib/metis/apps/scoped", PublicHost: "metis.internal", MasterIP: "10.0.0.10",
		ModelBindings: map[string]contract.ModelSlotBinding{"llm.0": {
			Endpoint: "http://gateway", Model: "gw-chat", APIKey: "secret",
			CardParams: map[string]string{"CONTEXT_WINDOW": "32768", "MAX_INPUT_TOKENS": "24576", "MAX_OUTPUT_TOKENS": "8192"},
		}},
		ExtraEnvironment: map[string]string{"METIS_PLATFORM_ENDPOINT": "http://metis.internal", "METIS_APP_TOKEN": "token"},
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
			Services: map[string]contract.ManifestService{"web": {Lifecycle: contract.ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []contract.ServiceEndpoint{{Name: "web", Service: "web", Protocol: contract.EndpointProtocolHTTP, ContainerPort: 8080}}}}},
		Settings: []contract.Setting{},
		Compose: map[string]string{contract.ArchAMD64: `services:
  web:
    image: prepared-app-a7x2m/web:1.0.0
    restart: unless-stopped
  worker:
    image: prepared-app-a7x2m/worker:1.0.0
    restart: unless-stopped
`},
	}
	plan, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/prepared-app-a7x2m",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ImageReferences: map[string]string{
			"prepared-app-a7x2m/web:1.0.0":    "metis.internal/prepared-app-a7x2m/web:1.0.0",
			"prepared-app-a7x2m/worker:1.0.0": "metis.internal/prepared-app-a7x2m/worker:1.0.0",
		},
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal", "METIS_APP_TOKEN": "token",
		},
		AssignPort: func(string) (int, error) { return 20001, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(plan.Compose)
	for _, target := range []string{
		"metis.internal/prepared-app-a7x2m/web:1.0.0",
		"metis.internal/prepared-app-a7x2m/worker:1.0.0",
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
			Services: map[string]contract.ManifestService{"web": {Lifecycle: contract.ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []contract.ServiceEndpoint{{Name: "web", Service: "web", Protocol: contract.EndpointProtocolHTTP, ContainerPort: 8080}}}}},
		Settings: []contract.Setting{},
		Compose: map[string]string{contract.ArchAMD64: `services:
  web:
    image: prepared-app-a7x2m/web:1.0.0
    restart: unless-stopped
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
    restart: unless-stopped
`)
	})
	_, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64, BaseDir: "/tmp/app", AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "setting \"required-token\" is required") {
		t.Fatalf("PlanInstall() error = %v, want required setting failure", err)
	}
}

// TestPlanInstallInjectsModelTraits 验证模型视觉、思考、工具调用能力特性环境变量的正确注入。
func TestPlanInstallInjectsModelTraits(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-traits-a7x2m
version: 1.0.0
display_name: Demo Traits
type: web
arch: [amd64]
dependencies: []
models:
  llm.0:
    interface: openai.chat.completions
    traits: [vision, thinking, tools]
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    capabilities:
      model-gateway:
        slots: [llm.0]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
`)
	})

	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-traits",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		ModelBindings: map[string]contract.ModelSlotBinding{
			"llm.0": {
				Endpoint: "http://gateway/v1",
				Model:    "gpt-4o",
				APIKey:   "sk-test",
				CardParams: map[string]string{
					"CONTEXT_WINDOW":    "128000",
					"MAX_INPUT_TOKENS":  "120000",
					"MAX_OUTPUT_TOKENS": "4096",
					"SUPPORTS_VISION":   "true",
					"SUPPORTS_THINKING": "false",
					"SUPPORTS_TOOLS":    "true",
				},
			},
		},
		AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v", err)
	}

	rendered := string(plan.Compose)
	for _, want := range []string{
		"METIS_LLM_0_SUPPORTS_VISION: \"true\"",
		"METIS_LLM_0_SUPPORTS_THINKING: \"false\"",
		"METIS_LLM_0_SUPPORTS_TOOLS: \"true\"",
		"METIS_LLM_0_CONTEXT_WINDOW: \"128000\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered compose missing trait env %q:\n%s", want, rendered)
		}
	}
}

// TestPlanInstallAllowsUnboundOptionalModelSlot 验证可选模型插槽未绑定时正常安装且不注入空环境变量。
func TestPlanInstallAllowsUnboundOptionalModelSlot(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-multi-model-a7x2m
version: 1.0.0
display_name: Demo Multi Model
type: web
arch: [amd64]
dependencies: []
models:
  llm.0:
    interface: openai.chat.completions
  llm.1:
    interface: openai.chat.completions
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    environment:
      OPTIONAL_MODEL:
        value: "${METIS_LLM_1_MODEL}"
    capabilities:
      model-gateway:
        slots: [llm.0, llm.1]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
`)
	})

	plan, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-multi-model",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		// 仅绑定必选主槽 llm.0，未绑定可选备选槽 llm.1
		ModelBindings: map[string]contract.ModelSlotBinding{
			"llm.0": {
				Endpoint: "http://gateway/v1",
				Model:    "gpt-4o",
				APIKey:   "sk-test",
			},
		},
		AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err != nil {
		t.Fatalf("PlanInstall() error = %v, want success when optional slot is unbound", err)
	}

	rendered := string(plan.Compose)
	if !strings.Contains(rendered, "METIS_LLM_0_ENDPOINT: http://gateway/v1") {
		t.Fatalf("rendered compose missing llm.0 env:\n%s", rendered)
	}
	// llm.1 未绑定，不应该被注入到容器环境
	if strings.Contains(rendered, "METIS_LLM_1_ENDPOINT") {
		t.Fatalf("rendered compose should not inject METIS_LLM_1_ENDPOINT:\n%s", rendered)
	}
	// Compose 中的显式占位符展开为空字符串
	if !strings.Contains(rendered, "OPTIONAL_MODEL: \"\"") && !strings.Contains(rendered, "OPTIONAL_MODEL: ''") {
		t.Fatalf("rendered compose should expand unbound placeholder to empty string:\n%s", rendered)
	}
}

// TestPlanInstallRejectsUnboundRequiredModelSlot 验证必选插槽缺失时明确阻断安装。
func TestPlanInstallRejectsUnboundRequiredModelSlot(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-required-slot-a7x2m
version: 1.0.0
display_name: Demo Required
type: web
arch: [amd64]
dependencies: []
models:
  llm.0:
    interface: openai.chat.completions
  llm.1:
    interface: openai.chat.completions
    required: true
services:
  web:
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    capabilities:
      model-gateway:
        slots: [llm.0, llm.1]
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
`)
	})

	// 测试 1：主槽 llm.0 缺失
	_, err := contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-required",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		ModelBindings: map[string]contract.ModelSlotBinding{
			"llm.1": {Endpoint: "http://gw", Model: "m", APIKey: "k"},
		},
		AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "required model slot llm.0 is not bound") {
		t.Fatalf("PlanInstall() error = %v, want required model slot llm.0 failure", err)
	}

	// 测试 2：显式声明 required: true 的 llm.1 缺失
	_, err = contract.PlanInstall(bytes.NewReader(data), contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/demo-required",
		PublicHost:   "metis.internal",
		MasterIP:     "10.0.0.10",
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://metis.internal",
			"METIS_APP_TOKEN":         "token",
		},
		ModelBindings: map[string]contract.ModelSlotBinding{
			"llm.0": {Endpoint: "http://gw", Model: "m", APIKey: "k"},
		},
		AssignPort: func(string) (int, error) { return 19081, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "required model slot llm.1 is not bound") {
		t.Fatalf("PlanInstall() error = %v, want required model slot llm.1 failure", err)
	}
}
