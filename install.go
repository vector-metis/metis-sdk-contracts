// Package contract 保存平台与 Agent 共用的 MPK 安装计划与部署副本渲染规则。
package contract

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Setting 描述 manifest.yaml settings 映射中的单个配置项；类型保持封闭集合。
type Setting struct {
	Key      string   `yaml:"key" json:"key"`
	Label    string   `yaml:"label" json:"label"`
	Type     string   `yaml:"type" json:"type"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty"`
	Options  []string `yaml:"options,omitempty" json:"options,omitempty"`
}

// ModelSlotBinding 是一个模型插槽在安装前必须补齐的三元组。
type ModelSlotBinding struct {
	Endpoint string
	Model    string
	APIKey   string
	// CardParams 是模型卡片参数，键为 CONTEXT_WINDOW 等规范后缀。
	CardParams map[string]string
}

// InstallOptions 携带节点架构与平台注入事实；契约不访问数据库或外部系统。
type InstallOptions struct {
	Architecture     string
	BaseDir          string
	Settings         map[string]string
	ModelBindings    map[string]ModelSlotBinding
	ExtraEnvironment map[string]string
	// PublicHost 和 MasterIP 生成容器访问统一平台域名所需的固定 hosts 条目。
	PublicHost string
	MasterIP   string
	// ImageReferences 将包内源 tag 精确映射到平台统一 Registry 的完整目标 tag。
	ImageReferences map[string]string
	// ComposeOverrides 是按 service 分组的管理员模板快照，切勿由应用包直接提供。
	ComposeOverrides map[string][]ComposeOverride
	AssignPort       func(name string) (int, error)
	// ContractOnly 允许离线测试没有镜像归档；真实安装永远保持 false。
	ContractOnly bool
}

// InstallPlan 是平台执行前得到的只读安装计划，Compose 是展平后的部署副本。
type InstallPlan struct {
	Manifest    Manifest
	Settings    []Setting
	Compose     []byte
	Environment map[string]string
	Checks      []string
}

// InstallPackageSummary 是安装向导在选择节点前可安全展示的包元数据和配置定义。
type InstallPackageSummary struct {
	PackageSummary
	Settings []Setting
}

var (
	installPlaceholderPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)
	installPlaceholderName    = regexp.MustCompile(`^[A-Z0-9_]+$`)
	slotPattern               = regexp.MustCompile(`^METIS_(LLM|EMBEDDING|RERANK)_(\d+)_(ENDPOINT|MODEL|API_KEY)$`)
)

// InspectInstallPackage 完整校验 MPK，并读取 manifest 中的安装配置定义。
func InspectInstallPackage(reader io.ReadSeeker) (*InstallPackageSummary, error) {
	summary, err := ValidateMPK(reader, ValidateOptions{})
	if err != nil {
		return nil, err
	}
	return &InstallPackageSummary{PackageSummary: *summary, Settings: summary.Manifest.Settings}, nil
}

// PlanInstall 校验离线 MPK、选择节点架构、校验配置并生成平台注入后的 Compose。
func PlanInstall(reader io.ReadSeeker, options InstallOptions) (*InstallPlan, error) {
	if options.BaseDir == "" {
		return nil, fmt.Errorf("mpk: install base directory is required")
	}
	summary, err := ValidateMPK(reader, ValidateOptions{ContractOnly: options.ContractOnly})
	if err != nil {
		return nil, err
	}
	metadata, err := InspectPackageMetadata(reader, summary.Manifest.ID, summary.Manifest.Version)
	if err != nil {
		return nil, err
	}
	return planPreparedInstall(*metadata, options, false)
}

// PlanPreparedInstall 从 PackageLibrary 已验证的轻量快照生成安装计划。
// 每个 Compose 源镜像必须且只能映射到一个平台 Registry 目标 tag。
func PlanPreparedInstall(metadata PackageMetadata, options InstallOptions) (*InstallPlan, error) {
	if options.BaseDir == "" {
		return nil, fmt.Errorf("mpk: install base directory is required")
	}
	if err := metadata.Manifest.validateIdentity(); err != nil {
		return nil, err
	}
	return planPreparedInstall(metadata, options, true)
}

