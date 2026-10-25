package lint_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/symcheck"
)

// SPEC-003 AC-15 (FR-25, FR-19): static check. The loader packages, the
// configload package and the tracing environment-validation code reference no
// process global (arguments, environment, files). Detection is by resolved
// object (symcheck), so aliases, dot-imports and method values are caught.

var forbiddenGlobals = []string{
	"os.Args", "os.Getenv", "os.LookupEnv", "os.Environ", "syscall.Getenv", "syscall.Environ",
	"os.ReadFile", "os.Open", "os.OpenFile", "os.Stat", "os.Lstat",
}

func render2(fs []symcheck.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Package + " " + filepath.Base(f.File) + ":" + itoa(f.Line) + " " + f.Symbol + "\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func TestAC15_ConfigPackagesUseNoProcessGlobals(t *testing.T) {
	root := repoRoot(t)
	fs, err := symcheck.Forbidden(root, []string{"./internal/infra/config/...", "./internal/app/configload"}, nil, forbiddenGlobals)
	if err != nil {
		t.Fatalf("symcheck: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("process globals referenced:\n%s", render2(fs))
	}
}

// Only the environment-validation files of the tracing package are in scope
// (the exporter keeps SDK semantics, FR-19).
func TestAC15_TracingEnvValidationUsesNoProcessGlobals(t *testing.T) {
	root := repoRoot(t)
	var seen []string
	filter := func(path string) bool {
		b := filepath.Base(path)
		in := strings.HasPrefix(b, "env") && strings.HasSuffix(b, ".go")
		if in {
			seen = append(seen, b)
		}
		return in
	}
	fs, err := symcheck.Forbidden(root, []string{"./internal/infra/telemetry/tracing"}, filter, forbiddenGlobals)
	if err != nil {
		t.Fatalf("symcheck: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no env*.go file in the tracing package was in scope (vacuous check)")
	}
	if len(fs) != 0 {
		t.Fatalf("process globals referenced in the tracing env files:\n%s", render2(fs))
	}
}

// Negative control: the real checker does find a global where one is used
// (osfile is the one place allowed to use them), so the checks above are not
// vacuous; and a file filter that excludes everything finds nothing.
func TestAC15_NegativeControls(t *testing.T) {
	root := repoRoot(t)
	fs, err := symcheck.Forbidden(root, []string{"./internal/infra/osfile"}, nil, forbiddenGlobals)
	if err != nil {
		t.Fatalf("symcheck: %v", err)
	}
	if len(fs) == 0 {
		t.Fatal("symcheck found no process global in osfile: the check cannot see real uses")
	}
	none, err := symcheck.Forbidden(root, []string{"./internal/infra/osfile"}, func(string) bool { return false }, forbiddenGlobals)
	if err != nil || len(none) != 0 {
		t.Fatalf("a filter excluding every file still produced %d findings (err %v)", len(none), err)
	}
}
