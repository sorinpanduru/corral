package lint_test

import (
	"reflect"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/importcheck"
)

// SPEC-003 AC-14 (FR-16): the SPEC-001 AC-1 import check additionally fails
// if a domain package imports the settings registry or the loader package.
// The check is the existing rule (stdlib and domain only); the fixture proves
// it reports exactly these imports, and the real tree proves no domain
// package imports any configuration package.

const cfgFixturePrefix = "cfgfix/internal/domain"

func cfgFixtureDir(t *testing.T) string { return fixtureDirNamed(t, "configfixture") }

func TestAC14_FixtureReportsRegistryAndLoaderImports(t *testing.T) {
	vs, err := importcheck.Check(cfgFixtureDir(t), "./internal/domain/...", cfgFixturePrefix, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []importcheck.Violation{
		{Package: "cfgfix/internal/domain/cfgbad", Import: "cfgfix/internal/infra/config/loader"},
		{Package: "cfgfix/internal/domain/cfgbad", Import: "cfgfix/internal/infra/config/registry"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// Non-vacuity: both fixture domain packages are examined; the compliant one
// is not reported.
func TestAC14_FixtureScansBothPackages(t *testing.T) {
	got, err := importcheck.Scan(cfgFixtureDir(t), "./internal/domain/...", cfgFixturePrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cfgfix/internal/domain/cfgbad", "cfgfix/internal/domain/cfggood"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan = %v, want %v", got, want)
	}
}

// Real tree: no domain package imports a configuration package, with the
// explicit prefixes of the registry, loader, types, source, settings and the
// config root (importcheck.Forbidden, direct imports).
func TestAC14_RealDomainImportsNoConfigPackage(t *testing.T) {
	const mod = "github.com/sorinpanduru/corral/internal/"
	vs, err := importcheck.Forbidden(repoRoot(t), "./internal/domain/...", nil, []string{
		mod + "infra/config", mod + "app/settings", mod + "app/configload", mod + "infra/osfile",
	})
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("domain packages import configuration packages:\n%s", render(vs))
	}
}
