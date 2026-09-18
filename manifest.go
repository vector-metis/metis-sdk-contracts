// Package contract 保存平台和商店共享的唯一包结构与规则；模块必须保持极薄，禁止导入任一服务实现。
package contract

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var endpointNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
var manifestPlaceholderPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)
var modelSlotPattern = regexp.MustCompile(`^(llm|embedding|rerank)\.[0-9]+$`)

// 支持的架构和能力名称是封闭集合，消费方用于校验、界面展示和安装兼容性判断。
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// ApplicationType 是应用运行形态的封闭集合；不同类型不能混用入口字段。
type ApplicationType string

const (
	// ApplicationTypeWeb 表示具有一个 HTTP/WebSocket 主入口的应用。
	ApplicationTypeWeb ApplicationType = "web"
	// ApplicationTypeService 表示只向其他应用提供原始 TCP/UDP 能力的应用。
	ApplicationTypeService ApplicationType = "service"
)

// EndpointProtocol 是 Service endpoint 可以声明的传输协议闭集。
type EndpointProtocol string

const (
	// EndpointProtocolTCP 表示面向连接的 TCP 字节流。
	EndpointProtocolTCP EndpointProtocol = "tcp"
	// EndpointProtocolUDP 表示保持数据报边界的 UDP 传输。
	EndpointProtocolUDP EndpointProtocol = "udp"
	// EndpointProtocolHTTP 表示 HTTP、WebSocket、SSE 和流式 HTTP 入口。
	EndpointProtocolHTTP EndpointProtocol = "http"
)

// ServiceEndpoint 描述 Service 应用在一个 Compose service 上公开的具名端点。
type ServiceEndpoint struct {
	// Name 是依赖方用于发现端点的应用内唯一名称。
	Name string `yaml:"name" json:"name"`
	// Service 是承载端点的 Compose service 名称。
	Service string `yaml:"service" json:"service"`
	// Protocol 是端点使用的原始传输协议。
	Protocol EndpointProtocol `yaml:"protocol" json:"protocol"`
	// ContainerPort 是容器内监听端口，不是 Worker 或 Master 的分配端口。
	ContainerPort int `yaml:"container_port" json:"containerPort"`
}

// EnvironmentSource 描述 service 环境变量的唯一来源。
// Value 和 Setting 在 manifest 中互斥；平台只在安装时解析 setting。
type EnvironmentSource struct {
	Value   *string `yaml:"value,omitempty" json:"value,omitempty"`
	Setting *string `yaml:"setting,omitempty" json:"setting,omitempty"`
}

// Mount 描述平台生成的沙箱或 overlay 挂载。
type Mount struct {
	Source   string `yaml:"source" json:"source"`
	Target   string `yaml:"target" json:"target"`
	ReadOnly bool   `yaml:"read_only,omitempty" json:"readOnly,omitempty"`
}

// CapabilityRequest 是 service 对平台能力的最小声明。
// slots 只对 model-gateway 有意义，其余能力必须使用空对象。
type CapabilityRequest struct {
	Slots []string `yaml:"slots,omitempty" json:"slots,omitempty"`
}

// ModelSlot 是 manifest 顶层声明的模型插槽。
type ModelSlot struct {
	Interface string `yaml:"interface" json:"interface"`
}

// ManifestService 是 manifest.services 中的单个 Compose service 声明。
type ManifestService struct {
	Endpoints    []ServiceEndpoint            `yaml:"endpoints" json:"endpoints"`
	Environment  map[string]EnvironmentSource `yaml:"environment" json:"environment"`
	Mounts       []Mount                      `yaml:"mounts" json:"mounts"`
	Capabilities map[string]CapabilityRequest `yaml:"capabilities" json:"capabilities"`
}

// Capabilities 是应用唯一允许请求的平台资源闭集。
var Capabilities = map[string]struct{}{
	"object-storage":   {},
	"model-gateway":    {},
	"platform-api":     {},
	"compose-override": {},
}

