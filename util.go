package contract

import (
	"regexp"
	"strings"
)

var modelPlaceholderPattern = regexp.MustCompile(`^METIS_(LLM|EMBEDDING|RERANK)_(\d+)_([A-Z0-9_]+)$`)

// parseModelPlaceholder 解析模型插槽占位符，区分调用三元组和类型化卡片参数。
func parseModelPlaceholder(name string) (modelType, index, suffix string, ok bool) {
	match := modelPlaceholderPattern.FindStringSubmatch(name)
	if match == nil {
		return "", "", "", false
	}
	return strings.ToLower(match[1]), match[2], match[3], true
}

func hasCapability(manifest Manifest, capability string) bool {
	for _, candidate := range manifest.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func isReservedTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "latest", "stable", "current":
		return true
	default:
		return false
	}
}
