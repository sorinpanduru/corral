package symcheck_test

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/symcheck"
)

// SPEC-003 AC-15 / FR-25 (T14): symcheck reports references to forbidden
// package-level symbols, found by resolved object so aliases, dot-imports and
// method values are caught and look-alikes are not.
//
// Interface assumptions (minimal shape, see the test-writer report):
//   - a symbol is "<package import path>.<Name>", for example "os.Getenv";
//   - fileFilter returns true for a file that is in scope; a nil filter means
//     every file is in scope;
//   - test files (*_test.go) are not analysed;
//   - Finding.File is the source file path (tests compare the base name);
//   - findings are sorted by Package, File, Line, Symbol.
//
// The fixture is a separate module under testdata/. Each line that must be
// reported carries a "// want: <symbol>" comment; "// want-unfiltered:" marks
// a line reported only when no file filter excludes its file. The expected
// findings are derived from those comments, so the fixture and the test
// cannot drift apart. Synthetic code only.

var allSymbols = []string{
	"os.Args", "os.Getenv", "os.LookupEnv", "os.Environ",
	"syscall.Getenv", "syscall.Environ",
	"os.ReadFile", "os.Open", "os.OpenFile", "os.Stat", "os.Lstat",
}

const fxModule = "fixture.example/fx"

func fixtureDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// want is one expected finding.
type want struct {
	pkg, file string // file is a base name
	line      int
	symbol    string
}

func (w want) String() string { return fmt.Sprintf("%s %s:%d %s", w.pkg, w.file, w.line, w.symbol) }

var wantRE = regexp.MustCompile(`// want(-unfiltered)?: (\S+)\s*$`)