// ModelInterfaces 是 MPK v1 声明模型 slot 时允许的透传协议闭集。
// 平台网关按路径透传，不在这些接口之间做协议转换。
var ModelInterfaces = map[string]map[string]struct{}{
	"llm": {
		"openai.chat.completions": {},
		"openai.responses":        {},
		"anthropic.messages":      {},
	},
	"embedding": {"openai.embeddings": {}},
	"rerank":    {"openai.rerank": {}},
}

// Dependency 表示 MPK 依赖的另一个应用；可选依赖安装时可缺失，必选依赖不可缺失。
type Dependency struct {
	ID       string `yaml:"id" json:"id"`
	Alias    string `yaml:"alias" json:"alias"`
	Required bool   `yaml:"required" json:"required"`
	// Version 是必填的有限 SemVer 约束，例如 ^1.2.0 或 >=1.2.0 <2.0.0。
	Version string `yaml:"version" json:"version"`
}

// Manifest 是保存在 MPK 包根的完整声明式元数据。
type Manifest struct {
	SchemaVersion      int             `yaml:"schema_version" json:"schemaVersion"`
	ID                 string          `yaml:"id" json:"id"`
	Version            string          `yaml:"version" json:"version"`
	DisplayName        string          `yaml:"display_name" json:"displayName"`
	Type               ApplicationType `yaml:"type" json:"type"`
	Description        string          `yaml:"description,omitempty" json:"description,omitempty"`
	IntegrationDocsURL string          `yaml:"integration_docs_url,omitempty" json:"integrationDocsUrl,omitempty"`
	Screenshots        []string        `yaml:"screenshots,omitempty" json:"screenshots,omitempty"`
	Architectures      []string        `yaml:"arch" json:"arch"`
	// Capabilities 是从 Services 去重派生的应用能力视图，不接受 manifest 顶层输入。
	Capabilities []string                   `yaml:"-" json:"capabilities"`
	Dependencies []Dependency               `yaml:"dependencies" json:"dependencies"`
	Settings     []Setting                  `yaml:"settings" json:"settings"`
	Models       map[string]ModelSlot       `yaml:"models" json:"models"`
	Services     map[string]ManifestService `yaml:"services" json:"services"`
}

// ComposeService 只包含确定性校验所需字段；未知字段保留在原始 map 中，Agent 可原样写出 Compose 模板。
type ComposeService struct {
	Image       string            `yaml:"image,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	NetworkMode string            `yaml:"network_mode,omitempty"`
	ExtraHosts  []string          `yaml:"extra_hosts,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Labels      any               `yaml:"labels,omitempty"`
}

type composeDocument struct {
	Services map[string]ComposeService `yaml:"services"`
}

type manifestForDecode struct {
	SchemaVersion      *int                        `yaml:"schema_version"`
	ID                 *string                     `yaml:"id"`
	Version            *string                     `yaml:"version"`
	DisplayName        *string                     `yaml:"display_name"`
	Type               *ApplicationType            `yaml:"type"`
	Description        *string                     `yaml:"description"`
	IntegrationDocsURL *string                     `yaml:"integration_docs_url"`
	Screenshots        *[]string                   `yaml:"screenshots"`
	Architectures      *[]string                   `yaml:"arch"`
	Capabilities       *[]string                   `yaml:"capabilities"`
	Dependencies       *[]Dependency               `yaml:"dependencies"`
	Settings           *map[string]Setting         `yaml:"settings"`
	Models             *map[string]ModelSlot       `yaml:"models"`
	Services           *map[string]ManifestService `yaml:"services"`
	PrimaryService     *string                     `yaml:"primary_service"`
	ContainerPort      *int                        `yaml:"container_port"`
	Endpoints          *[]ServiceEndpoint          `yaml:"endpoints"`
}

