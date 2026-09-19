package contract

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// 依赖版本语法是故意收敛的闭集；不要把商店实现成 npm 的完整 range 解析器。
var strictVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

var (
	// ErrInvalidVersion 表示不符合严格 SemVer 2.0 的版本。
	ErrInvalidVersion = errors.New("contract: invalid semantic version")
	// ErrInvalidDependencyConstraint 表示不属于平台支持子集的依赖约束。
	ErrInvalidDependencyConstraint = errors.New("contract: invalid dependency version constraint")
	// ErrNoSatisfyingVersion 表示候选集合中没有满足约束的稳定版本。
	ErrNoSatisfyingVersion = errors.New("contract: no satisfying dependency version")
)

// ValidateVersion 校验不带 v 前缀的严格 SemVer。预发布版本对 manifest.version 合法，
// 但稳定依赖解析会单独排除预发布候选。
func ValidateVersion(version string) error {
	if version == "" || strings.TrimSpace(version) != version || strings.HasPrefix(version, "v") || !strictVersionPattern.MatchString(version) {
		return fmt.Errorf("%w: %q must be SemVer without a v prefix", ErrInvalidVersion, version)
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidVersion, version, err)
	}
	return nil
}

var constraintTokenPattern = regexp.MustCompile(`^(?:(\^|~|>=|<=|>|<)?)([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?)$`)

// ValidateDependencyConstraint 只接受精确版本、^、~、单比较器和两个有序比较器。
// 不支持通配符、latest、||、逗号、连字符范围、预发布选择器或隐式 v 前缀。
func ValidateDependencyConstraint(constraint string) error {
	if constraint == "" || strings.TrimSpace(constraint) != constraint || strings.ContainsAny(constraint, "\t\r\n") || strings.Contains(constraint, "||") || strings.Contains(constraint, ",") {
		return fmt.Errorf("%w: %q; supported forms are 1.2.3, ^1.2.3, ~1.2.3, >=1.2.0, and >=1.2.0 <2.0.0", ErrInvalidDependencyConstraint, constraint)
	}
	parts := strings.Split(constraint, " ")
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("%w: %q contains repeated spaces", ErrInvalidDependencyConstraint, constraint)
		}
	}
	if len(parts) > 2 {
		return fmt.Errorf("%w: %q has too many comparators", ErrInvalidDependencyConstraint, constraint)
	}
	for index, part := range parts {
		match := constraintTokenPattern.FindStringSubmatch(part)
		if match == nil {
			return fmt.Errorf("%w: unsupported term %q", ErrInvalidDependencyConstraint, part)
		}
		version := match[2]
		if err := ValidateVersion(version); err != nil {
			return fmt.Errorf("%w: %q", ErrInvalidDependencyConstraint, err)
		}
		parsed, _ := semver.StrictNewVersion(version)
		if parsed.Prerelease() != "" {
			return fmt.Errorf("%w: prerelease selectors are not allowed: %q", ErrInvalidDependencyConstraint, version)
		}
		if len(parts) == 2 && match[1] == "" {
			return fmt.Errorf("%w: a range requires explicit comparators", ErrInvalidDependencyConstraint)
		}
		if len(parts) == 2 {
			if index == 0 && match[1] != ">=" && match[1] != ">" {
				return fmt.Errorf("%w: lower bound must use >= or >", ErrInvalidDependencyConstraint)
			}
			if index == 1 && match[1] != "<" && match[1] != "<=" {
				return fmt.Errorf("%w: upper bound must use < or <=", ErrInvalidDependencyConstraint)
			}
		}
	}
	if _, err := semver.NewConstraint(constraint); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidDependencyConstraint, constraint, err)
	}
	return nil
}

// DependencyVersionCandidate 是目录或测试适配器提供给解析器的最小候选事实。
type DependencyVersionCandidate struct {
	Version       string
	PackageSHA256 string
}

// ResolveDependencyVersion 从已过滤的稳定候选中返回最高满足版本和摘要。
// 调用方负责先过滤应用 ID、架构、发布状态和撤回状态。
func ResolveDependencyVersion(constraint string, candidates []DependencyVersionCandidate) (DependencyVersionCandidate, error) {
	return ResolveDependencyVersionForConstraints([]string{constraint}, candidates)
}

// ResolveDependencyVersionForConstraints 在多个依赖边指向同一应用时求约束交集，
// 并返回满足全部约束的最高稳定版本。调用方负责先过滤架构和发布状态。
func ResolveDependencyVersionForConstraints(constraints []string, candidates []DependencyVersionCandidate) (DependencyVersionCandidate, error) {
	if len(constraints) == 0 {
		return DependencyVersionCandidate{}, fmt.Errorf("%w: no constraints", ErrInvalidDependencyConstraint)
	}
	ranges := make([]*semver.Constraints, 0, len(constraints))
	for _, constraint := range constraints {
		if err := ValidateDependencyConstraint(constraint); err != nil {
			return DependencyVersionCandidate{}, err
		}
		rangeConstraint, err := semver.NewConstraint(constraint)
		if err != nil {
			return DependencyVersionCandidate{}, fmt.Errorf("%w: %v", ErrInvalidDependencyConstraint, err)
		}
		ranges = append(ranges, rangeConstraint)
	}
	type parsedCandidate struct {
		candidate DependencyVersionCandidate
		version   *semver.Version
	}
	parsed := make([]parsedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ValidateVersion(candidate.Version); err != nil {
			return DependencyVersionCandidate{}, fmt.Errorf("%w: candidate %q: %v", ErrInvalidVersion, candidate.Version, err)
		}
		version, _ := semver.StrictNewVersion(candidate.Version)
		if version.Prerelease() != "" || !allConstraintsMatch(ranges, version) {
			continue
		}
		parsed = append(parsed, parsedCandidate{candidate: candidate, version: version})
	}
	if len(parsed) == 0 {
		return DependencyVersionCandidate{}, fmt.Errorf("%w: %q", ErrNoSatisfyingVersion, strings.Join(constraints, " "))
	}
	sort.SliceStable(parsed, func(left, right int) bool {
		return parsed[left].version.GreaterThan(parsed[right].version)
	})
	return parsed[0].candidate, nil
}

func allConstraintsMatch(constraints []*semver.Constraints, version *semver.Version) bool {
	for _, constraint := range constraints {
		if !constraint.Check(version) {
			return false
		}
	}
	return true
}

// DependencyVersionSatisfies 判断一个稳定版本是否满足约束，用于反向依赖升级检查。
func DependencyVersionSatisfies(version, constraint string) (bool, error) {
	if err := ValidateVersion(version); err != nil {
		return false, err
	}
	if err := ValidateDependencyConstraint(constraint); err != nil {
		return false, err
	}
	parsedVersion, _ := semver.StrictNewVersion(version)
	parsedConstraint, _ := semver.NewConstraint(constraint)
	return parsedVersion.Prerelease() == "" && parsedConstraint.Check(parsedVersion), nil
}
