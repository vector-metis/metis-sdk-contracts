package contract_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// TestInspectPackageImagesStreamsAndValidatesAllArchitectures 锁定平台准备阶段只把当前
// 镜像归档交给访问器，并要求每个 Compose 引用在全部声明架构中一一对应。
func TestInspectPackageImagesStreamsAndValidatesAllArchitectures(t *testing.T) {
	t.Parallel()

	packageData := imagePackage(t, map[string][]byte{
		"images/amd64/web.tar": dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64),
		"images/arm64/web.tar": dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchARM64, contract.ArchARM64),
	})
	metadata := contract.PackageMetadata{
		Manifest: contract.Manifest{ID: "demo-a7x2m", Version: "1.0.0", Architectures: []string{contract.ArchAMD64, contract.ArchARM64}},
		Compose: map[string]string{
			contract.ArchAMD64: "services:\n  web:\n    image: demo-a7x2m/web:1.0.0\n",
			contract.ArchARM64: "services:\n  web:\n    image: demo-a7x2m/web:1.0.0\n",
		},
	}
	visited := make([]string, 0, 2)
	images, err := contract.InspectPackageImages(bytes.NewReader(packageData), metadata, func(archive contract.PackageImageArchive) (*contract.ImageArchiveSummary, error) {
		visited = append(visited, archive.Path)
		data, readErr := io.ReadAll(archive.Body)
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(data)) != archive.Size {
			t.Fatalf("archive %q size = %d, want %d", archive.Path, len(data), archive.Size)
		}
		return contract.InspectImageArchive(bytes.NewReader(data))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || len(visited) != 2 || images[0].Source != "demo-a7x2m/web:1.0.0" || images[1].Source != images[0].Source {
		t.Fatalf("images = %+v, visited = %v", images, visited)
	}
}

// TestInspectPackageImagesRejectsArchitectureReferenceDrift 确保两个架构不会在同一包内
// 悄悄使用不同 source tag，否则无法形成稳定的统一目标 tag。
func TestInspectPackageImagesRejectsArchitectureReferenceDrift(t *testing.T) {
	t.Parallel()

	packageData := imagePackage(t, map[string][]byte{
		"images/amd64/web.tar": dockerOCIArchive(t, "demo-a7x2m/web:1.0.0", "demo-a7x2m/web:1.0.0", contract.ArchAMD64, contract.ArchAMD64),
		"images/arm64/web.tar": dockerOCIArchive(t, "demo-a7x2m/web:2.0.0", "demo-a7x2m/web:2.0.0", contract.ArchARM64, contract.ArchARM64),
	})
	metadata := contract.PackageMetadata{
		Manifest: contract.Manifest{ID: "demo-a7x2m", Version: "1.0.0", Architectures: []string{contract.ArchAMD64, contract.ArchARM64}},
		Compose: map[string]string{
			contract.ArchAMD64: "services:\n  web:\n    image: demo-a7x2m/web:1.0.0\n",
			contract.ArchARM64: "services:\n  web:\n    image: demo-a7x2m/web:2.0.0\n",
		},
	}
	_, err := contract.InspectPackageImages(bytes.NewReader(packageData), metadata, func(archive contract.PackageImageArchive) (*contract.ImageArchiveSummary, error) {
		data, readErr := io.ReadAll(archive.Body)
		if readErr != nil {
			return nil, readErr
		}
		return contract.InspectImageArchive(bytes.NewReader(data))
	})
	if err == nil {
		t.Fatal("InspectPackageImages() = nil error, want cross-architecture reference failure")
	}
}

func imagePackage(t *testing.T, images map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range images {
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
