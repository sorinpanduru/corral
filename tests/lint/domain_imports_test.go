package lint_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/importcheck"
)

const domainPrefix = "github.com/sorinpanduru/corral/internal/domain"

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root go.mod not found: %v", err)
	}
	return root
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "fixture")
}

func render(vs []importcheck.Violation) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "%+v\n", v)
	}
	return b.String()
}

// AC-1 / FR-1: the real domain packages respect the import boundary.
func TestAC1_FR1_DomainPackagesRespectImportBoundary(t *testing.T) {
	vs, err := importcheck.Check(repoRoot(t), "./internal/domain/...", domainPrefix, importcheck.DomainAllowlist)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("domain import violations:\n%s", render(vs))
	}
}

// FR-1(c): the allowlist is currently empty; adding to it needs a spec amendment.
func TestFR1_DomainAllowlistIsEmpty(t *testing.T) {
	if len(importcheck.DomainAllowlist) != 0 {
		t.Fatalf("DomainAllowlist must be empty, got %v", importcheck.DomainAllowlist)
	}
}

const fixturePrefix = "fixture/internal/domain"

// AC-1 self-test: the violating fixture yields exactly the expected
// violations, attributed to the right package, sorted by package then import.
func TestAC1_SelfTest_ViolatingFixtureExactViolations(t *testing.T) {
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", fixturePrefix, nil)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	want := []importcheck.Violation{
		{Package: "fixture/internal/domain/bad", Import: "fixture/internal/infra/thing"},
		{Package: "fixture/internal/domain/bad", Import: "log/slog"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// FR-1: the allowlist is matched against direct imports only. With thing
// allowlisted, its own non-stdlib import (deep) is not reported; only
// log/slog remains.
func TestFR1_SelfTest_AllowlistDirectImportsOnly(t *testing.T) {
	allow := []string{"fixture/internal/infra/thing"}
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", fixturePrefix, allow)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	want := []importcheck.Violation{{Package: "fixture/internal/domain/bad", Import: "log/slog"}}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// Guard against a vacuous pass: Scan reports exactly the domain packages
// examined (sorted import paths), and Check examines the same set. The
// fixture module path is dotless, so only go list's Standard flag separates
// fixture packages from the standard library.
func TestFR1_SelfTest_ScanListsDomainPackages(t *testing.T) {
	got, err := importcheck.Scan(fixtureDir(t), "./...", fixturePrefix)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	want := []string{
		"fixture/internal/domain/bad",
		"fixture/internal/domain/good",
		"fixture/internal/domain/helper",
		"fixture/internal/domain/testonly",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan = %v, want %v", got, want)
	}
}

// Guard against a vacuous pass on the real tree: the health package is scanned.
func TestAC1_RealTreeScansHealthPackage(t *testing.T) {
	got, err := importcheck.Scan(repoRoot(t), "./internal/domain/...", domainPrefix)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if !slices.Contains(got, domainPrefix+"/health") {
		t.Fatalf("Scan = %v, missing %s/health", got, domainPrefix)
	}
}

// Failure behaviour: a pattern that matches no domain package is an error
// (nothing checked must not be a pass). The pattern matches only infra packages.
func TestFR1_NoDomainPackagesMatchedIsError(t *testing.T) {
	for _, pat := range []string{"./internal/infra/...", "./internal/nomatch/..."} {
		if _, err := importcheck.Check(fixtureDir(t), pat, fixturePrefix, nil); err == nil {
			t.Errorf("Check(%q): expected error when no domain packages match", pat)
		}
		if _, err := importcheck.Scan(fixtureDir(t), pat, fixturePrefix); err == nil {
			t.Errorf("Scan(%q): expected error when no domain packages match", pat)
		}
	}
}

// Security (advisory A1): a pattern that could be parsed as a go list flag is rejected.
func TestFR1_PatternStartingWithDashRejected(t *testing.T) {
	for _, pat := range []string{"-json", "-toolexec=/bin/true", "--help"} {
		if _, err := importcheck.Check(fixtureDir(t), pat, fixturePrefix, nil); err == nil {
			t.Errorf("Check(%q): expected error", pat)
		}
		if _, err := importcheck.Scan(fixtureDir(t), pat, fixturePrefix); err == nil {
			t.Errorf("Scan(%q): expected error", pat)
		}
	}
}

// FR-1: compliant packages and _test.go imports are not reported.
func TestFR1_SelfTest_CompliantAndTestOnlyImportsNotReported(t *testing.T) {
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", fixturePrefix, nil)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	for _, v := range vs {
		if v.Package != "fixture/internal/domain/bad" {
			t.Errorf("unexpected violation outside bad: %+v", v)
		}
	}
}

// Failure behaviour: an unusable directory is an error, not a silent pass.
func TestFR1_SelfTest_BadDirReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := importcheck.Check(missing, "./...", domainPrefix, nil); err == nil {
		t.Fatal("expected error for a missing directory")
	}
	if _, err := importcheck.Scan(missing, "./...", domainPrefix); err == nil {
		t.Fatal("expected Scan error for a missing directory")
	}
}