// expected scans the fixture for want comments in the packages under the given
// relative dirs (nil means all), keeping only the symbols in syms and, when
// excludeFile is non-empty, dropping want-unfiltered lines in that file.
func expected(t *testing.T, dirs []string, syms []string, filtered func(base string) bool) []want {
	t.Helper()
	root := fixtureDir(t)
	symSet := map[string]bool{}
	for _, s := range syms {
		symSet[s] = true
	}
	var out []want
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		rel = filepath.ToSlash(rel)
		if dirs != nil {
			in := false
			for _, d := range dirs {
				if rel == d {
					in = true
				}
			}
			if !in {
				return nil
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			m := wantRE.FindStringSubmatch(sc.Text())
			if m == nil || !symSet[m[2]] {
				continue
			}
			if m[1] != "" && filtered != nil && filtered(filepath.Base(p)) {
				continue
			}
			out = append(out, want{pkg: fxModule + "/" + rel, file: filepath.Base(p), line: n, symbol: m[2]})
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	sortWants(out)
	return out
}

func sortWants(ws []want) {
	sort.Slice(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.pkg != b.pkg {
			return a.pkg < b.pkg
		}
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.symbol < b.symbol
	})
}

func toWants(fs []symcheck.Finding) []want {
	var out []want
	for _, f := range fs {
		out = append(out, want{pkg: f.Package, file: filepath.Base(f.File), line: f.Line, symbol: f.Symbol})
	}
	return out
}

func requireFindings(t *testing.T, got []symcheck.Finding, wantWs []want) {
	t.Helper()
	g := toWants(got)
	sortWants(g)
	gs, ws := make([]string, len(g)), make([]string, len(wantWs))
	for i := range g {
		gs[i] = g[i].String()
	}
	for i := range wantWs {
		ws[i] = wantWs[i].String()
	}
	if strings.Join(gs, "\n") != strings.Join(ws, "\n") {
		t.Fatalf("findings differ.\n got:\n  %s\nwant:\n  %s", strings.Join(gs, "\n  "), strings.Join(ws, "\n  "))
	}
}

// Non-vacuity guard for the fixture itself: it really contains the cases the
// tests below rely on.
func TestFixtureHasExpectedCases(t *testing.T) {
	all := expected(t, nil, allSymbols, nil)
	seen := map[string]int{}
	for _, w := range all {
		seen[w.symbol]++
	}
	for _, s := range allSymbols {
		if seen[s] == 0 {
			t.Errorf("fixture has no case for %s", s)
		}
	}
	if len(expected(t, []string{"alias"}, allSymbols, nil)) < 5 {
		t.Error("fixture alias package lost cases")
	}
}

// AC-15: every forbidden symbol is detected, once, at its line.
func TestForbidden_EachSymbolReportedOnce(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad"}, nil, allSymbols)
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	if len(got) != len(allSymbols) {
		t.Errorf("got %d findings, want %d (one per symbol)", len(got), len(allSymbols))
	}
	requireFindings(t, got, expected(t, []string{"bad"}, allSymbols, nil))
}

// Detection is by resolved object: an aliased import, a dot-import, a method
// value (`f := os.Getenv`) and a function value passed as an argument.
func TestForbidden_AliasDotImportAndMethodValue(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./alias"}, nil, allSymbols)
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	requireFindings(t, got, expected(t, []string{"alias"}, allSymbols, nil))
}

// Detection is by resolved object, not by spelling: a clean package, look-alike
// methods and functions, strings, comments and a package whose import is
// shadowed by a different package named "os" produce no findings.
func TestForbidden_CleanAndLookAlikesNotReported(t *testing.T) {
	for _, pat := range []string{"./clean", "./shadow", "./fakeos"} {
		got, err := symcheck.Forbidden(fixtureDir(t), []string{pat}, nil, allSymbols)
		if err != nil {
			t.Fatalf("%s: %v", pat, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: %d findings, want 0 (first: %+v)", pat, len(got), got[0])
		}
	}
}

// Whole-tree run: the union of all cases and nothing else.
func TestForbidden_WholeFixtureTree(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./..."}, nil, allSymbols)
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	requireFindings(t, got, expected(t, nil, allSymbols, nil))
	for _, f := range got {
		for _, clean := range []string{"/clean", "/shadow", "/fakeos", "/withtest"} {
			if strings.HasSuffix(f.Package, clean) {
				t.Errorf("finding in %s", f.Package)
			}
		}
	}
}

// Test files are out of scope (the fixture's x_test.go uses os.Getenv).
func TestForbidden_TestFilesNotAnalysed(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./withtest"}, nil, allSymbols)
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("test file reported: %+v", got[0])
	}
}

// Negative control (plan): a file outside the filter is not reported, and the
// same file is reported without the filter.
func TestForbidden_FileFilter(t *testing.T) {
	skip := func(base string) bool { return base == "skipme.go" }
	var seen []string
	filter := func(path string) bool {
		seen = append(seen, filepath.Base(path))
		return filepath.Base(path) != "skipme.go"
	}
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./filtered"}, filter, allSymbols)
	if err != nil {
		t.Fatalf("Forbidden: %v", err)
	}
	requireFindings(t, got, expected(t, []string{"filtered"}, allSymbols, skip))
	if len(got) != 1 || filepath.Base(got[0].File) != "keep.go" {
		t.Fatalf("want exactly the keep.go finding, got %d findings", len(got))
	}
	sort.Strings(seen)
	if strings.Join(seen, ",") == "" || !contains(seen, "keep.go") || !contains(seen, "skipme.go") {
		t.Errorf("filter was called for %v, want both files of the package", seen)
	}
	for _, p := range seen {
		if p == "" {
			t.Error("filter called with an empty path")
		}
	}

	all, err := symcheck.Forbidden(fixtureDir(t), []string{"./filtered"}, nil, allSymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("nil filter: got %d findings, want 2", len(all))
	}
	always := func(string) bool { return true }
	all, err = symcheck.Forbidden(fixtureDir(t), []string{"./filtered"}, always, allSymbols)
	if err != nil || len(all) != 2 {
		t.Fatalf("accept-all filter: %d findings, err %v, want 2", len(all), err)
	}
	none := func(string) bool { return false }
	zero, err := symcheck.Forbidden(fixtureDir(t), []string{"./filtered"}, none, allSymbols)
	if err != nil || len(zero) != 0 {
		t.Fatalf("reject-all filter: %d findings, err %v, want 0", len(zero), err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Only the requested symbols are reported.
func TestForbidden_SymbolSubset(t *testing.T) {
	for _, syms := range [][]string{{"os.Getenv"}, {"os.Stat", "syscall.Environ"}, {"os.Args"}} {
		got, err := symcheck.Forbidden(fixtureDir(t), []string{"./..."}, nil, syms)
		if err != nil {
			t.Fatalf("%v: %v", syms, err)
		}
		if len(got) == 0 {
			t.Fatalf("%v: no findings", syms)
		}
		requireFindings(t, got, expected(t, nil, syms, nil))
	}
}

func TestForbidden_MultiplePatterns(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad", "./filtered"}, nil, allSymbols)
	if err != nil {
		t.Fatal(err)
	}
	requireFindings(t, got, expected(t, []string{"bad", "filtered"}, allSymbols, nil))
}

func TestForbidden_FindingFields(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./filtered"}, func(p string) bool {
		return filepath.Base(p) == "keep.go"
	}, []string{"os.Getenv"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d findings, err %v", len(got), err)
	}
	f := got[0]
	if f.Package != fxModule+"/filtered" {
		t.Errorf("Package = %q, want the import path %q", f.Package, fxModule+"/filtered")
	}
	if filepath.Base(f.File) != "keep.go" {
		t.Errorf("File = %q, want keep.go", f.File)
	}
	if f.Line != 5 {
		t.Errorf("Line = %d, want 5", f.Line)
	}
	if f.Symbol != "os.Getenv" {
		t.Errorf("Symbol = %q, want os.Getenv", f.Symbol)
	}
}

// Deterministic order: by Package, File, Line, Symbol.
func TestForbidden_SortedAndDeterministic(t *testing.T) {
	first, err := symcheck.Forbidden(fixtureDir(t), []string{"./..."}, nil, allSymbols)
	if err != nil {
		t.Fatal(err)
	}
	ws := toWants(first)
	if !sort.SliceIsSorted(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.pkg != b.pkg {
			return a.pkg < b.pkg
		}
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.symbol < b.symbol
	}) {
		t.Error("findings are not sorted by Package, File, Line, Symbol")
	}
	// Pattern order does not change the result.
	a, _ := symcheck.Forbidden(fixtureDir(t), []string{"./bad", "./alias"}, nil, allSymbols)
	b, _ := symcheck.Forbidden(fixtureDir(t), []string{"./alias", "./bad"}, nil, allSymbols)
	if fmt.Sprint(a) != fmt.Sprint(b) || len(a) == 0 {
		t.Error("result depends on pattern order")
	}
}

// ---- failure behaviour: never pass vacuously ----

func TestForbidden_RejectsPatternsStartingWithDash(t *testing.T) {
	for _, p := range []string{"-x", "--help", "-C=/", "-overlay=/dev/null", "-"} {
		if _, err := symcheck.Forbidden(fixtureDir(t), []string{p}, nil, allSymbols); err == nil {
			t.Errorf("pattern %q accepted", p)
		}
		// Also when not first.
		if _, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad", p}, nil, allSymbols); err == nil {
			t.Errorf("pattern %q accepted as a later pattern", p)
		}
	}
}

func TestForbidden_EmptyMatchIsAnError(t *testing.T) {
	for _, p := range []string{"./doesnotexist/...", "./doesnotexist", "./testdata/..."} {
		if _, err := symcheck.Forbidden(fixtureDir(t), []string{p}, nil, allSymbols); err == nil {
			t.Errorf("pattern %q matched nothing yet returned no error", p)
		}
	}
}

func TestForbidden_EmptyInputsAreErrors(t *testing.T) {
	if _, err := symcheck.Forbidden(fixtureDir(t), nil, nil, allSymbols); err == nil {
		t.Error("no patterns accepted")
	}
	if _, err := symcheck.Forbidden(fixtureDir(t), []string{}, nil, allSymbols); err == nil {
		t.Error("empty patterns accepted")
	}
	if _, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad"}, nil, nil); err == nil {
		t.Error("nil symbols accepted")
	}
	if _, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad"}, nil, []string{}); err == nil {
		t.Error("empty symbols accepted")
	}
}

