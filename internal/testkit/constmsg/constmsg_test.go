package constmsg_test

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/constmsg"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type loc struct {
	file string
	line int
}

// wantLocs returns, for every .go file in dir (build tags ignored), the lines
// ending in "// want" or "// want value".
func wantLocs(t *testing.T, dir string) (all []loc, value map[loc]bool) {
	t.Helper()
	value = map[loc]bool{}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture files in %s: %v", dir, err)
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			txt := strings.TrimSpace(sc.Text())
			l := loc{filepath.Base(path), n}
			switch {
			case strings.HasSuffix(txt, "// want value"):
				all = append(all, l)
				value[l] = true
			case strings.HasSuffix(txt, "// want"):
				all = append(all, l)
			}
		}
		f.Close()
	}
	return all, value
}

// AC-5 (static check): a package whose slog messages are all constants (and
// which has look-alike non-slog calls) yields no findings.
func TestConstantMessagesAreNotReported(t *testing.T) {
	fs, err := constmsg.Check(fixture(t), "./good")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("findings for constant messages: %+v", fs)
	}
}

// AC-5 (static check), FR-39: every slog call whose message is not a
// compile-time constant is reported, at the right file and line, across all
// package-level functions and *slog.Logger methods taking a message, method
// expressions, slog.NewRecord, multi-value spreads (fail closed), and files
// behind the functional build tag (Check must load with that tag). Method
// values and function variables are reported as "used as a value".
func TestNonConstantMessagesAreReported(t *testing.T) {
	dir := fixture(t)
	fs, err := constmsg.Check(dir, "./bad")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want, value := wantLocs(t, filepath.Join(dir, "bad"))
	if len(want) < 28 {
		t.Fatalf("fixture has only %d want markers", len(want))
	}
	got := map[loc]constmsg.Finding{}
	for _, f := range fs {
		l := loc{filepath.Base(f.File), f.Line}
		if _, dup := got[l]; dup {
			t.Errorf("duplicate finding at %v", l)
		}
		got[l] = f
	}
	for _, l := range want {
		f, ok := got[l]
		if !ok {
			t.Errorf("missing finding at %s:%d", l.file, l.line)
			continue
		}
		if value[l] && !strings.Contains(f.Message, "used as a value") {
			t.Errorf("%s:%d message %q lacks %q", l.file, l.line, f.Message, "used as a value")
		}
		delete(got, l)
	}
	for l := range got {
		t.Errorf("unexpected finding at %s:%d", l.file, l.line)
	}
}

// AC-5: the file behind //go:build functional is reported without the caller
// passing any tag.
func TestBuildTaggedFilesAreChecked(t *testing.T) {
	fs, err := constmsg.Check(fixture(t), "./bad")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if filepath.Base(f.File) == "tagged.go" {
			return
		}
	}
	t.Error("no finding from the //go:build functional file tagged.go")
}

// AC-5: the pattern list is honoured; both packages together report exactly
// the bad ones, and "./..." finds them as well.
func TestPatternsSelectPackages(t *testing.T) {
	dir := fixture(t)
	want := 0
	for _, d := range []string{"bad", "allow", "newlog"} {
		all, _ := wantLocs(t, filepath.Join(dir, d))
		want += len(all)
	}
	for _, patterns := range [][]string{{"./..."}, {"./good", "./bad", "./allow", "./newlog", "./internal/infra/telemetry/logbridge"}} {
		fs, err := constmsg.Check(dir, patterns...)
		if err != nil {
			t.Fatalf("Check(%v): %v", patterns, err)
		}
		if len(fs) != want {
			t.Errorf("Check(%v) = %d findings, want %d", patterns, len(fs), want)
		}
	}
}

// AC-5: a failing load (nonexistent directory) is an error, not a silent
// empty result that would let the check pass vacuously.
func TestLoadFailureIsError(t *testing.T) {
	fs, err := constmsg.Check(filepath.Join(t.TempDir(), "does-not-exist"), "./...")
	if err == nil {
		t.Fatalf("Check on a missing directory succeeded with %d findings", len(fs))
	}
}

// writeFakeTool creates an executable script in dir that records its
// execution in sentinel.
func writeFakeTool(t *testing.T, dir, name, sentinel string) {
	t.Helper()
	script := "#!/bin/sh\necho ran >> '" + sentinel + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Hermetic load: Check must set GOPACKAGESDRIVER=off and CGO_ENABLED=0 for the
// load, so neither an external packages driver found on PATH nor a C
// toolchain is ever executed, whatever the caller's environment says.
func TestLoadIsHermetic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes need a unix-like system")
	}
	bin := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "executed")
	writeFakeTool(t, bin, "gopackagesdriver", sentinel)
	for _, cc := range []string{"gcc", "cc", "clang"} {
		writeFakeTool(t, bin, cc, sentinel)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOPACKAGESDRIVER", "") // unset-equivalent: go/packages would search PATH
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("CC", filepath.Join(bin, "gcc"))

	if _, err := constmsg.Check(fixture(t), "./hermetic", "./good"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		b, _ := os.ReadFile(sentinel)
		t.Errorf("an external tool (gopackagesdriver or C compiler) was executed %d time(s)", strings.Count(string(b), "ran"))
	}
}

// AC-5 (static check), Orchestrator decision for T8: the annotation is honoured
// only in _test.go files (allow_test.go); a correctly annotated non-constant
// call in the non-test file annotated.go is still reported. A call carrying
// "//constmsg:allow <reason>" on its own line or on the line before it is not
// reported (a); an annotation with no reason, or a different keyword, is still
// reported (b); an annotation covers only its own line (c).
func TestAllowAnnotation(t *testing.T) {
	dir := fixture(t)
	fs, err := constmsg.Check(dir, "./allow")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want, _ := wantLocs(t, filepath.Join(dir, "allow"))
	if len(want) != 6 {
		t.Fatalf("fixture has %d want markers, want 6 (5 in allow_test.go, 1 in annotated.go)", len(want))
	}
	got := map[loc]bool{}
	for _, f := range fs {
		got[loc{filepath.Base(f.File), f.Line}] = true
	}
	for _, l := range want {
		if !got[l] {
			t.Errorf("missing finding at %s:%d (annotation must not cover it)", l.file, l.line)
		}
		delete(got, l)
	}
	for l := range got {
		t.Errorf("annotated line reported: %s:%d", l.file, l.line)
	}
}

// AC-5, FR-28 (T8 review): slog.NewLogLogger outside the logbridge package
// path is reported (even with an allow annotation, which is not honoured
// here); inside internal/infra/telemetry/logbridge it is not.
func TestNewLogLoggerOnlyAllowedInLogbridge(t *testing.T) {
	dir := fixture(t)
	fs, err := constmsg.Check(dir, "./newlog")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want, _ := wantLocs(t, filepath.Join(dir, "newlog"))
	if len(want) != 2 {
		t.Fatalf("fixture has %d want markers, want 2", len(want))
	}
	got := map[loc]bool{}
	for _, f := range fs {
		got[loc{filepath.Base(f.File), f.Line}] = true
	}
	for _, l := range want {
		if !got[l] {
			t.Errorf("missing finding at %s:%d", l.file, l.line)
		}
		delete(got, l)
	}
	for l := range got {
		t.Errorf("unexpected finding at %s:%d", l.file, l.line)
	}

	fs, err = constmsg.Check(dir, "./internal/infra/telemetry/logbridge")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("NewLogLogger reported inside the logbridge path: %+v", fs)
	}
}
