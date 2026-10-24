package lint_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/app/settings"
)

// ADR-0014 guard: the production build contains no corraltest code and no
// test settings.

func goEnv() []string {
	return append(os.Environ(), "GOFLAGS=", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off")
}

// The test group's namespace `t` is never part of the production registry.
func TestTestBuildNamespaceNotInProductionRegistry(t *testing.T) {
	for _, g := range settings.Groups() {
		for _, ns := range g.Namespaces {
			if ns == "t" {
				t.Errorf("group %q claims the test namespace t", g.Spec)
			}
		}
		for _, d := range g.Decls {
			if strings.HasPrefix(d.Name, "t.") {
				t.Errorf("production setting %s is in the test namespace", d.Name)
			}
		}
	}
	r, err := settings.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Settings() {
		if strings.HasPrefix(s.Name, "t.") {
			t.Errorf("registry holds test setting %s", s.Name)
		}
	}
}

func firstConstraint(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "//go:build") {
			return strings.TrimSpace(strings.TrimPrefix(l, "//go:build"))
		}
		if strings.HasPrefix(l, "package ") {
			break
		}
	}
	return ""
}

func TestTestBuildFilesCarryExclusiveConstraints(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "cmd", "corral")
	if c := firstConstraint(t, filepath.Join(dir, "testbuild_on.go")); c != "corraltest" {
		t.Errorf("testbuild_on.go constraint = %q, want corraltest", c)
	}
	if c := firstConstraint(t, filepath.Join(dir, "testbuild_off.go")); c != "!corraltest" {
		t.Errorf("testbuild_off.go constraint = %q, want !corraltest", c)
	}
}

func goListFiles(t *testing.T, tags string) string {
	t.Helper()
	args := []string{"list", "-f", "{{.GoFiles}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/corral")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	cmd.Env = goEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	return string(out)
}

// The default build compiles testbuild_off.go only; the tagged build the other.
func TestDefaultBuildExcludesTestBuildFile(t *testing.T) {
	def := goListFiles(t, "")
	if strings.Contains(def, "testbuild_on.go") || !strings.Contains(def, "testbuild_off.go") {
		t.Errorf("default build files = %s", def)
	}
	tagged := goListFiles(t, "corraltest")
	if !strings.Contains(tagged, "testbuild_on.go") || strings.Contains(tagged, "testbuild_off.go") {
		t.Errorf("corraltest build files = %s", tagged)
	}
}

// The production binary carries none of the CORRALTEST_* behaviour; the test
// binary does (so the check can see it).
func TestProductionBinaryHasNoCorraltestCode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	root := repoRoot(t)
	build := func(tags string) []byte {
		out := filepath.Join(t.TempDir(), "corral")
		args := []string{"build", "-o", out}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, "./cmd/corral")
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = goEnv()
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", tags, err, o)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	prod := build("")
	for _, needle := range []string{"CORRALTEST_HOLD_DIR", "CORRALTEST_REGISTRY", "CORRALTEST_HOLD_AT"} {
		if bytes.Contains(prod, []byte(needle)) {
			t.Errorf("the production binary contains %s", needle)
		}
	}
	if !bytes.Contains(build("corraltest"), []byte("CORRALTEST_HOLD_DIR")) {
		t.Error("the corraltest binary lacks CORRALTEST_HOLD_DIR: the guard cannot see test code")
	}
}
