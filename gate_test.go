package contract_test

import (
	"bytes"
	"strings"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// TestGateMPKUsesOneResultShapeAcrossConsumers 固化 CLI、Store 和平台准备阶段共用的门禁入口：
// 同一份包的成功摘要和失败 rule_id 不依赖调用方如何包装错误。
func TestGateMPKUsesOneResultShapeAcrossConsumers(t *testing.T) {
	data := buildMPK(t, nil)
	result, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{
		ValidateOptions: contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"},
	})
	if err != nil {
		t.Fatalf("GateMPK() error = %v", err)
	}
	if result.Metadata.Manifest.ID != "demo-a7x2m" || result.Size != int64(len(data)) || result.SHA256 == "" || len(result.Images) != 1 {
		t.Fatalf("GateMPK() result = %+v", result)
	}

	invalid := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte("services:\n  web:\n    image: demo-a7x2m/web:1.0.0\n    restart: unless-stopped\n    ports: [\"8080:8080\"]\n")
	})
	_, err = contract.GateMPK(bytes.NewReader(invalid), contract.GateOptions{})
	if err == nil {
		t.Fatal("GateMPK() = nil, want compose ports rejection")
	}
	findings := contract.FindingsFromError(err)
	if len(findings) != 1 || findings[0].RuleID != "MPK-COMPOSE-PORTS" {
		t.Fatalf("FindingsFromError() = %+v, want MPK-COMPOSE-PORTS", findings)
	}
	if findings[0].Path != "compose.amd64.yaml" || findings[0].Context != "service=web" {
		t.Fatalf("finding location = %+v", findings[0])
	}
	if !strings.Contains(err.Error(), "MPK-COMPOSE-PORTS") {
		t.Fatalf("GateMPK() error = %q, want stable rule id", err)
	}
}

// TestGateMPKAlwaysValidatesPublicAssets 固化完整 MPK 的四个入口不能选择性跳过图标和截图门禁。
func TestGateMPKAlwaysValidatesPublicAssets(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		delete(files, "icons/icon-64.png")
	})
	_, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{})
	if err == nil {
		t.Fatal("GateMPK() = nil, want missing icon rejection")
	}
	findings := contract.FindingsFromError(err)
	if len(findings) != 1 || findings[0].RuleID != "MPK-ASSET" {
		t.Fatalf("FindingsFromError() = %+v, want MPK-ASSET", findings)
	}
	if findings[0].Path != "icons/icon-64.png" {
		t.Fatalf("finding path = %q, want icons/icon-64.png", findings[0].Path)
	}
}

func TestGateMPKHonorsConfiguredCompressedAndExpandedLimits(t *testing.T) {
	data := buildMPK(t, nil)
	if _, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{
		ValidateOptions: contract.ValidateOptions{MaxPackageSize: int64(len(data) - 1)},
	}); err == nil || contract.FindingsFromError(err)[0].RuleID != "MPK-SIZE" {
		t.Fatalf("GateMPK(compressed limit) error = %v, want MPK-SIZE", err)
	}

	expanded := buildMPK(t, func(files map[string][]byte) {
		for _, name := range []string{"program/a.txt", "program/b.txt", "program/c.txt"} {
			files[name] = []byte(strings.Repeat("x", 800<<10))
		}
	})
	if int64(len(expanded)) >= 1<<20 {
		t.Fatalf("fixture compressed size = %d, want below configured limit", len(expanded))
	}
	if _, err := contract.GateMPK(bytes.NewReader(expanded), contract.GateOptions{
		ValidateOptions: contract.ValidateOptions{MaxPackageSize: 1 << 20, ContractOnly: true},
	}); err == nil || !strings.Contains(err.Error(), "expanded package size exceeds 2097152") {
		t.Fatalf("GateMPK(expanded limit) error = %v", err)
	}
}

func TestGateMPKContractOnlySkipsImageBody(t *testing.T) {
	data := buildContractFixture(t, "basic-app-a7x2m")
	result, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{
		ValidateOptions: contract.ValidateOptions{ExpectedAppID: "basic-app-a7x2m", ContractOnly: true},
	})
	if err != nil {
		t.Fatalf("GateMPK(contract-only) error = %v", err)
	}
	if result.Metadata.Manifest.ID != "basic-app-a7x2m" || len(result.Images) != 0 || len(result.Assets) != 0 {
		t.Fatalf("contract-only result = %+v", result)
	}
}

func TestGateMPKClassifiesManifestSemanticErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
		want   string
	}{
		{
			name: "endpoint protocol",
			mutate: func(files map[string][]byte) {
				files["manifest.yaml"] = bytes.Replace(files["manifest.yaml"], []byte("protocol: http"), []byte("protocol: tcp"), 1)
			},
			want: "MPK-MANIFEST-ENDPOINT",
		},
		{
			name: "reserved mount target",
			mutate: func(files map[string][]byte) {
				files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: data, target: /proc}\n")...)
			},
			want: "MPK-MANIFEST-MOUNT",
		},
		{
			name: "missing overlay file",
			mutate: func(files map[string][]byte) {
				files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: overlay, subpath: missing.conf, target: /etc/app.conf, read_only: true}\n")...)
			},
			want: "MPK-MANIFEST-OVERLAY",
		},
		{
			name: "legacy overlay source",
			mutate: func(files map[string][]byte) {
				files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: ./missing.conf, target: /etc/app.conf, read_only: true}\n")...)
			},
			want: "MPK-MANIFEST-MOUNT",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := contract.GateMPK(bytes.NewReader(buildMPK(t, test.mutate)), contract.GateOptions{})
			if err == nil {
				t.Fatal("GateMPK() = nil, want semantic rejection")
			}
			findings := contract.FindingsFromError(err)
			if len(findings) != 1 || findings[0].RuleID != test.want {
				t.Fatalf("FindingsFromError() = %+v, want %s", findings, test.want)
			}
		})
	}
}

func TestGateMPKAcceptsOverlayFile(t *testing.T) {
	t.Parallel()
	data := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: overlay, subpath: conf/app.conf, target: /etc/app.conf, read_only: true}\n")...)
		files["overlay/conf/app.conf"] = []byte("enabled=true\n")
	})
	if _, err := contract.GateMPK(bytes.NewReader(data), contract.GateOptions{}); err != nil {
		t.Fatalf("GateMPK() error = %v", err)
	}
}

func TestGateMPKRequiresWritableManifestBindingForImageVolume(t *testing.T) {
	t.Parallel()
	withoutBinding := buildMPK(t, func(files map[string][]byte) {
		files["images/amd64/app.tar"] = dockerArchiveWithVolume(t, contract.ArchAMD64, "demo-a7x2m/web:1.0.0", "/var/lib/app")
	})
	_, err := contract.GateMPK(bytes.NewReader(withoutBinding), contract.GateOptions{})
	if err == nil || contract.FindingsFromError(err)[0].RuleID != "MPK-VOLUME-UNMANAGED" {
		t.Fatalf("unbound image volume error = %v, want MPK-VOLUME-UNMANAGED", err)
	}

	withBinding := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: data, subpath: app, target: /var/lib/app}\n")...)
		files["images/amd64/app.tar"] = dockerArchiveWithVolume(t, contract.ArchAMD64, "demo-a7x2m/web:1.0.0", "/var/lib/app")
	})
	result, err := contract.GateMPK(bytes.NewReader(withBinding), contract.GateOptions{})
	if err != nil {
		t.Fatalf("writable image volume binding error = %v", err)
	}
	if got := result.ImageSummaries["images/amd64/app.tar"].Volumes; len(got) != 1 || got[0] != "/var/lib/app" {
		t.Fatalf("image volume summary = %#v", got)
	}
	bindings, err := contract.ImageVolumeBindings(result.Metadata, result.Images, result.ImageSummaries)
	if err != nil || len(bindings) != 1 || bindings[0].Source != "data" || bindings[0].Subpath != "app" {
		t.Fatalf("image volume binding facts = %#v, err=%v", bindings, err)
	}

	readOnlyBinding := buildMPK(t, func(files map[string][]byte) {
		files["manifest.yaml"] = append(files["manifest.yaml"], []byte("    mounts:\n      - {source: data, target: /var/lib/app, read_only: true}\n")...)
		files["images/amd64/app.tar"] = dockerArchiveWithVolume(t, contract.ArchAMD64, "demo-a7x2m/web:1.0.0", "/var/lib/app")
	})
	_, err = contract.GateMPK(bytes.NewReader(readOnlyBinding), contract.GateOptions{})
	if err == nil || contract.FindingsFromError(err)[0].RuleID != "MPK-MANIFEST-MOUNT" {
		t.Fatalf("read-only image volume error = %v, want MPK-MANIFEST-MOUNT", err)
	}
}
