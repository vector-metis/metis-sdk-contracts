package contract

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

var strictVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var constraintTokenPattern = regexp.MustCompile(`^(?:(\^|~|>=|<=|>|<)?)([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?)$`)

var (
	ErrInvalidVersion              = errors.New("contract: invalid semantic version")
	ErrInvalidDependencyConstraint = errors.New("contract: invalid dependency version constraint")
	ErrNoSatisfyingVersion         = errors.New("contract: no satisfying dependency version")
)

// ValidateVersion 校验不带 v 前缀的严格 SemVer。
func ValidateVersion(version string) error {
	if version == "" || strings.TrimSpace(version) != version || strings.HasPrefix(version, "v") || !strictVersionPattern.MatchString(version) {
		return fmt.Errorf("%w: %q must be SemVer without a v prefix", ErrInvalidVersion, version)
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidVersion, version, err)
	}
	return nil
}

// ValidateDependencyConstraint 校验平台支持的有限约束语法。
func ValidateDependencyConstraint(constraint string) error {
	if constraint == "" || strings.TrimSpace(constraint) != constraint || strings.ContainsAny(constraint, "\t\r\n") || strings.Contains(constraint, "||") || strings.Contains(constraint, ",") {
		return fmt.Errorf("%w: %q", ErrInvalidDependencyConstraint, constraint)
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
		if err := ValidateVersion(match[2]); err != nil {
			return fmt.Errorf("%w: %q", ErrInvalidDependencyConstraint, err)
		}
		parsed, _ := semver.StrictNewVersion(match[2])
		if parsed.Prerelease() != "" {
			return fmt.Errorf("%w: prerelease selectors are not allowed: %q", ErrInvalidDependencyConstraint, match[2])
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

type DependencyVersionCandidate struct {
	Version       string
	PackageSHA256 string
}

func ResolveDependencyVersion(constraint string, candidates []DependencyVersionCandidate) (DependencyVersionCandidate, error) {
	return ResolveDependencyVersionForConstraints([]string{constraint}, candidates)
}

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
		if version.Prerelease() == "" && allConstraintsMatch(ranges, version) {
			parsed = append(parsed, parsedCandidate{candidate: candidate, version: version})
		}
	}
	if len(parsed) == 0 {
		return DependencyVersionCandidate{}, fmt.Errorf("%w: %q", ErrNoSatisfyingVersion, strings.Join(constraints, " "))
	}
	sort.SliceStable(parsed, func(left, right int) bool { return parsed[left].version.GreaterThan(parsed[right].version) })
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