func planPreparedInstall(metadata PackageMetadata, options InstallOptions, requireImageMapping bool) (*InstallPlan, error) {
	manifest := metadata.Manifest
	arch := options.Architecture
	supported := false
	for _, item := range manifest.Architectures {
		supported = supported || item == arch
	}
	if !supported {
		return nil, fmt.Errorf("mpk: architecture %q is not supported by %q", arch, manifest.ID)
	}
	settings := metadata.Settings
	if len(settings) == 0 {
		settings = manifest.Settings
	}
	if err := validateSettings(settings, options.Settings); err != nil {
		return nil, err
	}
	composeName := "compose." + arch + ".yaml"
	compose, exists := metadata.Compose[arch]
	if !exists || strings.TrimSpace(compose) == "" {
		return nil, fmt.Errorf("mpk: missing %s for declared architecture", composeName)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(compose), &document); err != nil {
		return nil, fmt.Errorf("mpk: %s is invalid YAML: %w", composeName, err)
	}
	var raw struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &raw); err != nil {
		return nil, fmt.Errorf("mpk: %s is invalid YAML: %w", composeName, err)
	}
	if err := rewriteImageReferences(raw.Services, options.ImageReferences, requireImageMapping); err != nil {
		return nil, err
	}
	environment, checks, err := installEnvironment(manifest, settings, raw.Services, options)
	if err != nil {
		return nil, err
	}
	if err := assignApplicationPorts(manifest, raw.Services, environment, options.AssignPort); err != nil {
		return nil, err
	}
	addResourceLimits(raw.Services)
	injectPlatformEnvironment(manifest, raw.Services, environment)
	if err := injectPlatformHosts(raw.Services, options.PublicHost, options.MasterIP); err != nil {
		return nil, err
	}
	if err := applyComposeOverrides(manifest, raw.Services, options.ComposeOverrides); err != nil {
		return nil, err
	}
	document["services"] = raw.Services
	rendered, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("mpk: render install compose: %w", err)
	}
	checks = append(checks,
		"主入口恰好一个且只发布到 127.0.0.1",
		"managed mount root 与 subpath 已渲染为应用 scope 相对 source",
		"配置、端口、能力和资源默认值已写入部署副本",
	)
	if options.ContractOnly && !requireImageMapping {
		checks = append(checks, "契约模式跳过镜像归档校验")
	}
	return &InstallPlan{
		Manifest: manifest, Settings: settings, Compose: rendered, Environment: environment, Checks: checks,
	}, nil
}

// injectPlatformHosts 让所有容器把唯一平台逻辑域名解析到稳定 Master IP。
func injectPlatformHosts(services map[string]map[string]any, publicHost, masterIP string) error {
	publicHost, masterIP = strings.TrimSpace(publicHost), strings.TrimSpace(masterIP)
	if publicHost == "" || masterIP == "" {
		return fmt.Errorf("mpk: public host and master IP are required")
	}
	for serviceName, service := range services {
		if _, exists := service["extra_hosts"]; exists {
			return fmt.Errorf("mpk: service %q cannot declare extra_hosts", serviceName)
		}
		service["extra_hosts"] = []string{publicHost + ":" + masterIP}
	}
	return nil
}

func rewriteImageReferences(services map[string]map[string]any, references map[string]string, required bool) error {
	if len(services) == 0 {
		return fmt.Errorf("mpk: compose has no services")
	}
	if !required && len(references) == 0 {
		return nil
	}
	if len(references) == 0 {
		return fmt.Errorf("mpk: image mapping is required")
	}
	targets := make(map[string]string, len(references))
	for source, target := range references {
		if strings.TrimSpace(source) != source || strings.TrimSpace(target) != target || source == "" || target == "" {
			return fmt.Errorf("mpk: image mapping contains an empty or padded reference")
		}
		if _, err := parseExplicitImageTag(target); err != nil {
			return fmt.Errorf("mpk: target image %q is invalid: %w", target, err)
		}
		if previous, exists := targets[target]; exists && previous != source {
			return fmt.Errorf("mpk: image mappings %q and %q share target %q", previous, source, target)
		}
		targets[target] = source
	}
	used := make(map[string]struct{}, len(references))
	for serviceName, service := range services {
		source, ok := service["image"].(string)
		if !ok || source == "" {
			return fmt.Errorf("mpk: service %q has no image", serviceName)
		}
		target, exists := references[source]
		if !exists {
			return fmt.Errorf("mpk: image mapping is missing source %q", source)
		}
		service["image"] = target
		used[source] = struct{}{}
	}
	if len(used) != len(references) {
		for source := range references {
			if _, exists := used[source]; !exists {
				return fmt.Errorf("mpk: image mapping contains unused source %q", source)
			}
		}
	}
	return nil
}