func TestForbidden_MissingDirIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := symcheck.Forbidden(missing, []string{"./..."}, nil, allSymbols); err == nil {
		t.Error("missing dir accepted")
	}
}

// A tree that does not type-check cannot be proven clean: error, not "no
// findings".
func TestForbidden_BrokenPackageIsAnError(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "brokenfixture"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := symcheck.Forbidden(dir, []string{"./..."}, nil, allSymbols)
	if err == nil {
		t.Fatalf("package with a type error accepted, %d findings", len(got))
	}
}

// Hermetic (like importcheck): the caller's go environment must not change
// what is analysed or make loading fail.
func TestForbidden_IsHermetic(t *testing.T) {
	t.Setenv("GOFLAGS", "-bogusflag")
	t.Setenv("GOPACKAGESDRIVER", filepath.Join(t.TempDir(), "no-such-driver"))
	t.Setenv("GOWORK", filepath.Join(t.TempDir(), "no-such.work"))
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "no-such-env"))
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad"}, nil, allSymbols)
	if err != nil {
		t.Fatalf("hostile go environment broke Forbidden: %v", err)
	}
	requireFindings(t, got, expected(t, []string{"bad"}, allSymbols, nil))
}

// Errors do not make the test harness print findings as errors: an error is
// nil exactly when the analysis completed.
func TestForbidden_NoErrorWithFindings(t *testing.T) {
	got, err := symcheck.Forbidden(fixtureDir(t), []string{"./bad"}, nil, allSymbols)
	if err != nil || len(got) == 0 {
		t.Fatalf("findings must be returned without an error (err %v, %d findings)", err, len(got))
	}
}
