package lint_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/importcheck"
)

const (
	modulePath    = "github.com/sorinpanduru/corral"
	testkitPrefix = modulePath + "/internal/testkit"
	xToolsPrefix  = "golang.org/x/tools"

	tkFixtureTestkit = "tkfixture/internal/testkit"
	tkFixtureTools   = "tkfixture/external/tools"
)

func tkFixtureDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "testkitfixture")
}

// T21(a) / ADR-0001: no production package outside internal/testkit/...
// imports internal/testkit/... or golang.org/x/tools/...
func TestT21_ProductionCodeDoesNotImportTestInfrastructure(t *testing.T) {
	vs, err := importcheck.Forbidden(repoRoot(t), "./...",
		[]string{testkitPrefix}, []string{testkitPrefix, xToolsPrefix})
	if err != nil {
		t.Fatalf("Forbidden returned error: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("production code imports test infrastructure:\n%s", render(vs))
	}
}

// Guard against a vacuous pass on the real tree: ./... is non-empty outside
// the exclude prefix (Scan sees production packages).
func TestT21_RealTreeScansProductionPackages(t *testing.T) {
	got, err := importcheck.Scan(repoRoot(t), "./...", modulePath+"/internal")
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if !slices.Contains(got, domainPrefix+"/health") {
		t.Fatalf("Scan = %v, missing %s/health", got, domainPrefix)
	}
}

// T21(a) self-test: the violating fixture yields exactly the expected
// violations. Excluded testkit packages, test-file imports and prefix
// look-alikes (testkitx) are not reported. Sorted by package then import.
func TestT21_SelfTest_ViolatingFixtureExactViolations(t *testing.T) {
	vs, err := importcheck.Forbidden(tkFixtureDir(t), "./...",
		[]string{tkFixtureTestkit}, []string{tkFixtureTestkit, tkFixtureTools})
	if err != nil {
		t.Fatalf("Forbidden returned error: %v", err)
	}
	want := []importcheck.Violation{
		{Package: "tkfixture/internal/app/bad", Import: tkFixtureTools},
		{Package: "tkfixture/internal/app/bad", Import: "tkfixture/internal/testkit/kit"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// Without an exclude, the testkit helper package is also reported: proves the
// exclude list is what suppresses it.
func TestT21_SelfTest_ExcludeSuppressesTestkitImporters(t *testing.T) {
	vs, err := importcheck.Forbidden(tkFixtureDir(t), "./...",
		nil, []string{tkFixtureTestkit})
	if err != nil {
		t.Fatalf("Forbidden returned error: %v", err)
	}
	want := []importcheck.Violation{
		{Package: "tkfixture/internal/app/bad", Import: "tkfixture/internal/testkit/kit"},
		{Package: "tkfixture/internal/testkit/helper", Import: "tkfixture/internal/testkit/kit"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// Prefix matching is on path boundaries: forbidding ".../internal/testkit"
// does not match ".../internal/testkitx".
func TestT21_SelfTest_PrefixMatchesOnPathBoundary(t *testing.T) {
	vs, err := importcheck.Forbidden(tkFixtureDir(t), "./...",
		[]string{tkFixtureTestkit}, []string{tkFixtureTestkit})
	if err != nil {
		t.Fatalf("Forbidden returned error: %v", err)
	}
	for _, v := range vs {
		if strings.Contains(v.Import, "testkitx") || v.Package == "tkfixture/internal/app/lookalike" {
			t.Errorf("look-alike prefix wrongly reported: %+v", v)
		}
	}
	// Forbidding the look-alike itself is reported for the importer.
	vs, err = importcheck.Forbidden(tkFixtureDir(t), "./...",
		nil, []string{"tkfixture/internal/testkitx"})
	if err != nil {
		t.Fatalf("Forbidden returned error: %v", err)
	}
	want := []importcheck.Violation{
		{Package: "tkfixture/internal/app/lookalike", Import: "tkfixture/internal/testkitx"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("violations =\n%s\nwant\n%s", render(vs), render(want))
	}
}

// Failure behaviour: when every matched package is under an exclude prefix,
// nothing was checked, which must be an error and never a silent pass.
func TestT21_EmptyMatchIsError(t *testing.T) {
	for _, pat := range []string{"./internal/testkit/...", "./internal/nomatch/..."} {
		vs, err := importcheck.Forbidden(tkFixtureDir(t), pat,
			[]string{tkFixtureTestkit}, []string{tkFixtureTestkit})
		if err == nil {
			t.Errorf("Forbidden(%q): expected error for empty match, got %v", pat, vs)
		}
	}
}

// T2 guard kept: a pattern that could be parsed as a go list flag is rejected.
func TestT21_PatternStartingWithDashRejected(t *testing.T) {
	for _, pat := range []string{"-json", "-toolexec=/bin/true", "--help"} {
		if _, err := importcheck.Forbidden(tkFixtureDir(t), pat, nil, []string{tkFixtureTestkit}); err == nil {
			t.Errorf("Forbidden(%q): expected error", pat)
		}
	}
}

// Failure behaviour: an unusable directory is an error, not a silent pass.
func TestT21_BadDirReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := importcheck.Forbidden(missing, "./...", nil, []string{tkFixtureTestkit}); err == nil {
		t.Fatal("expected error for a missing directory")
	}
}

// installRecordingGo puts a wrapper named "go" first on PATH. It records the
// GOENV and GOPACKAGESDRIVER values the subprocess sees, then execs the real
// go. Returns the record file path.
func installRecordingGo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper requires a POSIX shell")
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go not on PATH: %v", err)
	}
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "env.txt")
	script := "#!/bin/sh\n" +
		"{ printf 'GOENV=%s\\n' \"${GOENV-<unset>}\"\n" +
		"  printf 'GOPACKAGESDRIVER=%s\\n' \"${GOPACKAGESDRIVER-<unset>}\"; } >> \"$CORRAL_T21_RECORD\"\n" +
		"exec \"$CORRAL_T21_REAL_GO\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORRAL_T21_RECORD", record)
	t.Setenv("CORRAL_T21_REAL_GO", realGo)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func readRecord(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("go wrapper never ran (importcheck must invoke go from PATH): %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// T21(b) self-test: every go list subprocess (Check, Scan, Forbidden) runs
// with GOENV=off and GOPACKAGESDRIVER=off, even when the caller's environment
// sets them to something else.
func TestT21_SelfTest_GoListEnvIsHermetic(t *testing.T) {
	// Not parallel: uses t.Setenv.
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "hostile-env"))
	t.Setenv("GOPACKAGESDRIVER", filepath.Join(t.TempDir(), "hostile-driver"))
	record := installRecordingGo(t)

	if _, err := importcheck.Check(fixtureDir(t), "./internal/domain/...", fixturePrefix, nil); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := importcheck.Scan(fixtureDir(t), "./...", fixturePrefix); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, err := importcheck.Forbidden(tkFixtureDir(t), "./...",
		[]string{tkFixtureTestkit}, []string{tkFixtureTestkit}); err != nil {
		t.Fatalf("Forbidden: %v", err)
	}

	lines := readRecord(t, record)
	if len(lines) < 6 {
		t.Fatalf("expected 3 go invocations recorded, got lines: %q", lines)
	}
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "GOENV="):
			if l != "GOENV=off" {
				t.Errorf("line %d: %s, want GOENV=off", i, l)
			}
		case strings.HasPrefix(l, "GOPACKAGESDRIVER="):
			if l != "GOPACKAGESDRIVER=off" {
				t.Errorf("line %d: %s, want GOPACKAGESDRIVER=off", i, l)
			}
		}
	}
}

// T21(b) behaviour: a hostile user go env file (GOENV pointing at a file that
// sets an invalid GOFLAGS) must not break or alter the run, because GOENV=off
// makes go ignore it.
func TestT21_SelfTest_HostileGoEnvFileIgnored(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "goenv")
	if err := os.WriteFile(envFile, []byte("GOFLAGS=-definitely-not-a-flag\nGOPROXY=https://invalid.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", envFile)
	t.Setenv("GOFLAGS", "")

	got, err := importcheck.Scan(fixtureDir(t), "./...", fixturePrefix)
	if err != nil {
		t.Fatalf("Scan broken by hostile GOENV file: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Scan returned no packages")
	}
	vs, err := importcheck.Forbidden(tkFixtureDir(t), "./...",
		[]string{tkFixtureTestkit}, []string{tkFixtureTestkit, tkFixtureTools})
	if err != nil {
		t.Fatalf("Forbidden broken by hostile GOENV file: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("violations = %s, want 2", render(vs))
	}
}
