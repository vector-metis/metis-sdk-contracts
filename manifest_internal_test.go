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
			"web": {Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
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
			"web": {Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			manifest.Models = test.models
			manifest.Services["web"] = ManifestService{
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
			"web": {Endpoints: []ServiceEndpoint{{Name: "web", Service: "web", Protocol: EndpointProtocolHTTP, ContainerPort: 8080}}},
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