func (m *Manifest) UnmarshalYAML(value *yaml.Node) error {
	if err := rejectUnknownManifestFields(value); err != nil {
		return err
	}
	var raw manifestForDecode
	if err := value.Decode(&raw); err != nil {
		return err
	}
	if raw.SchemaVersion == nil || *raw.SchemaVersion != 1 {
		return fmt.Errorf("manifest: schema_version must be 1")
	}
	if raw.ID == nil || raw.Version == nil || raw.DisplayName == nil || raw.Type == nil || raw.Architectures == nil || raw.Dependencies == nil || raw.Services == nil {
		return fmt.Errorf("manifest: id, version, display_name, type, arch, dependencies and services are required")
	}
	if raw.Capabilities != nil {
		return fmt.Errorf("manifest: top-level capabilities is not allowed; declare capabilities per service")
	}
	if raw.PrimaryService != nil || raw.ContainerPort != nil || raw.Endpoints != nil {
		return fmt.Errorf("manifest: primary_service, container_port and endpoints are legacy fields; declare endpoints under services")
	}
	m.SchemaVersion = *raw.SchemaVersion
	m.ID = *raw.ID
	m.Version = *raw.Version
	m.DisplayName = *raw.DisplayName
	m.Type = *raw.Type
	m.Description = stringOr(raw.Description)
	m.IntegrationDocsURL = stringOr(raw.IntegrationDocsURL)
	m.Screenshots = sliceOr(raw.Screenshots)
	m.Architectures = *raw.Architectures
	m.Capabilities = nil
	m.Dependencies = *raw.Dependencies
	if raw.Settings != nil {
		m.Settings = make([]Setting, 0, len(*raw.Settings))
		for key, setting := range *raw.Settings {
			setting.Key = key
			m.Settings = append(m.Settings, setting)
		}
		slices.SortFunc(m.Settings, func(left, right Setting) int { return strings.Compare(left.Key, right.Key) })
	}
	if raw.Models != nil {
		m.Models = *raw.Models
	} else {
		m.Models = map[string]ModelSlot{}
	}
	m.Services = *raw.Services
	for serviceName, service := range m.Services {
		if serviceName == "" {
			return fmt.Errorf("manifest: service name cannot be empty")
		}
		for index := range service.Endpoints {
			if service.Endpoints[index].Service == "" {
				service.Endpoints[index].Service = serviceName
			}
		}
		m.Services[serviceName] = service
	}
	return nil
}

