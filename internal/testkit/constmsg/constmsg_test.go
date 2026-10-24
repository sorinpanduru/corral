package constmsg_test

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
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

// wantLines returns the 1-based line numbers carrying a "// want" marker.
func wantLines(t *testing.T, path string) []int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []int
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "// want") {
			out = append(out, n)
		}
	}
	return out
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
// package-level functions and *slog.Logger methods taking a message.
func TestNonConstantMessagesAreReported(t *testing.T) {
	dir := fixture(t)
	fs, err := constmsg.Check(dir, "./bad")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := wantLines(t, filepath.Join(dir, "bad", "bad.go"))
	if len(want) < 20 {
		t.Fatalf("fixture has only %d want markers", len(want))
	}
	var got []int
	for _, f := range fs {
		if filepath.Base(f.File) != "bad.go" {
			t.Errorf("finding in unexpected file %q", f.File)
		}
		got = append(got, f.Line)
	}
	sort.Ints(got)
	if len(got) != len(want) {
		t.Fatalf("findings = %d (lines %v), want %d (lines %v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding lines = %v, want %v", got, want)
			break
		}
	}
}

// AC-5: the pattern list is honoured; both packages together report exactly
// the bad ones, and "./..." finds them as well.
func TestPatternsSelectPackages(t *testing.T) {
	dir := fixture(t)
	want := len(wantLines(t, filepath.Join(dir, "bad", "bad.go")))
	for _, patterns := range [][]string{{"./..."}, {"./good", "./bad"}} {
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
