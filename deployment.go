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

	"gopkg.in/yaml.v3"
)

// DeploymentPackage 是 Master 交给 Worker 的最小运行时副本，不包含 MPK 或镜像归档。
type DeploymentPackage struct {
	Compose     []byte
	Environment map[string]string
	Overlay     map[string][]byte
	// ManagedDirectories 是 Master 根据最终 Compose 计算出的运行时目录；
	// 目录条目会随部署包下发，但不会携带应用数据文件。
	ManagedDirectories []string
	Package            io.ReadSeeker
	Architecture       string
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
		mode := int64(0o750)
		if directory == "config/" || directory == "data/" || directory == "log/" || directory == "tmp/" {
			mode = 0o777
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: directory, Typeflag: tar.TypeDir, Mode: mode}); err != nil {
			return fmt.Errorf("deployment: write directory %q: %w", directory, err)
		}
	}
	if err := writeManagedDirectories(tarWriter, deployment.ManagedDirectories); err != nil {
		return err
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

func writeManagedDirectories(writer *tar.Writer, directories []string) error {
	seen := map[string]struct{}{
		"config": {},
		"data":   {},
		"log":    {},
		"tmp":    {},
	}
	for _, directory := range directories {
		cleaned := path.Clean(strings.TrimSuffix(directory, "/"))
		if !isRuntimeDirectoryPath(cleaned) {
			return fmt.Errorf("deployment: unsafe managed directory %q", directory)
		}
		pending := make([]string, 0, strings.Count(cleaned, "/"))
		for current := cleaned; current != "."; current = path.Dir(current) {
			if _, exists := seen[current]; exists {
				break
			}
			pending = append(pending, current)
			if path.Dir(current) == current {
				break
			}
		}
		for index := len(pending) - 1; index >= 0; index-- {
			current := pending[index]
			seen[current] = struct{}{}
			if err := writer.WriteHeader(&tar.Header{Name: current + "/", Typeflag: tar.TypeDir, Mode: 0o777}); err != nil {
				return fmt.Errorf("deployment: write managed directory %q: %w", current, err)
			}
		}
	}
	return nil
}

func isRuntimeDirectoryPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || parts[0] == "" {
		return false
	}
	switch parts[0] {
	case "config", "data", "log", "tmp":
	default:
		return false
	}
	if path.IsAbs(value) || value == "." || value == ".." || path.Clean(value) != value {
		return false
	}
	for _, part := range parts[1:] {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// ManagedDirectoriesFromCompose 从 Master 生成的最终 Compose 中提取需要预创建的
// config/data/log/tmp 嵌套目录。最终 Compose 只允许使用 scope 相对 bind source，
// 因此这里不接受宿主绝对路径或 scope 外路径；顶层目录由部署包固定写入。
func ManagedDirectoriesFromCompose(compose []byte) ([]string, error) {
	var document struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `yaml:"type"`
				Source string `yaml:"source"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &document); err != nil {
		return nil, fmt.Errorf("deployment: decode compose: %w", err)
	}
	seen := make(map[string]struct{})
	for _, service := range document.Services {
		for _, volume := range service.Volumes {
			if volume.Type != "bind" {
				continue
			}
			source := strings.TrimSpace(volume.Source)
			if !strings.HasPrefix(source, "./") {
				continue
			}
			source = strings.TrimPrefix(source, "./")
			root := strings.SplitN(source, "/", 2)[0]
			switch root {
			case "overlay", "program":
				continue
			case "config", "data", "log", "tmp":
				if source == root {
					continue
				}
			default:
				return nil, fmt.Errorf("deployment: invalid runtime bind source %q", volume.Source)
			}
			if !isRuntimeDirectoryPath(source) {
				return nil, fmt.Errorf("deployment: invalid runtime bind source %q", volume.Source)
			}
			seen[source] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
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
