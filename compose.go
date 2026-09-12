package contract

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

func validateCompose(manifest Manifest, files map[string][]byte, contractOnly bool) error {
	var baseline map[string]struct{}
	for _, architecture := range manifest.Architectures {
		name := "compose." + architecture + ".yaml"
		data, exists := files[name]
		if !exists {
			return fmt.Errorf("mpk: missing %s for declared architecture", name)
		}
		var document struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("mpk: %s is invalid YAML: %w", name, err)
		}
		var rawDocument any
		if err := yaml.Unmarshal(data, &rawDocument); err != nil {
			return fmt.Errorf("mpk: %s is invalid YAML: %w", name, err)
		}
		if len(document.Services) == 0 {
			return fmt.Errorf("mpk: %s has no services", name)
		}
		if err := validateComposeServices(manifest, name, document.Services, files, architecture, contractOnly); err != nil {
			return err
		}
		if containsComposeInterpolation(rawDocument) {
			return fmt.Errorf("mpk: %s cannot use Compose interpolation; declare values in manifest.yaml", name)
		}
		current := make(map[string]struct{}, len(document.Services))
		for serviceName := range document.Services {
			current[serviceName] = struct{}{}
		}
		if baseline == nil {
			baseline = current
		} else if !sameStringSet(baseline, current) {
			return fmt.Errorf("mpk: compose service sets differ between architectures")
		}
	}
	return nil
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, exists := right[key]; !exists {
			return false
		}
	}
	return true
}

func validateComposeServices(
	manifest Manifest,
	composeName string,
	services map[string]map[string]any,
	files map[string][]byte,
	architecture string,
	contractOnly bool,
) error {
	composeImages := make(map[string]struct{})
	serviceNames := make(map[string]struct{}, len(services))
	declaredServices := make(map[string]struct{}, len(manifest.Services))
	for name := range manifest.Services {
		declaredServices[name] = struct{}{}
	}
	for serviceName, service := range services {
		serviceNames[serviceName] = struct{}{}
		if _, exists := declaredServices[serviceName]; !exists {
			return fmt.Errorf("mpk: %s contains undeclared service %q", composeName, serviceName)
		}
		image, _ := service["image"].(string)
		if image == "" {
			return fmt.Errorf("mpk: %s service %q has no image", composeName, serviceName)
		}
		if strings.ContainsRune(image, '$') {
			return fmt.Errorf("mpk: %s service %q image must be literal", composeName, serviceName)
		}
		imageReference, err := parseExplicitImageTag(image)
		if err != nil || isReservedTag(imageReference.Identifier()) {
			return fmt.Errorf("mpk: %s service %q image %q needs a non-reserved tag", composeName, serviceName, image)
		}
		composeImages[image] = struct{}{}
		for _, forbidden := range []string{"ports", "environment", "env_file", "volumes", "volumes_from", "tmpfs", "configs", "secrets"} {
			if _, exists := service[forbidden]; exists {
				return fmt.Errorf("mpk: %s service %q cannot declare %s; use manifest.yaml", composeName, serviceName, forbidden)
			}
		}
		if networkMode, _ := service["network_mode"].(string); strings.EqualFold(networkMode, "host") {
			return fmt.Errorf("mpk: %s service %q cannot use host network mode", composeName, serviceName)
		}
		if _, exists := service["extra_hosts"]; exists {
			return fmt.Errorf("mpk: %s service %q cannot declare extra_hosts; the platform adds its own host mapping", composeName, serviceName)
		}
		if labels, exists := service["labels"]; exists {
			for label := range stringMapKeys(labels) {
				if strings.HasPrefix(strings.ToLower(label), "platform.") {
					return fmt.Errorf("mpk: %s service %q cannot declare platform label %q", composeName, serviceName, label)
				}
			}
		}
	}
	for serviceName := range declaredServices {
		if _, exists := serviceNames[serviceName]; !exists {
			return fmt.Errorf("mpk: %s is missing declared service %q", composeName, serviceName)
		}
	}
	for serviceName, declaration := range manifest.Services {
		for _, mount := range declaration.Mounts {
			if !strings.HasPrefix(mount.Source, "./") {
				continue
			}
			prefix := "overlay/" + strings.TrimPrefix(path.Clean(mount.Source), "./")
			found := false
			for name := range files {
				if name == prefix || strings.HasPrefix(name, prefix+"/") {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("mpk: %s service %q overlay mount %q is missing from overlay/", composeName, serviceName, mount.Source)
			}
		}
	}
	if err := validateTargetImageCollisions(manifest.ID, composeImages); err != nil {
		return fmt.Errorf("mpk: %s: %w", composeName, err)
	}
	if contractOnly {
		// 契约 fixture 不携带镜像；镜像引用的语法与保留 tag 规则仍然必须通过。
		return nil
	}
	expectedImages, err := imagesInPackage(files, architecture)
	if err != nil {
		return err
	}
	if len(expectedImages) != len(composeImages) {
		return fmt.Errorf("mpk: %s image references and images/%s archives do not correspond one-to-one", composeName, architecture)
	}
	for image := range composeImages {
		if _, exists := expectedImages[image]; !exists {
			return fmt.Errorf("mpk: %s references image %q without a matching archive", composeName, image)
		}
	}
	return nil
}

func stringMapKeys(value any) map[string]struct{} {
	result := map[string]struct{}{}
	switch values := value.(type) {
	case map[string]any:
		for key := range values {
			result[key] = struct{}{}
		}
	case map[string]string:
		for key := range values {
			result[key] = struct{}{}
		}
	case []any:
		for _, item := range values {
			if text, ok := item.(string); ok {
				key := text
				if index := strings.IndexByte(text, '='); index >= 0 {
					key = text[:index]
				}
				if key = strings.TrimSpace(key); key != "" {
					result[key] = struct{}{}
				}
			}
		}
	}
	return result
}

func containsComposeInterpolation(value any) bool {
	found := false
	var visit func(any)
	visit = func(item any) {
		if found {
			return
		}
		switch typed := item.(type) {
		case string:
			for index := 0; index < len(typed); index++ {
				if typed[index] == '$' {
					if index+1 < len(typed) && typed[index+1] == '$' {
						index++
						continue
					}
					found = true
					return
				}
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		case map[string]any:
			for key, child := range typed {
				visit(key)
				visit(child)
			}
		}
	}
	visit(value)
	return found
}

func imagesInPackage(files map[string][]byte, architecture string) (map[string]struct{}, error) {
	prefix := "images/" + architecture + "/"
	tags := make(map[string]struct{})
	archiveCount := 0
	for name := range files {
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, "/") {
			continue
		}
		if path.Ext(name) != ".tar" {
			return nil, fmt.Errorf("mpk: images/%s contains non-tar file %q", architecture, name)
		}
		archiveCount++
		summary, err := InspectImageArchive(bytes.NewReader(files[name]))
		if err != nil {
			return nil, fmt.Errorf("mpk: inspect image archive %q: %w", name, err)
		}
		if summary.Architecture != architecture {
			return nil, fmt.Errorf("mpk: image archive %q architecture %q does not match directory architecture %q", name, summary.Architecture, architecture)
		}
		tags[summary.RepoTag] = struct{}{}
	}
	if archiveCount == 0 {
		return nil, fmt.Errorf("mpk: images/%s is empty", architecture)
	}
	if len(tags) != archiveCount {
		return nil, fmt.Errorf("mpk: images/%s archives do not each contain exactly one manifest tag", architecture)
	}
	return tags, nil
}
