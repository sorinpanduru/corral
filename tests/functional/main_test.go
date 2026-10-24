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
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "go list -m:", err)
		return 2
	}
	build := exec.Command("go", "build", "-o", binPath, "./cmd/corral")
	build.Dir = string(trimNL(root))
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go build ./cmd/corral failed:", err)
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