// ModelSlots 在正式计划前读取所选架构声明的模型插槽，供平台先解析默认绑定。
func ModelSlots(reader io.ReadSeeker, architecture string) ([]string, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mpk: rewind package: %w", err)
	}
	defer func() { _, _ = reader.Seek(0, io.SeekStart) }()
	metadata, err := InspectPackageMetadata(reader, "", "")
	if err != nil {
		return nil, err
	}
	manifest := metadata.Manifest
	composeName := "compose." + architecture + ".yaml"
	var raw struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(metadata.Compose[architecture]), &raw); err != nil {
		return nil, fmt.Errorf("mpk: %s is invalid YAML: %w", composeName, err)
	}
	return modelSlots(manifest, raw.Services), nil
}

func modelSlots(manifest Manifest, services map[string]map[string]any) []string {
	seen := make(map[string]struct{})
	for _, declaration := range manifest.Services {
		for _, request := range declaration.Capabilities {
			for _, slot := range request.Slots {
				seen[slot] = struct{}{}
			}
		}
	}
	for _, service := range services {
		for _, placeholder := range placeholdersInRawService(service) {
			if !strings.HasPrefix(placeholder, "METIS_LLM_") && !strings.HasPrefix(placeholder, "METIS_EMBEDDING_") && !strings.HasPrefix(placeholder, "METIS_RERANK_") {
				continue
			}
			match := slotPattern.FindStringSubmatch(placeholder)
			if match == nil {
				continue
			}
			seen[strings.ToLower(match[1])+"."+match[2]] = struct{}{}
		}
	}
	slots := make([]string, 0, len(seen))
	for slot := range seen {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	return slots
}

// ModelSlotsFromMetadata 从准备快照读取所选架构的模型插槽，不重新扫描 MPK。
func ModelSlotsFromMetadata(metadata PackageMetadata, architecture string) ([]string, error) {
	compose, exists := metadata.Compose[architecture]
	if !exists || strings.TrimSpace(compose) == "" {
		return nil, fmt.Errorf("mpk: missing compose.%s.yaml for declared architecture", architecture)
	}
	var raw struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &raw); err != nil {
		return nil, fmt.Errorf("mpk: compose.%s.yaml is invalid YAML: %w", architecture, err)
	}
	return modelSlots(metadata.Manifest, raw.Services), nil
}

// validateSettings 只做类型和封闭选项校验；secret 不提供默认值。
func validateSettings(definitions []Setting, values map[string]string) error {
	for _, definition := range definitions {
		if !definition.Required {
			continue
		}
		value, exists := values[definition.Key]
		if !exists || value == "" {
			if definition.Default == "" {
				return fmt.Errorf("mpk: setting %q is required", definition.Key)
			}
			continue
		}
		switch definition.Type {
		case "string", "secret":
		case "number":
			var number float64
			if _, err := fmt.Sscan(value, &number); err != nil {
				return fmt.Errorf("mpk: setting %q is not a number", definition.Key)
			}
		case "bool":
			switch value {
			case "true", "false":
			default:
				return fmt.Errorf("mpk: setting %q is not a boolean", definition.Key)
			}
		case "select":
			allowed := false
			for _, option := range definition.Options {
				allowed = allowed || option == value
			}
			if !allowed {
				return fmt.Errorf("mpk: setting %q is outside allowed options", definition.Key)
			}
		default:
			return fmt.Errorf("mpk: setting %q has unsupported type %q", definition.Key, definition.Type)
		}
	}
	return nil
}

