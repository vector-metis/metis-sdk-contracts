package contract

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestManifestRejectsAmbiguousDependencySelectors 验证包上传前即可拒绝不确定的 Runtime 选择器。
func TestManifestRejectsAmbiguousDependencySelectors(t *testing.T) {
	base := Manifest{
		SchemaVersion: 1, ID: "source-app-a7x2m", Version: "1.0.0", DisplayName: "Source", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Capabilities: []string{}, Services: map[string]ManifestService{
			"web": {Lifecycle: ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
		},
	}
	for _, test := range []struct {
		name         string
		dependencies []Dependency
		want         string
	}{
		{
			name: "duplicate app id",
			dependencies: []Dependency{
				{ID: "target-app-a7x2m", Alias: "first", Version: "1.0.0"},
				{ID: "target-app-a7x2m", Alias: "second", Version: "1.0.0"},
			},
			want: "duplicate dependency id",
		},
		{
			name: "alias conflicts with app id",
			dependencies: []Dependency{
				{ID: "target-app-a7x2m", Alias: "data", Version: "1.0.0"},
				{ID: "data", Alias: "other", Version: "1.0.0"},
			},
			want: "conflicts with another dependency id",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			manifest.Dependencies = test.dependencies
			if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateIdentity() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestManifestYAMLRejectsUnknownFieldsAndUnusedDeclarations(t *testing.T) {
	unknown := []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    typo: true
`)
	var manifest Manifest
	if err := yaml.Unmarshal(unknown, &manifest); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("yaml.Unmarshal() error = %v, want unknown field", err)
	}

	unused := []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
settings:
  unused: {label: Unused, type: string}
models:
  llm.0: {interface: openai.chat.completions}
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
    capabilities: {model-gateway: {slots: [llm.0]}}
`)
	if err := yaml.Unmarshal(unused, &manifest); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), "declared but not used") {
		t.Fatalf("validateIdentity() error = %v, want unused declaration", err)
	}
}

func TestManifestRejectsInvalidModelCapabilityDeclaration(t *testing.T) {
	base := Manifest{
		SchemaVersion: 1, ID: "model-app-a7x2m", Version: "1.0.0", DisplayName: "Model", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Dependencies: []Dependency{},
		Services: map[string]ManifestService{
			"web": {Lifecycle: ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
		},
	}
	tests := []struct {
		name    string
		models  map[string]ModelSlot
		request map[string]CapabilityRequest
		want    string
	}{
		{name: "invalid slot", models: map[string]ModelSlot{"chat": {Interface: "openai.chat.completions"}}, want: "invalid model slot"},
		{name: "missing interface", models: map[string]ModelSlot{"llm.0": {}}, want: "interface is required"},
		{name: "unsupported interface", models: map[string]ModelSlot{"embedding.0": {Interface: "embeddings"}}, want: "does not support interface"},
		{name: "empty capability", models: map[string]ModelSlot{"llm.0": {Interface: "openai.chat.completions"}}, request: map[string]CapabilityRequest{"model-gateway": {}}, want: "requires at least one slot"},
		{name: "unsupported trait", models: map[string]ModelSlot{"llm.0": {Interface: "openai.chat.completions", Traits: []string{"invalid_trait"}}}, request: map[string]CapabilityRequest{"model-gateway": {Slots: []string{"llm.0"}}}, want: "unsupported trait"},
		{name: "duplicate trait", models: map[string]ModelSlot{"llm.0": {Interface: "openai.chat.completions", Traits: []string{"vision", "vision"}}}, request: map[string]CapabilityRequest{"model-gateway": {Slots: []string{"llm.0"}}}, want: "duplicate trait"},
		{name: "traits on embedding", models: map[string]ModelSlot{"embedding.0": {Interface: "openai.embeddings", Traits: []string{"vision"}}}, request: map[string]CapabilityRequest{"model-gateway": {Slots: []string{"embedding.0"}}}, want: "does not support traits"},
		{name: "negative dimensions", models: map[string]ModelSlot{"embedding.0": {Interface: "openai.embeddings", Dimensions: -1}}, request: map[string]CapabilityRequest{"model-gateway": {Slots: []string{"embedding.0"}}}, want: "dimensions must be greater than 0"},
		{name: "dimensions on llm", models: map[string]ModelSlot{"llm.0": {Interface: "openai.chat.completions", Dimensions: 1536}}, request: map[string]CapabilityRequest{"model-gateway": {Slots: []string{"llm.0"}}}, want: "does not support dimensions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			manifest.Models = test.models
			manifest.Services["web"] = ManifestService{
				Lifecycle:    ServiceLifecycle{Restart: "unless-stopped"},
				Endpoints:    []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}},
				Capabilities: test.request,
			}
			if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateIdentity() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestManifestRejectsReservedAndDuplicateMountTargets(t *testing.T) {
	base := Manifest{
		SchemaVersion: 1, ID: "mount-app-a7x2m", Version: "1.0.0", DisplayName: "Mount", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Dependencies: []Dependency{},
		Services: map[string]ManifestService{
			"web": {Lifecycle: ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
		},
	}
	tests := []struct {
		name   string
		mounts []Mount
		want   string
	}{
		{name: "proc subtree", mounts: []Mount{{Source: "data", Target: "/proc/sys"}}, want: "reserved"},
		{name: "normalized sys", mounts: []Mount{{Source: "data", Target: "/tmp/../sys/kernel"}}, want: "reserved"},
		{name: "dev root", mounts: []Mount{{Source: "data", Target: "/dev"}}, want: "reserved"},
		{name: "docker socket", mounts: []Mount{{Source: "data", Target: "/var/run/docker.sock"}}, want: "reserved"},
		{name: "normalized duplicate", mounts: []Mount{
			{Source: "data", Target: "/var/lib/app"},
			{Source: "config", Target: "/var/lib/app/../app"},
		}, want: "duplicate mount target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			service := manifest.Services["web"]
			service.Mounts = test.mounts
			manifest.Services = map[string]ManifestService{"web": service}
			if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateIdentity() error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestManifestRejectsNonCanonicalOverlaySources 固化 managed mount subpath 的严格路径契约。
func TestManifestRejectsNonCanonicalOverlaySources(t *testing.T) {
	base := Manifest{
		SchemaVersion: 1, ID: "overlay-path-app-a7x2m", Version: "1.0.0", DisplayName: "Overlay Path", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Dependencies: []Dependency{},
		Services: map[string]ManifestService{
			"web": {Lifecycle: ServiceLifecycle{Restart: "unless-stopped"}, Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
		},
	}
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "legacy root file", source: "./config.yaml"},
		{name: "parent escape", source: "./overlay/../config.yaml"},
		{name: "double slash", source: "./overlay//config.yaml"},
		{name: "current directory segment", source: "./overlay/./config.yaml"},
		{name: "trailing slash", source: "./overlay/config.yaml/"},
		{name: "absolute path", source: "/var/lib/app/config.yaml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			service := manifest.Services["web"]
			service.Mounts = []Mount{{Source: test.source, Target: "/etc/app/config.yaml", ReadOnly: true}}
			manifest.Services = map[string]ManifestService{"web": service}
			if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), "invalid mount source") {
				t.Fatalf("validateIdentity() error = %v, want invalid mount source for %q", err, test.source)
			}
		})
	}
}

// TestManifestAcceptsCanonicalOverlayRootAndNestedSources 固化文件、子目录和 overlay 根目录均可声明。
func TestManifestAcceptsCanonicalOverlayRootAndNestedSources(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1, ID: "overlay-root-app-a7x2m", Version: "1.0.0", DisplayName: "Overlay Root", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Dependencies: []Dependency{},
		Services: map[string]ManifestService{
			"web": {
				Lifecycle: ServiceLifecycle{Restart: "unless-stopped"},
				Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}},
				Mounts: []Mount{
					{Source: "overlay", Target: "/etc/app", ReadOnly: true},
					{Source: "overlay", Subpath: "static", Target: "/usr/share/app", ReadOnly: true},
					{Source: "overlay", Subpath: "static/index.html", Target: "/var/lib/app/index.html", ReadOnly: true},
				},
			},
		},
	}
	if err := manifest.validateIdentity(); err != nil {
		t.Fatalf("validateIdentity() error = %v", err)
	}
}

