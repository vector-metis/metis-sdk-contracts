package contract

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// DeploymentPackage 是 Master 交给 Worker 的最小运行时副本，不包含 MPK 或镜像归档。
type DeploymentPackage struct {
	Compose      []byte
	Environment  map[string]string
	Overlay      map[string][]byte
	Package      io.ReadSeeker
	Architecture string
}

// WriteDeploymentPackage 流式写出可直接解压到应用隔离目录的 tar.gz。
func WriteDeploymentPackage(output io.Writer, deployment DeploymentPackage) (resultErr error) {
	if output == nil || len(deployment.Compose) == 0 || deployment.Environment == nil {
		return fmt.Errorf("deployment: compose, environment and output are required")
	}
	gzipWriter := gzip.NewWriter(output)
	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		resultErr = errors.Join(resultErr, tarWriter.Close(), gzipWriter.Close())
	}()

	for _, directory := range []string{"program/", "config/", "data/", "log/", "tmp/", "overlay/"} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: directory, Typeflag: tar.TypeDir, Mode: 0o750}); err != nil {
			return fmt.Errorf("deployment: write directory %q: %w", directory, err)
		}
	}
	if err := writeDeploymentProgram(tarWriter, deployment.Package, deployment.Architecture); err != nil {
		return err
	}
	if err := writeDeploymentFile(tarWriter, "compose.yaml", 0o640, deployment.Compose); err != nil {
		return err
	}
	if err := writeDeploymentFile(tarWriter, ".env", 0o600, deploymentEnvironment(deployment.Environment)); err != nil {
		return err
	}

	directories := make(map[string]struct{})
	files := make(map[string][]byte, len(deployment.Overlay))
	for name := range deployment.Overlay {
		isDirectory := strings.HasSuffix(name, "/")
		cleaned := path.Clean(name)
		if name == "" || path.IsAbs(name) || cleaned == "." || cleaned == ".." ||
			strings.HasPrefix(cleaned, "../") || strings.ContainsRune(name, '\x00') {
			return fmt.Errorf("deployment: unsafe overlay path %q", name)
		}
		if isDirectory {
			directories[cleaned] = struct{}{}
			continue
		}
		if _, duplicate := files[cleaned]; duplicate {
			return fmt.Errorf("deployment: duplicate overlay path %q", cleaned)
		}
		files[cleaned] = deployment.Overlay[name]
		for parent := path.Dir(cleaned); parent != "." && parent != ""; parent = path.Dir(parent) {
			directories[parent] = struct{}{}
		}
	}
	directoryNames := make([]string, 0, len(directories))
	for name := range directories {
		if _, exists := files[name]; exists {
			return fmt.Errorf("deployment: overlay path is both file and directory %q", name)
		}
		for parent := path.Dir(name); parent != "." && parent != ""; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return fmt.Errorf("deployment: overlay file blocks directory %q", name)
			}
		}
		directoryNames = append(directoryNames, name)
	}
	sort.Strings(directoryNames)
	for _, name := range directoryNames {
		if err := tarWriter.WriteHeader(&tar.Header{Name: "overlay/" + name + "/", Typeflag: tar.TypeDir, Mode: 0o750}); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeDeploymentFile(tarWriter, "overlay/"+name, 0o640, files[name]); err != nil {
			return err
		}
	}
	return nil
}

// writeDeploymentProgram 从 ready MPK 中顺序复制目标架构的可选程序文件。
// 镜像和其他架构成员只被扫描，不进入部署包，也不会整体读入内存。
func writeDeploymentProgram(writer *tar.Writer, packageBody io.ReadSeeker, architecture string) error {
	if packageBody == nil {
		return nil
	}
	if architecture != ArchAMD64 && architecture != ArchARM64 {
		return fmt.Errorf("deployment: unsupported architecture %q", architecture)
	}
	if _, err := packageBody.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("deployment: rewind package: %w", err)
	}
	defer func() { _, _ = packageBody.Seek(0, io.SeekStart) }()
	gzipReader, err := gzip.NewReader(packageBody)
	if err != nil {
		return fmt.Errorf("deployment: package gzip format is invalid: %w", err)
	}
	defer gzipReader.Close()

	prefix := "program/" + architecture + "/"
	written := make(map[string]struct{})
	tarReader := tar.NewReader(gzipReader)
	for {
		header, readErr := tarReader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("deployment: package tar format is invalid: %w", readErr)
		}
		name := path.Clean(header.Name)
		if header.Name == "" || name == "." || path.IsAbs(header.Name) || name == ".." ||
			strings.HasPrefix(name, "../") || strings.ContainsRune(header.Name, '\x00') {
			return fmt.Errorf("deployment: unsafe package path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg:
		default:
			return fmt.Errorf("deployment: forbidden package node %q", name)
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		relative := strings.TrimPrefix(name, prefix)
		if relative == "" || relative == "." {
			continue
		}
		target := "program/" + relative
		if _, duplicate := written[target]; duplicate {
			return fmt.Errorf("deployment: duplicate program entry %q", target)
		}
		written[target] = struct{}{}
		mode := header.Mode & 0o777
		if header.Typeflag == tar.TypeDir {
			if mode == 0 {
				mode = 0o750
			}
			if err := writer.WriteHeader(&tar.Header{Name: strings.TrimSuffix(target, "/") + "/", Typeflag: tar.TypeDir, Mode: mode}); err != nil {
				return fmt.Errorf("deployment: write program directory %q: %w", target, err)
			}
			continue
		}
		if mode == 0 {
			mode = 0o640
		}
		if err := writer.WriteHeader(&tar.Header{Name: target, Typeflag: tar.TypeReg, Mode: mode, Size: header.Size}); err != nil {
			return fmt.Errorf("deployment: write program header %q: %w", target, err)
		}
		if _, err := io.CopyN(writer, tarReader, header.Size); err != nil {
			return fmt.Errorf("deployment: copy program file %q: %w", target, err)
		}
	}
	return nil
}

func writeDeploymentFile(writer *tar.Writer, name string, mode int64, content []byte) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(content))}); err != nil {
		return fmt.Errorf("deployment: write header %q: %w", name, err)
	}
	if _, err := writer.Write(content); err != nil {
		return fmt.Errorf("deployment: write %q: %w", name, err)
	}
	return nil
}

func deploymentEnvironment(values map[string]string) []byte {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		output.WriteString(name)
		output.WriteByte('=')
		output.WriteString(values[name])
		output.WriteByte('\n')
	}
	return []byte(output.String())
}
