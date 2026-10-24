package lint_test

import (
	"os/exec"
	"strings"
	"testing"
)

// SPEC-003 FR-22 (security review fix): the Makefile accepts only a VERSION
// made of [A-Za-z0-9._+-], checked on the whole value, so a multi-line or
// quoted value cannot inject shell commands. `make -n` only prints the recipe.
func TestMakefileRejectsUnsafeVersion(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	run := func(v string) (string, error) {
		cmd := exec.Command("make", "-n", "build", "VERSION="+v)
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for name, v := range map[string]string{
		"multi-line":  "1.0\n\"; echo INJECTED; echo \"",
		"space":       "1 x",
		"quote":       "x';rm",
		"empty":       "",
		"newline end": "1.0\n",
		"dollar":      "$$(id)",
	} {
		out, err := run(v)
		if err == nil {
			t.Errorf("%s: make accepted an unsafe VERSION", name)
		}
		if strings.Contains(out, "INJECTED") && !strings.Contains(out, "VERSION must match") {
			t.Errorf("%s: output suggests the value reached a shell command", name)
		}
	}
	for _, v := range []string{"devel", "1.2.3-rc.1+x", "v0.9.0"} {
		if out, err := run(v); err != nil {
			t.Errorf("VERSION %q rejected: %v\n%s", v, err, out)
		}
	}
}
