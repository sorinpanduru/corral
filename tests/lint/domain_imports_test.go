package lint_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	_, file, _, _ := runtime.Caller(0)
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

// AC-1 self-test: the violating fixture is reported, with both offending imports.
func TestAC1_SelfTest_ViolatingFixtureFails(t *testing.T) {
	prefix := "example.test/fixture/internal/domain"
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", prefix, nil)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if len(vs) < 2 {
		t.Fatalf("expected at least 2 violations (log/slog and non-stdlib), got %d:\n%s", len(vs), render(vs))
	}
	out := render(vs)
	for _, want := range []string{"log/slog", "example.test/fixture/internal/infra/thing", "example.test/fixture/internal/domain/bad"} {
		if !strings.Contains(out, want) {
			t.Errorf("violations do not mention %q:\n%s", want, out)
		}
	}
}

// FR-1: compliant packages (stdlib, other domain packages) and _test.go
// imports (Test/XTestImports) are not reported.
func TestFR1_SelfTest_CompliantAndTestOnlyImportsNotReported(t *testing.T) {
	prefix := "example.test/fixture/internal/domain"
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", prefix, nil)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	out := render(vs)
	for _, bad := range []string{"domain/good", "domain/helper", "domain/testonly"} {
		if strings.Contains(out, bad) {
			t.Errorf("compliant package %q reported:\n%s", bad, out)
		}
	}
}

// FR-1(c): an explicit allowlist entry permits that import; log/slog stays forbidden.
func TestFR1_SelfTest_AllowlistPermitsEntry(t *testing.T) {
	prefix := "example.test/fixture/internal/domain"
	allow := []string{"example.test/fixture/internal/infra/thing"}
	vs, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", prefix, allow)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected exactly 1 violation (log/slog), got %d:\n%s", len(vs), render(vs))
	}
	out := render(vs)
	if !strings.Contains(out, "log/slog") || strings.Contains(out, "infra/thing") {
		t.Fatalf("unexpected violation set:\n%s", out)
	}
}

// Failure behaviour: an unusable directory is an error, not a silent pass.
func TestFR1_SelfTest_BadDirReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := importcheck.Check(missing, "./...", domainPrefix, nil); err == nil {
		t.Fatal("expected error for a missing directory")
	}
}
