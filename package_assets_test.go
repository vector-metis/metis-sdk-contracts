package contract_test

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

func TestInspectPackageAssets(t *testing.T) {
	screenshot := pngFixture(t, 320, 180)
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = manifestWithScreenshots("screenshots/home.png")
		files["screenshots/home.png"] = screenshot
	})
	summary, err := validate(t, data)
	if err != nil {
		t.Fatalf("ValidateMPK() error = %v", err)
	}

	assets, err := contract.InspectPackageAssets(bytes.NewReader(data), summary.Manifest)
	if err != nil {
		t.Fatalf("InspectPackageAssets() error = %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("len(assets) = %d, want 3", len(assets))
	}
	if assets[0].Name != "icons/icon-64.png" || assets[0].MediaType != "image/png" {
		t.Fatalf("assets[0] = %+v", assets[0])
	}
	if assets[1].Name != "icons/icon-256.png" || assets[1].MediaType != "image/png" {
		t.Fatalf("assets[1] = %+v", assets[1])
	}
	if assets[2].Name != "screenshots/home.png" || !bytes.Equal(assets[2].Content, screenshot) {
		t.Fatalf("assets[2] = %+v", assets[2])
	}
}

func TestGateMPKRejectsInvalidAssets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
	}{
		{
			name: "missing 64 icon",
			mutate: func(files map[string][]byte) {
				delete(files, "icons/icon-64.png")
			},
		},
		{
			name: "wrong 256 icon dimensions",
			mutate: func(files map[string][]byte) {
				files["icons/icon-256.png"] = pngFixture(t, 32, 32)
			},
		},
		{
			name: "fake png icon",
			mutate: func(files map[string][]byte) {
				files["icons/icon-64.png"] = []byte("not a png")
			},
		},
		{
			name: "icon without alpha",
			mutate: func(files map[string][]byte) {
				var output bytes.Buffer
				if err := png.Encode(&output, image.NewGray(image.Rect(0, 0, 64, 64))); err != nil {
					t.Fatal(err)
				}
				files["icons/icon-64.png"] = output.Bytes()
			},
		},
		{
			name: "too many screenshots",
			mutate: func(files map[string][]byte) {
				names := make([]string, contract.MaxScreenshotCount+1)
				for index := range names {
					names[index] = fmt.Sprintf("screenshots/%d.png", index)
					files[names[index]] = pngFixture(t, 8, 8)
				}
				files["manifest.yaml"] = manifestWithScreenshots(names...)
			},
		},
		{
			name: "screenshot extension mismatch",
			mutate: func(files map[string][]byte) {
				files["manifest.yaml"] = manifestWithScreenshots("screenshots/home.webp")
				files["screenshots/home.webp"] = pngFixture(t, 8, 8)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := buildMPK(t, test.mutate)
			_, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{})
			if err == nil {
				t.Fatal("GateMPK() error = nil, want invalid asset error")
			}
			findings := contract.FindingsFromError(err)
			if len(findings) != 1 || findings[0].RuleID != "MPK-ASSET" {
				t.Fatalf("FindingsFromError() = %+v, want MPK-ASSET", findings)
			}
		})
	}
}

func manifestWithScreenshots(names ...string) []byte {
	manifest := `schema_version: 1
id: demo-a7x2m
version: 1.0.0
display_name: Demo
arch: [amd64]
dependencies: []
type: web
services:
  web:
    endpoints:
      - {name: web, protocol: http, container_port: 8080}
screenshots:
`
	for _, name := range names {
		manifest += "  - " + name + "\n"
	}
	return []byte(manifest)
}
