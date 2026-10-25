//go:build functional

// Package functional runs the corral binary end to end (SPEC-001 Part A, no
// Docker). TestMain builds the binary once with `go build` into a temp dir.
// Settings are passed as interim flags (ADR-0003). Loopback only, synthetic
// data only.
package functional

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var binPath string

// testBinPath is the `-tags corraltest` binary (ADR-0014): test registries and
// hold points. Only the tests that need them use it.
var testBinPath string

func TestMain(m *testing.M) {
	os.Exit(realMain(m))
}

func realMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "corral-functional-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		return 2
	}
	defer os.RemoveAll(dir)
	binPath = filepath.Join(dir, "corral")
	root, err := moduleRootDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "go list -m:", err)
		return 2
	}
	build := exec.Command("go", "build", "-o", binPath, "./cmd/corral")
	build.Dir = root
	build.Env = hermeticEnv()
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go build ./cmd/corral failed:", err)
		return 2
	}
	testBinPath = filepath.Join(dir, "corral-test")
	tbuild := exec.Command("go", "build", "-tags", "corraltest", "-o", testBinPath, "./cmd/corral")
	tbuild.Dir = root
	tbuild.Env = hermeticEnv()
	tbuild.Stdout, tbuild.Stderr = os.Stdout, os.Stderr
	if err := tbuild.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go build -tags corraltest ./cmd/corral failed:", err)
		return 2
	}
	return m.Run()
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// hermeticEnv is the environment of every go tool invocation in this package
// (build and go list alike), so the guard analyses what was built: the user's
// go env file and any package driver are switched off.
func hermeticEnv() []string {
	return append(os.Environ(), "GOENV=off", "GOPACKAGESDRIVER=off")
}

func moduleRootDir() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	cmd.Env = hermeticEnv()
	root, err := cmd.Output()
	return string(trimNL(root)), err
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	r, err := moduleRootDir()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return r
}
