package contract_test

import (
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
	"gopkg.in/yaml.v3"
)

// TestValidateComposeOverride 固化管理员模板的最小结构契约；模板不是完整 Compose 文档。
func TestValidateComposeOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "service mapping", body: "deploy:\n  resources:\n    limits:\n      memory: 4GiB\n"},
		{name: "empty document", body: "", wantErr: "non-empty mapping"},
		{name: "empty mapping", body: "{}\n", wantErr: "non-empty mapping"},
		{name: "sequence root", body: "- command\n", wantErr: "root must be"},
		{name: "services wrapper", body: "services:\n  app:\n    command: run\n", wantErr: "must not contain services"},
		{name: "multiple documents", body: "command: first\n---\ncommand: second\n", wantErr: "one YAML document"},
		{name: "custom tag", body: "ports: !override []\n", wantErr: "custom YAML tag"},
		{name: "non-string key", body: "? [one, two]\n: value\n", wantErr: "mapping keys must be strings"},
		{name: "too large", body: "command: " + strings.Repeat("x", contract.MaxComposeOverrideBodyBytes), wantErr: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := contract.ValidateComposeOverride(test.body)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateComposeOverride() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateComposeOverride() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// TestPlanPreparedInstallAppliesComposeOverridesLast 证明模板能覆盖平台刚生成的字段，
// 并固定 map 递归、列表整体替换、null 明确覆盖和后模板优先的顺序。
func TestPlanPreparedInstallAppliesComposeOverridesLast(t *testing.T) {
	t.Parallel()

	metadata := preparedOverrideMetadata()
	plan, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/override-app-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ImageReferences: map[string]string{
			"override-app-a7x2m/web:1.0.0": "platform.example.invalid/override-app-a7x2m/web:1.0.0",
		},
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
		ComposeOverrides: map[string][]contract.ComposeOverride{
			"web": {
				{ID: 10, Name: "gpu", Body: `command: [nvidia-smi]
deploy:
  resources:
    reservations:
      devices:
        - driver: nvidia
          count: 1
          capabilities: [gpu]
ports:
  - target: 9000
    published: "29000"
    protocol: tcp
environment:
  CUSTOM: first
`},
				{ID: 11, Name: "site", Body: `command: [serve]
deploy:
  resources:
    reservations:
      devices: []
environment:
  CUSTOM: second
extra_hosts: null
`},
			},
		},
	})
	if err != nil {
		t.Fatalf("PlanPreparedInstall() error = %v", err)
	}

	var document struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(plan.Compose, &document); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	web := document.Services["web"]
	if got := web["image"]; got != "platform.example.invalid/override-app-a7x2m/web:1.0.0" {
		t.Fatalf("image = %#v", got)
	}
	command, ok := web["command"].([]any)
	if !ok || len(command) != 1 || command[0] != "serve" {
		t.Fatalf("command = %#v", web["command"])
	}
	ports, ok := web["ports"].([]any)
	if !ok || len(ports) != 1 {
		t.Fatalf("ports = %#v", web["ports"])
	}
	port, ok := ports[0].(map[string]any)
	if !ok || port["target"] != 9000 || port["published"] != "29000" {
		t.Fatalf("port = %#v", ports[0])
	}
	deploy := web["deploy"].(map[string]any)
	resources := deploy["resources"].(map[string]any)
	if limits := resources["limits"].(map[string]any); limits["memory"] != "1GiB" {
		t.Fatalf("limits = %#v", limits)
	}
	reservations := resources["reservations"].(map[string]any)
	if devices := reservations["devices"].([]any); len(devices) != 0 {
		t.Fatalf("devices = %#v", devices)
	}
	environment := web["environment"].(map[string]any)
	if environment["METIS_APP_ID"] != "override-app-a7x2m" || environment["CUSTOM"] != "second" {
		t.Fatalf("environment = %#v", environment)
	}
	if value, exists := web["extra_hosts"]; !exists || value != nil {
		t.Fatalf("extra_hosts = %#v, exists = %t", value, exists)
	}
}

