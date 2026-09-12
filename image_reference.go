package contract

import (
	"fmt"
	"path"
	"regexp"
	"sort"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/opencontainers/go-digest"
)

var targetAppIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,30}[a-z0-9])?-[a-z0-9]{5}$`)

// ImageImportDecision 表示目标镜像 tag 在准备阶段应执行的动作。
type ImageImportDecision uint8

const (
	// ImageImportPublish 表示目标 tag 尚不存在，可以发布待导入镜像。
	ImageImportPublish ImageImportDecision = iota + 1
	// ImageImportReuse 表示目标 tag 已指向相同内容，无需重复发布。
	ImageImportReuse
)

// TargetImageReference 把包内源镜像改写为平台统一 Registry 引用。
// registryHost 只接受 Docker Registry 主机名，可带端口但不能带 URL scheme。
func TargetImageReference(registryHost, appID, source string) (string, error) {
	registry, err := name.NewRegistry(registryHost, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("mpk: registry host %q is invalid: %w", registryHost, err)
	}
	targetPath, err := targetImagePath(appID, source)
	if err != nil {
		return "", err
	}
	target, err := name.NewTag(registry.Name()+"/"+targetPath, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("mpk: target image reference is invalid: %w", err)
	}
	return target.Name(), nil
}

// DecideImageImport 根据目标 tag 的现有内容决定发布或复用。
// existingDigest 为空表示目标 tag 尚不存在；已存在但内容不同时必须拒绝，
// 由应用开发者修改源镜像 tag 后重新打包，平台不会覆盖现有 tag。
func DecideImageImport(target, existingDigest, incomingDigest string) (ImageImportDecision, error) {
	if _, err := name.NewTag(target, name.StrictValidation); err != nil {
		return 0, fmt.Errorf("mpk: target image reference %q is invalid: %w", target, err)
	}
	incoming, err := digest.Parse(incomingDigest)
	if err != nil {
		return 0, fmt.Errorf("mpk: incoming image digest %q is invalid: %w", incomingDigest, err)
	}
	if existingDigest == "" {
		return ImageImportPublish, nil
	}
	existing, err := digest.Parse(existingDigest)
	if err != nil {
		return 0, fmt.Errorf("mpk: existing image digest %q is invalid: %w", existingDigest, err)
	}
	if existing == incoming {
		return ImageImportReuse, nil
	}
	return 0, fmt.Errorf(
		"mpk: target image %q already points to digest %q instead of %q",
		target,
		existing,
		incoming,
	)
}

func targetImagePath(appID, source string) (string, error) {
	if !targetAppIDPattern.MatchString(appID) {
		return "", fmt.Errorf("mpk: application id %q is invalid", appID)
	}
	sourceReference, err := parseExplicitImageTag(source)
	if err != nil {
		return "", fmt.Errorf("mpk: source image reference is invalid: %w", err)
	}
	if isReservedTag(sourceReference.Identifier()) {
		return "", fmt.Errorf("mpk: source image reference %q uses a reserved tag", source)
	}
	basename := path.Base(sourceReference.Context().RepositoryStr())
	targetPath := appID + "/" + basename + ":" + sourceReference.Identifier()
	// 使用固定合法 Registry 复用库的严格 repository/path 校验，不自行维护正则。
	if _, err := name.NewTag("registry.invalid/"+targetPath, name.StrictValidation); err != nil {
		return "", fmt.Errorf("mpk: target image path %q is invalid: %w", targetPath, err)
	}
	return targetPath, nil
}

// validateTargetImageCollisions 在导入前拒绝不同源镜像映射到同一 app/basename:tag。
func validateTargetImageCollisions(appID string, sources map[string]struct{}) error {
	ordered := make([]string, 0, len(sources))
	for source := range sources {
		ordered = append(ordered, source)
	}
	sort.Strings(ordered)
	targetSources := make(map[string]string, len(ordered))
	for _, source := range ordered {
		target, err := targetImagePath(appID, source)
		if err != nil {
			return err
		}
		if previous, exists := targetSources[target]; exists && previous != source {
			return fmt.Errorf("mpk: source images %q and %q map to the same target image %q", previous, source, target)
		}
		targetSources[target] = source
	}
	return nil
}
