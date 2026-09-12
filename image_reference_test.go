package contract_test

import (
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestTargetImageReferenceUsesRepositoryBasename(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{name: "application image", source: "qa-market-1-wpsjl/web:1.0.0", expected: "metis.internal/qa-market-1-wpsjl/web:1.0.0"},
		{name: "docker hub image", source: "docker.io/library/postgres:16", expected: "metis.internal/qa-market-1-wpsjl/postgres:16"},
		{name: "source registry with port", source: "source.internal:5000/team/api:v2", expected: "metis.internal/qa-market-1-wpsjl/api:v2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := contract.TargetImageReference("metis.internal", "qa-market-1-wpsjl", test.source)
			if err != nil {
				t.Fatal(err)
			}
			if target != test.expected {
				t.Fatalf("TargetImageReference() = %q, want %q", target, test.expected)
			}
		})
	}
}

func TestTargetImageReferenceRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		appID  string
		source string
	}{
		{name: "host contains scheme", host: "http://metis.internal", appID: "demo-a7x2m", source: "demo/web:1.0.0"},
		{name: "unsafe application id", host: "metis.internal", appID: "../demo", source: "demo/web:1.0.0"},
		{name: "source lacks tag", host: "metis.internal", appID: "demo-a7x2m", source: "demo/web"},
		{name: "source uses digest", host: "metis.internal", appID: "demo-a7x2m", source: "demo/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := contract.TargetImageReference(test.host, test.appID, test.source); err == nil {
				t.Fatal("TargetImageReference() = nil error, want invalid input")
			}
		})
	}
}

func TestDecideImageImport(t *testing.T) {
	tests := []struct {
		name           string
		existingDigest string
		incomingDigest string
		want           contract.ImageImportDecision
		wantError      string
	}{
		{
			name:           "target tag does not exist",
			incomingDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want:           contract.ImageImportPublish,
		},
		{
			name:           "same digest reuses target tag",
			existingDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			incomingDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want:           contract.ImageImportReuse,
		},
		{
			name:           "different digest rejects target tag",
			existingDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			incomingDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			wantError:      "already points to digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := contract.DecideImageImport(
				"metis.internal/demo-a7x2m/web:1.0.0",
				test.existingDigest,
				test.incomingDigest,
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("DecideImageImport() error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if decision != test.want {
				t.Fatalf("DecideImageImport() = %v, want %v", decision, test.want)
			}
		})
	}
}

func TestValidateMPKRejectsTargetImageCollision(t *testing.T) {
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
  worker: {}
`)
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: first.example/team/web:1.0.0
  worker:
    image: second.example/other/web:1.0.0
`)
		files["images/amd64/app.tar"] = dockerArchive(t, "first.example/team/web:1.0.0")
		files["images/amd64/worker.tar"] = dockerArchive(t, "second.example/other/web:1.0.0")
	})

	_, err := validate(t, data)
	if err == nil || !strings.Contains(err.Error(), "same target image") {
		t.Fatalf("error = %v, want target collision", err)
	}
}