// installEnvironment 扫描白名单占位符并生成完整注入值；未知占位符大声失败。
func installEnvironment(manifest Manifest, definitions []Setting, services map[string]map[string]any, options InstallOptions) (map[string]string, []string, error) {
	placeholders := make(map[string]struct{})
	for _, service := range services {
		for _, name := range placeholdersInRawService(service) {
			placeholders[name] = struct{}{}
		}
	}
	for _, service := range manifest.Services {
		for _, source := range service.Environment {
			if source.Value != nil {
				for _, name := range installPlaceholders(*source.Value) {
					placeholders[name] = struct{}{}
				}
			}
		}
	}
	environment := map[string]string{
		"METIS_APP_ID":      manifest.ID,
		"METIS_APP_NAME":    manifest.DisplayName,
		"METIS_APP_VERSION": manifest.Version,
	}
	settingValues := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		value := definition.Default
		if supplied, exists := options.Settings[definition.Key]; exists {
			value = supplied
		}
		name := settingEnvironmentKey(definition.Key)
		environment[name] = value
		settingValues[name] = struct{}{}
	}
	slotNames := make([]string, 0)
	for name := range placeholders {
		switch {
		case name == "METIS_APP_ID" || name == "METIS_APP_NAME" || name == "METIS_APP_VERSION":
			continue
		case strings.HasPrefix(name, "METIS_SETTING_"):
			if _, declared := settingValues[name]; !declared {
				return nil, nil, fmt.Errorf("mpk: undeclared placeholder %s", name)
			}
		case strings.HasPrefix(name, "METIS_LLM_") || strings.HasPrefix(name, "METIS_EMBEDDING_") || strings.HasPrefix(name, "METIS_RERANK_"):
			slotNames = append(slotNames, name)
			continue
		default:
			if value, exists := options.ExtraEnvironment[name]; exists {
				environment[name] = value
				continue
			}
			return nil, nil, fmt.Errorf("mpk: no installation value for placeholder %s", name)
		}
	}
	if err := capabilityDefaults(manifest, options, environment); err != nil {
		return nil, nil, err
	}
	for slotName, slot := range manifest.Models {
		binding, exists := options.ModelBindings[slotName]
		if !exists {
			if IsSlotRequired(slotName, slot) {
				return nil, nil, fmt.Errorf("mpk: required model slot %s is not bound", slotName)
			}
			continue
		}
		parts := strings.SplitN(slotName, ".", 2)
		if len(parts) != 2 {
			return nil, nil, fmt.Errorf("mpk: invalid model slot %s", slotName)
		}
		prefix := "METIS_" + strings.ToUpper(parts[0]) + "_" + parts[1]
		environment[prefix+"_ENDPOINT"] = binding.Endpoint
		environment[prefix+"_MODEL"] = binding.Model
		environment[prefix+"_API_KEY"] = binding.APIKey
		for _, suffix := range modelTraitSuffixes(parts[0]) {
			if value, ok := binding.CardParams[suffix]; ok {
				environment[prefix+"_"+suffix] = value
			}
		}
		if slot.Interface == "" {
			return nil, nil, fmt.Errorf("mpk: model slot %s interface is required", slotName)
		}
	}
	sort.Strings(slotNames)
	for _, name := range slotNames {
		modelType, index, suffix, ok := parseModelPlaceholder(name)
		if !ok {
			return nil, nil, fmt.Errorf("mpk: invalid model placeholder %s", name)
		}
		slot := modelType + "." + index
		binding, exists := options.ModelBindings[slot]
		if !exists {
			slotDecl, hasSlot := manifest.Models[slot]
			if !hasSlot || IsSlotRequired(slot, slotDecl) {
				return nil, nil, fmt.Errorf("mpk: model slot %s is not bound", slot)
			}
			environment[name] = ""
			continue
		}
		switch suffix {
		case "ENDPOINT":
			environment[name] = binding.Endpoint
		case "MODEL":
			environment[name] = binding.Model
		case "API_KEY":
			environment[name] = binding.APIKey
		default:
			value, exists := binding.CardParams[suffix]
			if !exists || !slices.Contains(modelTraitSuffixes(modelType), suffix) {
				return nil, nil, fmt.Errorf("mpk: invalid model placeholder %s", name)
			}
			environment[name] = value
		}
	}
	return environment, []string{"安装计划占位符注入完成"}, nil
}