// TestPlanPreparedInstallRequiresDeclaredComposeOverride 保证非标能力不会在未绑定模板时静默启动。
func TestPlanPreparedInstallRequiresDeclaredComposeOverride(t *testing.T) {
	t.Parallel()

	metadata := preparedOverrideMetadata()
	_, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
		Architecture: contract.ArchAMD64,
		BaseDir:      "/var/lib/metis/apps/override-app-a7x2m",
		PublicHost:   "platform.example.invalid",
		MasterIP:     "10.0.0.10",
		ImageReferences: map[string]string{
			"override-app-a7x2m/web:1.0.0": "platform.example.invalid/override-app-a7x2m/web:1.0.0",
		},
		ExtraEnvironment: map[string]string{
			"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
			"METIS_APP_TOKEN":         "token",
		},
		AssignPort: func(string) (int, error) { return 22001, nil },
	})
	if err == nil || !strings.Contains(err.Error(), `service "web" requires at least one Compose override`) {
		t.Fatalf("PlanPreparedInstall() error = %v", err)
	}
}

// TestPlanPreparedInstallRejectsInvalidOverrideBindings 固化未知 service 和重复模板的调用错误。
func TestPlanPreparedInstallRejectsInvalidOverrideBindings(t *testing.T) {
	t.Parallel()

	metadata := preparedOverrideMetadata()
	tests := []struct {
		name      string
		overrides map[string][]contract.ComposeOverride
		want      string
	}{
		{
			name: "unknown service",
			overrides: map[string][]contract.ComposeOverride{
				"web":    {{ID: 9, Name: "required", Body: "command: run\n"}},
				"worker": {{ID: 10, Name: "template", Body: "command: run\n"}},
			},
			want: `unknown service "worker"`,
		},
		{
			name: "duplicate template",
			overrides: map[string][]contract.ComposeOverride{
				"web": {
					{ID: 10, Name: "template", Body: "command: first\n"},
					{ID: 10, Name: "template", Body: "command: second\n"},
				},
			},
			want: "duplicate Compose override template 10",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := contract.PlanPreparedInstall(metadata, contract.InstallOptions{
				Architecture: contract.ArchAMD64,
				BaseDir:      "/var/lib/metis/apps/override-app-a7x2m",
				PublicHost:   "platform.example.invalid",
				MasterIP:     "10.0.0.10",
				ImageReferences: map[string]string{
					"override-app-a7x2m/web:1.0.0": "platform.example.invalid/override-app-a7x2m/web:1.0.0",
				},
				ExtraEnvironment: map[string]string{
					"METIS_PLATFORM_ENDPOINT": "http://platform.example.invalid",
					"METIS_APP_TOKEN":         "token",
				},
				AssignPort:       func(string) (int, error) { return 22001, nil },
				ComposeOverrides: test.overrides,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("PlanPreparedInstall() error = %v, want %q", err, test.want)
			}
		})
	}
}

func preparedOverrideMetadata() contract.PackageMetadata {
	return contract.PackageMetadata{
		Manifest: contract.Manifest{
			SchemaVersion: 1,
			ID:            "override-app-a7x2m",
			Version:       "1.0.0",
			DisplayName:   "Override",
			Type:          contract.ApplicationTypeWeb,
			Architectures: []string{contract.ArchAMD64},
			Dependencies:  []contract.Dependency{},
			Services: map[string]contract.ManifestService{
				"web": {
					Endpoints: []contract.ServiceEndpoint{{
						Name: "web", Service: "web", Protocol: contract.EndpointProtocolHTTP, ContainerPort: 8080,
					}},
					Capabilities: map[string]contract.CapabilityRequest{"compose-override": {}},
				},
			},
		},
		Settings: []contract.Setting{},
		Compose: map[string]string{contract.ArchAMD64: `services:
  web:
    image: override-app-a7x2m/web:1.0.0
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 1GiB
`},
	}
}
