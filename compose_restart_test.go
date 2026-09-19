package contract_test

import (
	"bytes"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// TestValidateMPKRejectsComposeLifecycle 防止应用绕过 manifest lifecycle 直接控制 Compose。
func TestValidateMPKRequiresResidentRestartPolicy(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["__preserve_compose_lifecycle"] = nil
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: always
`)
	})
	_, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"})
	if err == nil || !strings.Contains(err.Error(), "cannot declare restart") {
		t.Fatalf("ValidateMPK() error = %v, want source Compose lifecycle rejection", err)
	}
}

// TestValidateMPKAllowsManifestOneshot 固化一次性 service 的生命周期由 manifest 声明。
func TestValidateMPKAllowsOneshotWithoutRestartPolicy(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["__preserve_compose_lifecycle"] = nil
		files["manifest.yaml"] = []byte(`schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
type: web
arch: [amd64]
dependencies: []
services:
  web:
    lifecycle:
      oneshot: true
      required: true
    endpoints:
      - {name: web, protocol: http, container_port: 8080}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
`)
	})
	if _, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"}); err != nil {
		t.Fatalf("ValidateMPK() error = %v, want oneshot accepted", err)
	}
}

// TestValidateMPKRejectsComposeXMetis 防止旧的 Compose 扩展继续成为策略来源。
func TestValidateMPKRejectsResidentRestartForOneshot(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["__preserve_compose_lifecycle"] = nil
		files["manifest.yaml"] = []byte(`schema_version: 1
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
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
    x-metis:
      oneshot: true
`)
	})
	_, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"})
	if err == nil || !strings.Contains(err.Error(), "cannot declare restart") {
		t.Fatalf("ValidateMPK() error = %v, want source Compose lifecycle rejection", err)
	}
}
