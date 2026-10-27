package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestInspectPackageMetadataCollectsProvidedSkillFacts(t *testing.T) {
	entries := []metadataEntry{
		{name: "manifest.yaml", content: `schema_version: 1
id: skill-app-a7x2m
version: 1.0.0
display_name: Skill app
arch: [amd64]
dependencies: []
type: web
provides:
  mcp:
    - name: crm
      description: Query CRM records
      endpoint: web
      path: /mcp
  skills: [crm-followup]
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints:
      - {name: web, protocol: http, container_port: 8080}
`},
		{name: "compose.amd64.yaml", content: "services:\n  web:\n    image: skill-app-a7x2m/web:1.0.0\n"},
		// Directory entries are part of a normal tar archive and must not be
		// mistaken for missing Skill files.
		{name: "skills", typeflag: tar.TypeDir},
		{name: "skills/crm-followup", typeflag: tar.TypeDir},
		{name: "skills/crm-followup/SKILL.md", content: "---\nname: crm-followup\ndescription: Follow up CRM leads\n---\n\nUse the CRM MCP.\n"},
		{name: "skills/crm-followup/examples.md", content: "# Examples\n"},
	}
	metadata, err := contract.InspectPackageMetadata(bytes.NewReader(metadataPackageEntries(t, entries)), "skill-app-a7x2m", "1.0.0")
	if err != nil {
		t.Fatalf("InspectPackageMetadata() error = %v", err)
	}
	if len(metadata.SkillFiles) != 2 {
		t.Fatalf("SkillFiles length = %d, want 2", len(metadata.SkillFiles))
	}
	want := sha256.Sum256([]byte("# Examples\n"))
	if metadata.SkillFiles[0].Path != "skills/crm-followup/SKILL.md" || metadata.SkillFiles[1].Path != "skills/crm-followup/examples.md" {
		t.Fatalf("SkillFiles order = %#v", metadata.SkillFiles)
	}
	if metadata.SkillFiles[1].SHA256 != hex.EncodeToString(want[:]) || metadata.SkillFiles[1].Size != int64(len("# Examples\n")) {
		t.Fatalf("examples metadata = %#v", metadata.SkillFiles[1])
	}
	if len(metadata.Manifest.Provides.MCP) != 1 || metadata.Manifest.Provides.MCP[0].Name != "crm" {
		t.Fatalf("provides.mcp = %#v", metadata.Manifest.Provides.MCP)
	}
}

func TestInspectPackageMetadataRejectsUndeclaredSkillDirectory(t *testing.T) {
	entries := []metadataEntry{
		{name: "manifest.yaml", content: `schema_version: 1
id: skill-app-a7x2m
version: 1.0.0
display_name: Skill app
arch: [amd64]
dependencies: []
type: web
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`},
		{name: "compose.amd64.yaml", content: "services:\n  web:\n    image: skill-app-a7x2m/web:1.0.0\n"},
		{name: "skills", typeflag: tar.TypeDir},
		{name: "skills/undeclared", typeflag: tar.TypeDir},
		{name: "skills/undeclared/SKILL.md", content: "---\nname: undeclared\ndescription: Not declared\n---\n"},
	}
	_, err := contract.InspectPackageMetadata(bytes.NewReader(metadataPackageEntries(t, entries)), "skill-app-a7x2m", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("InspectPackageMetadata() error = %v, want undeclared Skill rejection", err)
	}
}

func TestInspectPackageMetadataRejectsInvalidMCPProvider(t *testing.T) {
	packageData := metadataPackage(t, map[string]string{
		"manifest.yaml": `schema_version: 1
id: mcp-app-a7x2m
version: 1.0.0
display_name: MCP app
arch: [amd64]
dependencies: []
type: web
provides:
  mcp:
    - {name: CRM, description: invalid name, endpoint: web, path: /mcp}
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`,
		"compose.amd64.yaml": "services:\n  web:\n    image: mcp-app-a7x2m/web:1.0.0\n",
	})
	_, err := contract.InspectPackageMetadata(bytes.NewReader(packageData), "mcp-app-a7x2m", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("InspectPackageMetadata() error = %v, want provides gate error", err)
	}
}

func TestBuildPackagePreservesProvidedSkillFiles(t *testing.T) {
	// This is a small end-to-end fixture check: the package builder must not
	// discard Skill paths before the metadata gate sees them.
	files := map[string][]byte{
		"manifest.yaml": []byte(`schema_version: 1
id: skill-build-a7x2m
version: 1.0.0
display_name: Skill build
arch: [amd64]
dependencies: []
type: web
provides:
  skills: [demo]
services:
  web:
    lifecycle: {restart: unless-stopped}
    endpoints: [{name: web, protocol: http, container_port: 8080}]
`),
		"compose.amd64.yaml":   []byte("services:\n  web:\n    image: skill-build-a7x2m/web:1.0.0\n"),
		"skills/demo/SKILL.md": []byte("---\nname: demo\ndescription: Demo\n---\n"),
	}
	packageData := tarGzipFiles(t, files)
	metadata, err := contract.InspectPackageMetadata(bytes.NewReader(packageData), "skill-build-a7x2m", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.SkillFiles) != 1 {
		t.Fatalf("SkillFiles = %#v", metadata.SkillFiles)
	}
}

func tarGzipFiles(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
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
	return output.Bytes()
}
