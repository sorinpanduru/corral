//go:build functional

package functional

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// SPEC-003 FR-22 (T12): --version prints exactly "corral <version>" and exits
// 0. The default build reports "devel"; the linker stamps the version through
// -X main.version (Makefile: VERSION ?= devel). No log record, nothing on stderr.

func runVersion(t *testing.T, bin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = baseEnv()
	var out, errb safeBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", bin, err)
	}
	return string(out.Bytes()), string(errb.Bytes()), code
}

func TestVersionDefaultsToDevel(t *testing.T) {
	for _, flag := range []string{"--version", "-version"} {
		out, errs, code := runVersion(t, binPath, flag)
		if code != 0 || out != "corral devel\n" || errs != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 0, %q, empty", flag, code, out, errs, "corral devel\n")
		}
	}
}

func TestVersionStampedByLinker(t *testing.T) {
	root := moduleRoot(t)
	const v = "9.9.9-functest"
	bin := filepath.Join(t.TempDir(), "corral-stamped")
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+v, "-o", bin, "./cmd/corral")
	build.Dir = root
	build.Env = hermeticEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, errs, code := runVersion(t, bin, "--version")
	if code != 0 || out != "corral "+v+"\n" || errs != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0, %q, empty", code, out, errs, "corral "+v+"\n")
	}
}

// FR-24: a configuration error exits 2, --help exits 0.
func TestExitCodesBinary(t *testing.T) {
	if _, _, code := runVersion(t, binPath, "--bogus"); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	if out, errs, code := runVersion(t, binPath, "--help"); code != 0 || errs != "" || out == "" {
		t.Errorf("--help: exit %d, %d bytes stdout, stderr %q", code, len(out), errs)
	}
}
