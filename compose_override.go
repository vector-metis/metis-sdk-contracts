package contract

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxComposeOverrideBodyBytes 是单个 Compose service 覆盖模板正文的最大字节数。
const MaxComposeOverrideBodyBytes = 256 << 10

// ComposeOverride 是平台解析模板绑定后交给安装契约的不可变快照。
// ID 和 Name 只用于校验重复绑定及提供可定位的错误上下文。
type ComposeOverride struct {
	ID   int64
	Name string
	Body string
}

// ValidateComposeOverride 校验模板是单个非空 Compose service YAML mapping。
// 它不校验 Compose 字段或宿主能力，业务正确性由维护模板的平台管理员负责。
func ValidateComposeOverride(body string) error {
	_, err := decodeComposeOverride(body)
	return err
}

// applyComposeOverrides 在平台完成标准注入后，按 service 和绑定顺序执行最终覆盖。
func applyComposeOverrides(
	manifest Manifest,
	services map[string]map[string]any,
	overrides map[string][]ComposeOverride,
) error {
	serviceNames := make([]string, 0, len(manifest.Services))
	for serviceName := range manifest.Services {
		serviceNames = append(serviceNames, serviceName)
	}
	slices.Sort(serviceNames)

	for _, serviceName := range serviceNames {
		declaration := manifest.Services[serviceName]
		items := overrides[serviceName]
		if _, required := declaration.Capabilities["compose-override"]; required && len(items) == 0 {
			return fmt.Errorf("mpk: service %q requires at least one Compose override", serviceName)
		}
	}

	overrideServices := make([]string, 0, len(overrides))
	for serviceName := range overrides {
		overrideServices = append(overrideServices, serviceName)
	}
	slices.Sort(overrideServices)
	for _, serviceName := range overrideServices {
		service, exists := services[serviceName]
		if !exists {
			return fmt.Errorf("mpk: Compose override references unknown service %q", serviceName)
		}
		seen := make(map[int64]struct{}, len(overrides[serviceName]))
		for position, item := range overrides[serviceName] {
			if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
				return fmt.Errorf("mpk: service %q Compose override at position %d requires template ID and name", serviceName, position)
			}
			if _, duplicate := seen[item.ID]; duplicate {
				return fmt.Errorf("mpk: service %q has duplicate Compose override template %d", serviceName, item.ID)
			}
			seen[item.ID] = struct{}{}

			mapping, err := decodeComposeOverride(item.Body)
			if err != nil {
				return fmt.Errorf(
					"mpk: service %q Compose override %q (%d) at position %d: %w",
					serviceName,
					item.Name,
					item.ID,
					position,
					err,
				)
			}
			service = mergeComposeMapping(service, mapping)
		}
		services[serviceName] = service
	}
	return nil
}

func decodeComposeOverride(body string) (map[string]any, error) {
	if len([]byte(body)) > MaxComposeOverrideBodyBytes {
		return nil, fmt.Errorf("Compose override body exceeds %d bytes", MaxComposeOverrideBodyBytes)
	}
	decoder := yaml.NewDecoder(strings.NewReader(body))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("Compose override root must be a non-empty mapping")
		}
		return nil, fmt.Errorf("decode Compose override YAML: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode || len(document.Content[0].Content) == 0 {
		return nil, errors.New("Compose override root must be a non-empty mapping")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode trailing Compose override YAML: %w", err)
		}
		return nil, errors.New("Compose override must contain exactly one YAML document")
	}
	root := document.Content[0]
	if err := validateComposeOverrideNode(root); err != nil {
		return nil, err
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "services" {
			return nil, errors.New("Compose override root must not contain services; provide one service mapping")
		}
	}
	result := map[string]any{}
	if err := root.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode Compose override mapping: %w", err)
	}
	return result, nil
}

func validateComposeOverrideNode(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if strings.HasPrefix(node.Tag, "!") && !strings.HasPrefix(node.Tag, "!!") {
		return fmt.Errorf("Compose override contains unsupported custom YAML tag %q", node.Tag)
	}
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" {
				return errors.New("Compose override mapping keys must be strings")
			}
			if err := validateComposeOverrideNode(node.Content[index+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validateComposeOverrideNode(child); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return errors.New("Compose override YAML aliases are not supported")
	case yaml.ScalarNode:
		return nil
	default:
		return fmt.Errorf("Compose override contains unsupported YAML node kind %d", node.Kind)
	}
	return nil
}

func mergeComposeMapping(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		result[key] = cloneComposeValue(value)
	}
	for key, value := range override {
		current, exists := result[key]
		if !exists {
			result[key] = cloneComposeValue(value)
			continue
		}
		currentMap, currentIsMap := current.(map[string]any)
		overrideMap, overrideIsMap := value.(map[string]any)
		if currentIsMap && overrideIsMap {
			result[key] = mergeComposeMapping(currentMap, overrideMap)
			continue
		}
		result[key] = cloneComposeValue(value)
	}
	return result
}

func cloneComposeValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return mergeComposeMapping(map[string]any{}, item)
	case []any:
		result := make([]any, len(item))
		for index, child := range item {
			result[index] = cloneComposeValue(child)
		}
		return result
	default:
		return item
	}
}
