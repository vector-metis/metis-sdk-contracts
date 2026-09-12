package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestWriteDeploymentPackageContainsOnlyRuntimeFiles(t *testing.T) {
	t.Parallel()

	packageData := deploymentMPK(t, []deploymentMPKEntry{
		{name: "program/amd64/start.sh", mode: 0o755, content: "#!/bin/sh\necho amd64\n"},
		{name: "program/arm64/start.sh", mode: 0o755, content: "#!/bin/sh\necho arm64\n"},
		{name: "images/amd64/web.tar", mode: 0o644, content: "docker image"},
	})
	var output bytes.Buffer
	err := contract.WriteDeploymentPackage(&output, contract.DeploymentPackage{
		Compose:      []byte("services:\n  web:\n    image: metis.internal/demo/web:1.0.0\n"),
		Environment:  map[string]string{"METIS_APP_ID": "demo", "EMPTY": ""},
		Overlay:      map[string][]byte{"etc/app.conf": []byte("enabled=true\n")},
		Package:      bytes.NewReader(packageData),
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	names := make([]string, 0)
	contents := make(map[string]string)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Typeflag == tar.TypeReg {
			body, readErr := io.ReadAll(tarReader)
			if readErr != nil {
				t.Fatal(readErr)
			}
			contents[header.Name] = string(body)
		}
	}
	for _, name := range []string{"program/", "config/", "data/", "log/", "tmp/", "overlay/", "compose.yaml", ".env", "program/start.sh", "overlay/etc/app.conf"} {
		if !slices.Contains(names, name) {
			t.Fatalf("deployment entries = %#v, missing %q", names, name)
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "images/") || strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".mpk") {
			t.Fatalf("deployment contains forbidden entry %q", name)
		}
	}
	if contents["overlay/etc/app.conf"] != "enabled=true\n" || !strings.Contains(contents[".env"], "METIS_APP_ID=demo\n") {
		t.Fatalf("deployment contents = %#v", contents)
	}
	if contents["program/start.sh"] != "#!/bin/sh\necho amd64\n" || strings.Contains(strings.Join(names, "\n"), "arm64") {
		t.Fatalf("deployment architecture contents = %#v", contents)
	}
}

func TestWriteDeploymentPackageRejectsUnsafeOverlayPath(t *testing.T) {
	t.Parallel()

	err := contract.WriteDeploymentPackage(io.Discard, contract.DeploymentPackage{
		Compose: []byte("services: {}\n"), Environment: map[string]string{},
		Overlay: map[string][]byte{"../outside": []byte("bad")},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe overlay") {
		t.Fatalf("WriteDeploymentPackage() error = %v, want unsafe overlay failure", err)
	}
}

type deploymentMPKEntry struct {
	name    string
	mode    int64
	content string
}

func deploymentMPK(t *testing.T, entries []deploymentMPKEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarWriter, entry.content); err != nil {
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
