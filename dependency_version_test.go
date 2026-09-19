package contract

import (
	"errors"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	valid := []string{"0.0.0", "1.2.3", "1.2.3-alpha.1", "1.2.3+build.7", "1.2.3-alpha.1+build.7"}
	for _, value := range valid {
		if err := ValidateVersion(value); err != nil {
			t.Errorf("ValidateVersion(%q) = %v", value, err)
		}
	}
	invalid := []string{"", " v1.2.3", "v1.2.3", "01.2.3", "1.2", "1.2.3.", "1.2.3-"}
	for _, value := range invalid {
		if err := ValidateVersion(value); !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("ValidateVersion(%q) = %v, want ErrInvalidVersion", value, err)
		}
	}
}

func TestValidateDependencyConstraint(t *testing.T) {
	valid := []string{"1.2.3", "^1.2.3", "~1.2.3", ">=1.2.0", ">1.2.0", "<=2.0.0", ">=1.2.0 <2.0.0", ">1.2.0 <=2.0.0"}
	for _, value := range valid {
		if err := ValidateDependencyConstraint(value); err != nil {
			t.Errorf("ValidateDependencyConstraint(%q) = %v", value, err)
		}
	}
	invalid := []string{"*", "latest", "1.x", "1.2", "1.2.3 || 2.0.0", "1.2.3,2.0.0", "1.2.3 - 2.0.0", "v1.2.3", "1.2.3-alpha", ">=1.2.0 >=2.0.0", " >=1.2.0", ">=1.2.0  <2.0.0"}
	for _, value := range invalid {
		if err := ValidateDependencyConstraint(value); !errors.Is(err, ErrInvalidDependencyConstraint) {
			t.Errorf("ValidateDependencyConstraint(%q) = %v, want ErrInvalidDependencyConstraint", value, err)
		}
	}
}

func TestResolveDependencyVersionSelectsHighestStableCandidate(t *testing.T) {
	resolved, err := ResolveDependencyVersion("^1.2.0", []DependencyVersionCandidate{
		{Version: "1.2.0", PackageSHA256: "old"},
		{Version: "1.4.2", PackageSHA256: "new"},
		{Version: "2.0.0", PackageSHA256: "major"},
		{Version: "1.9.0-beta.1", PackageSHA256: "pre"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != "1.4.2" || resolved.PackageSHA256 != "new" {
		t.Fatalf("resolved = %+v, want 1.4.2/new", resolved)
	}
}

func TestResolveDependencyVersionNoMatch(t *testing.T) {
	_, err := ResolveDependencyVersion("^2.0.0", []DependencyVersionCandidate{{Version: "1.0.0"}})
	if !errors.Is(err, ErrNoSatisfyingVersion) {
		t.Fatalf("error = %v, want ErrNoSatisfyingVersion", err)
	}
}

func TestDependencyVersionSatisfies(t *testing.T) {
	matched, err := DependencyVersionSatisfies("1.4.2", ">=1.2.0 <2.0.0")
	if err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	matched, err = DependencyVersionSatisfies("2.0.0", "^1.2.0")
	if err != nil || matched {
		t.Fatalf("matched=%v err=%v, want false", matched, err)
	}
}

func TestResolveDependencyVersionForConstraints(t *testing.T) {
	resolved, err := ResolveDependencyVersionForConstraints([]string{">=1.0.0 <2.0.0", "<1.5.0"}, []DependencyVersionCandidate{
		{Version: "1.4.0", PackageSHA256: "sha-14"},
		{Version: "1.6.0", PackageSHA256: "sha-16"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Version != "1.4.0" || resolved.PackageSHA256 != "sha-14" {
		t.Fatalf("resolved = %+v", resolved)
	}
}
