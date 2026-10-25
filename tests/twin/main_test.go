//go:build twin

// Package twin runs the corral binary against simulated upstreams
// (twins/mongosim, twins/faultproxy) with raw-socket and driver clients
// (SPEC-002 T23 to T25, ADR-0022). TestMain builds the production binary and
// the corraltest binary once. Loopback only, synthetic data only.
package twin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var prodBin, testBin string

func TestMain(m *testing.M) { os.Exit(realMain(m)) }

func realMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "corral-twin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		return 2
	}
	defer os.RemoveAll(dir)
	root, err := moduleRootDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "go list -m:", err)
		return 2
	}
	prodBin = filepath.Join(dir, "corral")
	testBin = filepath.Join(dir, "corral-test")
	for _, b := range []struct {
		out  string
		tags []string
	}{{prodBin, nil}, {testBin, []string{"-tags", "corraltest"}}} {
		args := append([]string{"build"}, b.tags...)
		args = append(args, "-o", b.out, "./cmd/corral")
		c := exec.Command("go", args...)
		c.Dir = root
		c.Env = append(os.Environ(), "GOENV=off", "GOPACKAGESDRIVER=off", "GOFLAGS=")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "go build failed:", err)
			return 2
		}
	}
	return m.Run()
}

func moduleRootDir() (string, error) {
	c := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	c.Env = append(os.Environ(), "GOENV=off", "GOPACKAGESDRIVER=off")
	out, err := c.Output()
	for len(out) > 0 && (out[len(out)-1] == '\n' || out[len(out)-1] == '\r') {
		out = out[:len(out)-1]
	}
	return string(out), err
}