// rejectUnknownManifestFields keeps the v1 YAML contract closed while allowing
// dynamic keys only where the schema explicitly defines a map (services,
// settings, models, environment and capabilities).
func rejectUnknownManifestFields(value *yaml.Node) error {
	if value.Kind == yaml.DocumentNode {
		value = value.Content[0]
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest: root must be a mapping")
	}
	if err := rejectMappingKeys(value, map[string]struct{}{
		"schema_version": {}, "id": {}, "version": {}, "display_name": {},
		"type": {}, "description": {}, "integration_docs_url": {}, "screenshots": {},
		"arch": {}, "capabilities": {}, "dependencies": {}, "settings": {},
		"models": {}, "services": {}, "primary_service": {}, "container_port": {}, "endpoints": {},
	}); err != nil {
		return err
	}
	for _, pair := range mappingPairs(value) {
		switch pair.key {
		case "dependencies":
			for _, item := range sequenceItems(pair.value) {
				if err := rejectMappingKeys(item, map[string]struct{}{"id": {}, "alias": {}, "required": {}, "version": {}}); err != nil {
					return err
				}
			}
		case "settings":
			for _, item := range mappingValues(pair.value) {
				if err := rejectMappingKeys(item, map[string]struct{}{"label": {}, "type": {}, "required": {}, "default": {}, "options": {}}); err != nil {
					return err
				}
			}
		case "models":
			for _, item := range mappingValues(pair.value) {
				if err := rejectMappingKeys(item, map[string]struct{}{"interface": {}}); err != nil {
					return err
				}
			}
		case "services":
			for _, service := range mappingValues(pair.value) {
				if err := rejectMappingKeys(service, map[string]struct{}{"endpoints": {}, "environment": {}, "mounts": {}, "capabilities": {}}); err != nil {
					return err
				}
				for _, servicePair := range mappingPairs(service) {
					switch servicePair.key {
					case "endpoints":
						for _, endpoint := range sequenceItems(servicePair.value) {
							if err := rejectMappingKeys(endpoint, map[string]struct{}{"name": {}, "service": {}, "protocol": {}, "container_port": {}}); err != nil {
								return err
							}
						}
					case "environment":
						for _, source := range mappingValues(servicePair.value) {
							if err := rejectMappingKeys(source, map[string]struct{}{"value": {}, "setting": {}}); err != nil {
								return err
							}
						}
					case "mounts":
						for _, mount := range sequenceItems(servicePair.value) {
							if err := rejectMappingKeys(mount, map[string]struct{}{"source": {}, "target": {}, "read_only": {}}); err != nil {
								return err
							}
						}
					case "capabilities":
						for _, capability := range mappingValues(servicePair.value) {
							if err := rejectMappingKeys(capability, map[string]struct{}{"slots": {}}); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return nil
}

type manifestMappingPair struct {
	key   string
	value *yaml.Node
}

func mappingPairs(node *yaml.Node) []manifestMappingPair {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	result := make([]manifestMappingPair, 0, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		result = append(result, manifestMappingPair{key: node.Content[index].Value, value: node.Content[index+1]})
	}
	return result
}

func mappingValues(node *yaml.Node) []*yaml.Node {
	pairs := mappingPairs(node)
	result := make([]*yaml.Node, 0, len(pairs))
	for _, pair := range pairs {
		result = append(result, pair.value)
	}
	return result
}

func sequenceItems(node *yaml.Node) []*yaml.Node {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	return node.Content
}

func rejectMappingKeys(node *yaml.Node, allowed map[string]struct{}) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for _, pair := range mappingPairs(node) {
		if _, ok := allowed[pair.key]; !ok {
			return fmt.Errorf("manifest: unknown field %q", pair.key)
		}
	}
	return nil
}

func stringOr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sliceOr[T any](value *[]T) []T {
	if value == nil {
		return nil
	}
	return *value
}

func (m *Manifest) validateIdentity() error {
	if m.ID == "" || m.Version == "" || m.DisplayName == "" {
		return fmt.Errorf("manifest: id, version and display_name are required")
	}
	if err := ValidateVersion(m.Version); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("manifest: schema_version must be 1")
	}
	if len(m.Services) == 0 {
		return fmt.Errorf("manifest: services are required")
	}
	switch m.Type {
	case ApplicationTypeWeb:
		if err := validateWebEndpoints(m.Services); err != nil {
			return err
		}
	case ApplicationTypeService:
		if err := validateServiceManifestEndpoints(m.Services); err != nil {
			return err
		}
	default:
		return fmt.Errorf("manifest: unsupported application type %q", m.Type)
	}
	if len(m.Architectures) == 0 {
		return fmt.Errorf("manifest: at least one architecture is required")
	}
	seenArch := make(map[string]struct{}, len(m.Architectures))
	for _, architecture := range m.Architectures {
		if architecture != ArchAMD64 && architecture != ArchARM64 {
			return fmt.Errorf("manifest: unsupported architecture %q", architecture)
		}
		if _, exists := seenArch[architecture]; exists {
			return fmt.Errorf("manifest: duplicate architecture %q", architecture)
		}
		seenArch[architecture] = struct{}{}
	}
	capabilities, err := validateServiceCapabilities(m.Services, m.Models)
	if err != nil {
		return err
	}
	m.Capabilities = capabilities
	if err := validateManifestSettings(m.Settings); err != nil {
		return err
	}
	if err := validateServiceEnvironment(m.Services, m.Settings); err != nil {
		return err
	}
	aliases := make(map[string]struct{}, len(m.Dependencies))
	dependencyIDs := make(map[string]struct{}, len(m.Dependencies))
	for index, dependency := range m.Dependencies {
		if dependency.ID == "" || dependency.Alias == "" || dependency.Version == "" {
			return fmt.Errorf("manifest: dependencies[%d] requires id, alias and version", index)
		}
		if err := ValidateDependencyConstraint(dependency.Version); err != nil {
			return fmt.Errorf("manifest: dependencies[%d] has invalid version constraint: %w", index, err)
		}
		if dependency.ID == m.ID {
			return fmt.Errorf("manifest: dependency %q references itself", dependency.ID)
		}
		if _, exists := aliases[dependency.Alias]; exists {
			return fmt.Errorf("manifest: duplicate dependency alias %q", dependency.Alias)
		}
		if _, exists := dependencyIDs[dependency.ID]; exists {
			return fmt.Errorf("manifest: duplicate dependency id %q", dependency.ID)
		}
		aliases[dependency.Alias] = struct{}{}
		dependencyIDs[dependency.ID] = struct{}{}
	}
	for _, dependency := range m.Dependencies {
		if _, conflicts := dependencyIDs[dependency.Alias]; conflicts && dependency.Alias != dependency.ID {
			return fmt.Errorf("manifest: dependency alias %q conflicts with another dependency id", dependency.Alias)
		}
	}
	return nil
}

func validateWebEndpoints(services map[string]ManifestService) error {
	count := 0
	for serviceName, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint.Protocol != EndpointProtocolHTTP {
				return fmt.Errorf("manifest: web service %q can only declare http endpoints", serviceName)
			}
			if endpoint.Service != "" && endpoint.Service != serviceName {
				return fmt.Errorf("manifest: endpoint %q references unknown service %q", endpoint.Name, endpoint.Service)
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("manifest: web application requires exactly one http endpoint")
	}
	return validateServiceEndpoints(flattenEndpoints(services), true)
}

func validateServiceManifestEndpoints(services map[string]ManifestService) error {
	endpoints := flattenEndpoints(services)
	if len(endpoints) == 0 {
		return fmt.Errorf("manifest: service application requires at least one endpoint")
	}
	for serviceName, declaration := range services {
		for _, endpoint := range declaration.Endpoints {
			if endpoint.Service != "" && endpoint.Service != serviceName {
				return fmt.Errorf("manifest: endpoint %q must belong to its declaring service %q", endpoint.Name, serviceName)
			}
			if endpoint.Protocol != EndpointProtocolTCP && endpoint.Protocol != EndpointProtocolUDP {
				return fmt.Errorf("manifest: service endpoint %q must use tcp or udp", endpoint.Name)
			}
		}
	}
	return validateServiceEndpoints(endpoints, false)
}

func flattenEndpoints(services map[string]ManifestService) []ServiceEndpoint {
	result := make([]ServiceEndpoint, 0)
	for serviceName, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint.Service == "" {
				endpoint.Service = serviceName
			}
			result = append(result, endpoint)
		}
	}
	return result
}

func validateManifestSettings(settings []Setting) error {
	seen := make(map[string]struct{}, len(settings))
	for _, setting := range settings {
		if setting.Key == "" || setting.Label == "" || setting.Type == "" {
			return fmt.Errorf("manifest: setting key, label and type are required")
		}
		switch setting.Type {
		case "string", "number", "bool", "secret", "select":
		default:
			return fmt.Errorf("manifest: setting %q has unsupported type %q", setting.Key, setting.Type)
		}
		if _, exists := seen[setting.Key]; exists {
			return fmt.Errorf("manifest: duplicate setting key %q", setting.Key)
		}
		seen[setting.Key] = struct{}{}
		if setting.Type == "secret" && setting.Default != "" {
			return fmt.Errorf("manifest: secret setting %q cannot have a default", setting.Key)
		}
		if setting.Type == "select" && len(setting.Options) == 0 {
			return fmt.Errorf("manifest: select setting %q requires options", setting.Key)
		}
		if setting.Type != "select" && len(setting.Options) != 0 {
			return fmt.Errorf("manifest: setting %q options are only valid for select", setting.Key)
		}
		if setting.Default != "" {
			switch setting.Type {
			case "number":
				if _, err := strconv.ParseFloat(setting.Default, 64); err != nil {
					return fmt.Errorf("manifest: setting %q default is not a number", setting.Key)
				}
			case "bool":
				if setting.Default != "true" && setting.Default != "false" {
					return fmt.Errorf("manifest: setting %q default is not a boolean", setting.Key)
				}
			case "select":
				if !slices.Contains(setting.Options, setting.Default) {
					return fmt.Errorf("manifest: setting %q default is not in options", setting.Key)
				}
			}
		}
	}
	return nil
}

func validateServiceEnvironment(services map[string]ManifestService, settings []Setting) error {
	known := make(map[string]struct{}, len(settings))
	used := make(map[string]struct{}, len(settings))
	for _, setting := range settings {
		known[setting.Key] = struct{}{}
	}
	for serviceName, service := range services {
		for name, source := range service.Environment {
			if name == "" || strings.HasPrefix(name, "METIS_") {
				return fmt.Errorf("manifest: service %q has invalid environment name %q", serviceName, name)
			}
			if (source.Value == nil) == (source.Setting == nil) {
				return fmt.Errorf("manifest: service %q environment %q requires exactly one value or setting", serviceName, name)
			}
			if source.Setting != nil {
				if _, exists := known[*source.Setting]; !exists {
					return fmt.Errorf("manifest: service %q environment %q references unknown setting %q", serviceName, name, *source.Setting)
				}
				used[*source.Setting] = struct{}{}
			}
			for _, match := range manifestPlaceholderPattern.FindAllStringSubmatch(stringValue(source.Value), -1) {
				if err := validateServicePlaceholder(serviceName, service, match[1], modelsForService(service)); err != nil {
					return err
				}
			}
		}
		seenTargets := make(map[string]struct{}, len(service.Mounts))
		for _, mount := range service.Mounts {
			if mount.Target == "" || !strings.HasPrefix(mount.Target, "/") || mount.Target == "/" {
				return fmt.Errorf("manifest: service %q mount target must be an absolute non-root path", serviceName)
			}
			target := path.Clean(mount.Target)
			if isReservedMountTarget(target) {
				return fmt.Errorf("manifest: service %q mount target %q is reserved", serviceName, mount.Target)
			}
			if _, exists := seenTargets[target]; exists {
				return fmt.Errorf("manifest: service %q has duplicate mount target %q", serviceName, mount.Target)
			}
			seenTargets[target] = struct{}{}
			switch mount.Source {
			case "program", "config", "data", "log", "tmp":
			default:
				if !isOverlayMountSource(mount.Source) || !mount.ReadOnly {
					return withGateRule("MPK-MANIFEST-MOUNT", fmt.Errorf("manifest: service %q has invalid mount source %q", serviceName, mount.Source))
				}
			}
		}
	}
	for setting := range known {
		if _, exists := used[setting]; !exists {
			return fmt.Errorf("manifest: setting %q is declared but not used by any service environment", setting)
		}
	}
	return nil
}

// isOverlayMountSource 只接受应用 scope 中 overlay 目录的规范化相对路径。
// source 必须保留 ./overlay 前缀，并且必须是规范化路径，避免 ./overlay/../... 绕过根目录限制；
// 包内成员是否存在由 MPK 元数据门禁继续校验。
func isOverlayMountSource(source string) bool {
	if !strings.HasPrefix(source, "./") {
		return false
	}
	relative := strings.TrimPrefix(source, "./")
	if relative != "overlay" && !strings.HasPrefix(relative, "overlay/") {
		return false
	}
	return path.Clean(source) == relative
}

func isReservedMountTarget(target string) bool {
	for _, root := range []string{"/proc", "/sys", "/dev"} {
		if target == root || strings.HasPrefix(target, root+"/") {
			return true
		}
	}
	return target == "/var/run/docker.sock"
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func modelsForService(service ManifestService) map[string]struct{} {
	result := make(map[string]struct{})
	if request, exists := service.Capabilities["model-gateway"]; exists {
		for _, slot := range request.Slots {
			result[slot] = struct{}{}
		}
	}
	return result
}

func validateServicePlaceholder(serviceName string, service ManifestService, name string, slots map[string]struct{}) error {
	switch {
	case strings.HasPrefix(name, "METIS_S3_"):
		if _, exists := service.Capabilities["object-storage"]; !exists {
			return fmt.Errorf("manifest: service %q uses %s without object-storage capability", serviceName, name)
		}
	case strings.HasPrefix(name, "METIS_LLM_"), strings.HasPrefix(name, "METIS_EMBEDDING_"), strings.HasPrefix(name, "METIS_RERANK_"):
		parts := strings.Split(strings.TrimPrefix(name, "METIS_"), "_")
		if len(parts) < 3 {
			return fmt.Errorf("manifest: service %q uses invalid model placeholder %q", serviceName, name)
		}
		slot := strings.ToLower(parts[0]) + "." + parts[1]
		if _, exists := slots[slot]; !exists {
			return fmt.Errorf("manifest: service %q uses %s without model-gateway slot %s", serviceName, name, slot)
		}
	}
	return nil
}

func validateServiceCapabilities(services map[string]ManifestService, models map[string]ModelSlot) ([]string, error) {
	usedSlots := make(map[string]struct{}, len(models))
	for slot, declaration := range models {
		if !modelSlotPattern.MatchString(slot) {
			return nil, fmt.Errorf("manifest: invalid model slot %q", slot)
		}
		if strings.TrimSpace(declaration.Interface) == "" {
			return nil, fmt.Errorf("manifest: model slot %q interface is required", slot)
		}
		parts := strings.SplitN(slot, ".", 2)
		if _, allowed := ModelInterfaces[parts[0]][declaration.Interface]; !allowed {
			return nil, fmt.Errorf("manifest: model slot %q does not support interface %q", slot, declaration.Interface)
		}
	}
	seen := make(map[string]struct{})
	for serviceName, service := range services {
		for capability, request := range service.Capabilities {
			if _, supported := Capabilities[capability]; !supported {
				return nil, fmt.Errorf("manifest: service %q requests unsupported capability %q", serviceName, capability)
			}
			if capability != "model-gateway" && len(request.Slots) != 0 {
				return nil, fmt.Errorf("manifest: capability %q on service %q cannot declare model slots", capability, serviceName)
			}
			if capability == "model-gateway" && len(request.Slots) == 0 {
				return nil, fmt.Errorf("manifest: service %q model-gateway capability requires at least one slot", serviceName)
			}
			for _, slot := range request.Slots {
				if _, exists := models[slot]; !exists {
					return nil, fmt.Errorf("manifest: service %q references undeclared model slot %q", serviceName, slot)
				}
				usedSlots[slot] = struct{}{}
			}
			seen[capability] = struct{}{}
		}
	}
	for slot := range models {
		if _, exists := usedSlots[slot]; !exists {
			return nil, fmt.Errorf("manifest: model slot %q is declared but not used by any service", slot)
		}
	}
	result := make([]string, 0, len(seen))
	for capability := range seen {
		result = append(result, capability)
	}
	slices.Sort(result)
	return result, nil
}

// validateServiceEndpoints 校验 endpoint 的最小运行不变量；Compose 引用在读取架构文件后校验。
func validateServiceEndpoints(endpoints []ServiceEndpoint, allowHTTP bool) error {
	names := make(map[string]struct{}, len(endpoints))
	normalizedNames := make(map[string]string, len(endpoints))
	for index, endpoint := range endpoints {
		if endpoint.Name == "" || endpoint.Service == "" {
			return fmt.Errorf("manifest: endpoints[%d] requires name and service", index)
		}
		if !endpointNamePattern.MatchString(endpoint.Name) {
			return fmt.Errorf("manifest: endpoint name %q must start with a lowercase letter and contain only lowercase letters, digits, hyphens or underscores", endpoint.Name)
		}
		if endpoint.Protocol != EndpointProtocolTCP && endpoint.Protocol != EndpointProtocolUDP && (!allowHTTP || endpoint.Protocol != EndpointProtocolHTTP) {
			return fmt.Errorf("manifest: endpoint %q has unsupported protocol %q", endpoint.Name, endpoint.Protocol)
		}
		if endpoint.ContainerPort < 1 || endpoint.ContainerPort > 65535 {
			return fmt.Errorf("manifest: endpoint %q container_port must be between 1 and 65535", endpoint.Name)
		}
		if _, exists := names[endpoint.Name]; exists {
			return fmt.Errorf("manifest: duplicate endpoint name %q", endpoint.Name)
		}
		names[endpoint.Name] = struct{}{}
		normalized := strings.ToUpper(strings.ReplaceAll(endpoint.Name, "-", "_"))
		if previous, exists := normalizedNames[normalized]; exists {
			return fmt.Errorf("manifest: endpoint names %q and %q share normalized endpoint name %q", previous, endpoint.Name, normalized)
		}
		normalizedNames[normalized] = endpoint.Name
	}
	return nil
}