// TestManifestRejectsRemovedDirectoryPlaceholders 固化旧宿主目录变量不能从环境值回流到容器。
func TestManifestRejectsRemovedDirectoryPlaceholders(t *testing.T) {
	value := "${METIS_DIR_CONFIG}/app.yaml"
	manifest := Manifest{
		SchemaVersion: 1, ID: "removed-dir-placeholder-a7x2m", Version: "1.0.0", DisplayName: "Removed Directory Placeholder", Type: ApplicationTypeWeb,
		Architectures: []string{ArchAMD64}, Dependencies: []Dependency{},
		Services: map[string]ManifestService{
			"web": {
				Lifecycle: ServiceLifecycle{Restart: "unless-stopped"},
				Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}},
				Environment: map[string]EnvironmentSource{
					"APP_CONFIG": {Value: &value},
				},
			},
		},
	}
	if err := manifest.validateIdentity(); err == nil || !strings.Contains(err.Error(), "removed directory placeholder") {
		t.Fatalf("validateIdentity() error = %v, want removed directory placeholder rejection", err)
	}
}

func TestModelSlotTraitsAndRequirements(t *testing.T) {
	manifestYAML := `
schema_version: 1
id: test-models-app
version: 1.0.0
display_name: Test Models
type: web
arch: [amd64]
dependencies: []
models:
  llm.0:
    interface: openai.chat.completions
    traits: [vision, tools]
  llm.1:
    interface: openai.chat.completions
    traits: [thinking]
    required: true
  llm.2:
    interface: openai.chat.completions
    required: false
  embedding.0:
    interface: openai.embeddings
    dimensions: 1536
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints:
      - name: web
        service: web
        protocol: http
        container_port: 8080
    capabilities:
      model-gateway:
        slots: [llm.0, llm.1, llm.2, embedding.0]
`
	var manifest Manifest
	if err := yaml.Unmarshal([]byte(manifestYAML), &manifest); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if err := manifest.validateIdentity(); err != nil {
		t.Fatalf("validateIdentity() error = %v", err)
	}

	slot0 := manifest.Models["llm.0"]
	if !IsSlotRequired("llm.0", slot0) {
		t.Fatalf("llm.0 should default to required")
	}
	if len(slot0.Traits) != 2 || slot0.Traits[0] != "vision" || slot0.Traits[1] != "tools" {
		t.Fatalf("llm.0 traits = %#v, want [vision, tools]", slot0.Traits)
	}

	slot1 := manifest.Models["llm.1"]
	if !IsSlotRequired("llm.1", slot1) {
		t.Fatalf("llm.1 should be required via explicit setting")
	}
	if len(slot1.Traits) != 1 || slot1.Traits[0] != "thinking" {
		t.Fatalf("llm.1 traits = %#v, want [thinking]", slot1.Traits)
	}

	slot2 := manifest.Models["llm.2"]
	if IsSlotRequired("llm.2", slot2) {
		t.Fatalf("llm.2 should not be required")
	}

	emb0 := manifest.Models["embedding.0"]
	if !IsSlotRequired("embedding.0", emb0) {
		t.Fatalf("embedding.0 should default to required")
	}
	if emb0.Dimensions != 1536 {
		t.Fatalf("embedding.0 dimensions = %d, want 1536", emb0.Dimensions)
	}
}