// assignApplicationPorts 根据 manifest 生成最终回环映射；源 Compose 不参与端口选择。
func assignApplicationPorts(
	manifest Manifest,
	services map[string]map[string]any,
	environment map[string]string,
	assign func(name string) (int, error),
) error {
	if assign == nil {
		return fmt.Errorf("mpk: port allocator is required")
	}
	for serviceName, service := range services {
		if _, exists := service["ports"]; exists {
			return fmt.Errorf("mpk: service %q cannot publish source ports", serviceName)
		}
	}
	addPort := func(serviceName, environmentName string, containerPort int, protocol EndpointProtocol) error {
		service, exists := services[serviceName]
		if !exists {
			return fmt.Errorf("mpk: endpoint service %q does not exist", serviceName)
		}
		port, err := assign(environmentName)
		if err != nil {
			return fmt.Errorf("mpk: assign %s: %w", environmentName, err)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("mpk: assign %s returned invalid port %d", environmentName, port)
		}
		environment[environmentName] = fmt.Sprintf("%d", port)
		composeProtocol := string(protocol)
		if protocol == EndpointProtocolHTTP {
			composeProtocol = string(EndpointProtocolTCP)
		}
		mapping := map[string]any{"target": containerPort, "published": fmt.Sprintf("${%s}", environmentName), "protocol": composeProtocol, "host_ip": "127.0.0.1"}
		ports, _ := service["ports"].([]any)
		service["ports"] = append(ports, mapping)
		return nil
	}

	switch manifest.Type {
	case ApplicationTypeWeb:
		for serviceName, declaration := range manifest.Services {
			for _, endpoint := range declaration.Endpoints {
				return addPort(serviceName, "METIS_ENTRY_PORT", endpoint.ContainerPort, endpoint.Protocol)
			}
		}
		return nil
	case ApplicationTypeService:
		for serviceName, declaration := range manifest.Services {
			for _, endpoint := range declaration.Endpoints {
				environmentName := "METIS_ENDPOINT_" + strings.ToUpper(strings.ReplaceAll(endpoint.Name, "-", "_")) + "_PORT"
				if err := addPort(serviceName, environmentName, endpoint.ContainerPort, endpoint.Protocol); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("mpk: unsupported application type %q", manifest.Type)
	}
}

func modelTraitSuffixes(modelType string) []string {
	switch modelType {
	case "llm":
		return []string{
			"CONTEXT_WINDOW", "MAX_INPUT_TOKENS", "MAX_OUTPUT_TOKENS",
			"SUPPORTS_VISION", "SUPPORTS_THINKING", "SUPPORTS_TOOLS",
		}
	case "embedding":
		return []string{"MAX_INPUT_TOKENS", "DIMENSIONS", "NORMALIZED"}
	case "rerank":
		return []string{"MAX_INPUT_TOKENS", "MAX_DOCUMENTS"}
	default:
		return nil
	}
}

// capabilityDefaults 把已声明能力的平台默认事实写入安装环境。
// 值来源是平台注入参数；应用不需要在 Compose 中重复声明这些占位符。
func capabilityDefaults(manifest Manifest, options InstallOptions, environment map[string]string) error {
	for _, name := range []string{"METIS_PLATFORM_ENDPOINT", "METIS_APP_TOKEN"} {
		value, exists := options.ExtraEnvironment[name]
		if !exists || value == "" {
			return fmt.Errorf("mpk: application identity requires %s", name)
		}
		environment[name] = value
	}
	if hasCapability(manifest, "object-storage") {
		for _, name := range []string{
			"METIS_S3_ENDPOINT", "METIS_S3_REGION", "METIS_S3_ACCESS_KEY",
			"METIS_S3_SECRET_KEY", "METIS_S3_BUCKET",
		} {
			value, exists := options.ExtraEnvironment[name]
			if !exists || value == "" {
				return fmt.Errorf("mpk: object storage requires %s", name)
			}
			environment[name] = value
		}
		environment["METIS_S3_SHARED_BUCKETS"] = options.ExtraEnvironment["METIS_S3_SHARED_BUCKETS"]
	}
	if hasCapability(manifest, "model-gateway") {
		if len(manifest.Models) == 0 {
			return fmt.Errorf("mpk: model-gateway requires at least one model slot")
		}
	}
	return nil
}

// injectPlatformEnvironment 把统一身份注入全部 service，
// 把 S3、模型、端口等敏感或具名运行事实限制在 manifest 明确声明的 service。
func injectPlatformEnvironment(manifest Manifest, services map[string]map[string]any, environment map[string]string) {
	for serviceName, service := range services {
		values, _ := service["environment"].(map[string]any)
		if values == nil {
			values = make(map[string]any)
		}
		if declaration, exists := manifest.Services[serviceName]; exists {
			if declaration.Lifecycle.Oneshot {
				service["restart"] = "no"
			} else {
				service["restart"] = declaration.Lifecycle.Restart
			}
			for name, source := range declaration.Environment {
				if source.Value != nil {
					values[name] = expandManifestEnvironmentValue(*source.Value, environment)
					continue
				}
				if source.Setting != nil {
					values[name] = environment[settingEnvironmentKey(*source.Setting)]
				}
			}
			if len(declaration.Mounts) > 0 {
				mounts := make([]any, 0, len(declaration.Mounts))
				for _, mount := range declaration.Mounts {
					source := "./" + managedMountPath(mount)
					mounts = append(mounts, map[string]any{"type": "bind", "source": source, "target": mount.Target, "read_only": mount.ReadOnly, "bind": map[string]any{"create_host_path": false}})
				}
				service["volumes"] = mounts
			}
		}
		for _, name := range []string{"METIS_APP_ID", "METIS_PLATFORM_ENDPOINT", "METIS_APP_TOKEN"} {
			values[name] = environment[name]
		}
		if declaration, exists := manifest.Services[serviceName]; exists {
			if _, requested := declaration.Capabilities["object-storage"]; requested {
				for name, value := range environment {
					if strings.HasPrefix(name, "METIS_S3_") {
						values[name] = value
					}
				}
			}
			if request, requested := declaration.Capabilities["model-gateway"]; requested {
				for _, slotName := range request.Slots {
					prefix := modelEnvironmentPrefix(slotName)
					for name, value := range environment {
						if strings.HasPrefix(name, prefix) && strings.TrimSpace(value) != "" {
							values[name] = value
						}
					}
				}
			}
			for _, endpoint := range declaration.Endpoints {
				name := "METIS_ENDPOINT_" + strings.ToUpper(strings.ReplaceAll(endpoint.Name, "-", "_")) + "_PORT"
				if endpoint.Protocol == EndpointProtocolHTTP {
					name = "METIS_ENTRY_PORT"
				}
				if value, exists := environment[name]; exists {
					values[name] = value
				}
			}
		}
		service["environment"] = values
	}
}

func settingEnvironmentKey(key string) string {
	return "METIS_SETTING_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(key))
}

func modelEnvironmentPrefix(slotName string) string {
	parts := strings.SplitN(slotName, ".", 2)
	if len(parts) != 2 {
		return "METIS_INVALID_SLOT_"
	}
	return "METIS_" + strings.ToUpper(parts[0]) + "_" + parts[1] + "_"
}

func expandManifestEnvironmentValue(value string, environment map[string]string) string {
	return installPlaceholderPattern.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if replacement, exists := environment[name]; exists {
			return replacement
		}
		return match
	})
}

