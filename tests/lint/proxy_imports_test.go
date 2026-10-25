package lint_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/importcheck"
)

// SPEC-002 T29: boundary lint for the proxy packages (AC-29, AC-48 static
// part, FR-53, FR-64).

const (
	mongoDriverPrefix = "go.mongodb.org/mongo-driver"
	twinsPrefix       = modulePath + "/twins"
)

// AC-29 / FR-53: the domain import check matches every new domain package, and
// passes.
func TestT29_DomainCheckMatchesProxyPackages_AC29(t *testing.T) {
	got, err := importcheck.Scan(repoRoot(t), "./internal/domain/...", domainPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bson", "clock", "wire", "proxy", "pool", "upstream", "health"} {
		if !slices.Contains(got, domainPrefix+"/"+p) {
			t.Errorf("the domain check does not match %s/%s (matched: %v)", domainPrefix, p, got)
		}
	}
	vs, err := importcheck.Check(repoRoot(t), "./internal/domain/...", domainPrefix, importcheck.DomainAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("domain import violations:\n%s", render(vs))
	}
}

// T29: no production package imports the MongoDB driver, twins/... or
// internal/testkit/.... The driver, the twins and the test kit themselves, and
// tests, are the only users.
func TestT29_ProductionCodeImportsNoDriverTwinsOrTestkit(t *testing.T) {
	vs, err := importcheck.Forbidden(repoRoot(t), "./...",
		[]string{testkitPrefix, twinsPrefix},
		[]string{mongoDriverPrefix, twinsPrefix, testkitPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("production code imports the driver, twins or the test kit:\n%s", render(vs))
	}
}

// T29: the default binary's dependency closure holds no driver, twin or test
// kit package.
func TestT29_DefaultBinaryClosureHasNoTestInfrastructure(t *testing.T) {
	got, err := importcheck.ForbiddenDeps(repoRoot(t), "./cmd/...", "linux", "amd64",
		[]string{mongoDriverPrefix, twinsPrefix, testkitPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the default build depends on %v", got)
	}
}

func listDeps(t *testing.T, tags string) []string {
	t.Helper()
	args := []string{"list", "-deps", "-f", "{{.ImportPath}}|{{.Dir}}|{{join .GoFiles \",\"}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/corral")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = goEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// AC-48 static part, FR-64: the default build's dependency closure contains no
// hook or probe package, and no compiled file mentions a CORRALTEST_ variable.
func TestT29_DefaultBuildHasNoHookOrProbeCode_AC48(t *testing.T) {
	for _, l := range listDeps(t, "") {
		parts := strings.SplitN(l, "|", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[0], modulePath) {
			continue
		}
		rel := strings.TrimPrefix(parts[0], modulePath)
		for _, w := range strings.Split(rel, "/") {
			if strings.Contains(w, "probe") || strings.Contains(w, "corraltest") || w == "hooks" {
				t.Errorf("package %s (a hook or probe package) is in the default closure", parts[0])
			}
		}
		for _, f := range strings.Split(parts[2], ",") {
			if f == "" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(parts[1], f))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "CORRALTEST_") {
				t.Errorf("%s/%s is compiled into the default build and mentions CORRALTEST_", parts[0], f)
			}
		}
	}
}

// FR-64: every reference to CORRALTEST_ in non-test Go code of the product
// (outside the test kit, twins and tests) is in a file with the corraltest
// build constraint.
func TestT29_CorraltestReferencesOnlyUnderTheTag_FR64(t *testing.T) {
	root := repoRoot(t)
	checked := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				if rel == filepath.Join("internal", "testkit") || d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			checked++
			if !strings.Contains(string(b), "CORRALTEST_") {
				return nil
			}
			c := firstConstraint(t, p)
			if !strings.Contains(c, "corraltest") || strings.Contains(c, "!corraltest") {
				t.Errorf("%s mentions CORRALTEST_ without the corraltest build constraint (constraint %q)", rel, c)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d files scanned: the walk is vacuous", checked)
	}
}

// T29: the corraltest-only files of cmd/corral carry the constraint, and the
// hook code is not reachable from the production build (extends SPEC-003 T14).
func TestT29_HookFilesCarryTheConstraint(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "cmd", "corral")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, n))
		if strings.Contains(string(b), "CORRALTEST_") && firstConstraint(t, filepath.Join(dir, n)) != "corraltest" {
			t.Errorf("cmd/corral/%s mentions CORRALTEST_ and lacks the exact corraltest constraint", n)
		}
	}
}

// T29: the new packages are covered by the constant-message lint (constmsg
// runs over ./..., so the packages must be part of it).
func TestT29_NewPackagesAreInTheLintedTree(t *testing.T) {
	got, err := importcheck.Scan(repoRoot(t), "./...", modulePath+"/internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"infra/proxyserver", "infra/upstreamconn", "app/proxyapp", "domain/proxy", "domain/pool", "domain/upstream", "domain/wire"} {
		if !slices.Contains(got, modulePath+"/internal/"+p) {
			t.Errorf("%s is not part of the linted tree", p)
		}
	}
}