// placeholdersInRawService 递归收集字符串值中的占位符，保留 Compose 原始字段。
func placeholdersInRawService(service map[string]any) []string {
	names := make([]string, 0)
	var visit func(value any)
	visit = func(value any) {
		switch item := value.(type) {
		case string:
			names = append(names, installPlaceholders(item)...)
		case []any:
			for _, child := range item {
				visit(child)
			}
		case map[string]any:
			for _, child := range item {
				visit(child)
			}
		case map[any]any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(service)
	return names
}

// installPlaceholders 收集平台安装占位符，同时跳过 Compose 的 $$ 转义。
// `$${NAME}` 必须原样留给容器内 shell；只有未转义的 `${NAME}` 才属于安装契约。
func installPlaceholders(value string) []string {
	names := make([]string, 0)
	for index := 0; index < len(value); {
		if value[index] != '$' {
			index++
			continue
		}
		if index+1 < len(value) && value[index+1] == '$' {
			index += 2
			continue
		}
		if index+1 >= len(value) || value[index+1] != '{' {
			index++
			continue
		}
		end := strings.IndexByte(value[index+2:], '}')
		if end < 0 {
			index += 2
			continue
		}
		end += index + 2
		name := value[index+2 : end]
		if installPlaceholderName.MatchString(name) {
			names = append(names, name)
		}
		index = end + 1
	}
	return names
}

// addResourceLimits 按 ADR-0058 写入通用资源默认值；非标资源由最终覆盖模板负责。
func addResourceLimits(services map[string]map[string]any) {
	for _, service := range services {
		service["cpus"], service["mem_limit"] = 2, "2GiB"
	}
}
