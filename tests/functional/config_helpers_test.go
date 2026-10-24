//go:build functional

package functional

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// SPEC-003 T13: binary-level configuration tests. The default binary is used
// unless a test needs the corraltest build (ADR-0014: required or secret test
// settings, hold points). Loopback only, synthetic data only. Failure
// messages never print supplied values.

const (
	msgUnknownEnvFn = "unknown CORRAL_ environment variables ignored"
	msgLoadedFn     = "configuration loaded"
	msgReloadFn     = "configuration reload not supported; restart required"
	msgTLSFn        = "OTLP TLS settings ignored because the traces endpoint uses http://"
)

func fmark(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "fnmark-" + hex.EncodeToString(b)
}

// startTest starts the corraltest binary.
func startTest(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	return startProcBin(t, testBinPath, env, args...)
}

// scanClean asserts none of the markers appears in the process output.
func scanClean(t *testing.T, p *proc, markers ...string) {
	t.Helper()
	sc := markerscan.New()
	for _, m := range markers {
		sc.AddMarker(t, m)
	}
	sc.Add(markerscan.SourceStdout, p.stdout.Bytes())
	sc.Add(markerscan.SourceStderr, p.stderr.Bytes())
	sc.AssertClean(t)
}

func msgsOfRecs(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		out = append(out, str(r, "msg"))
	}
	return out
}

func settingsOfRecs(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		out = append(out, str(r, "setting"))
	}
	return out
}

func requireStderrEmpty(t *testing.T, p *proc) {
	t.Helper()
	if b := p.stderr.Bytes(); len(b) != 0 {
		t.Errorf("stderr has %d bytes, want empty", len(b))
	}
}

func requirePortFree(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Errorf("admin address is bound: %v", err)
		return
	}
	ln.Close()
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the hold point")
}

func release(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func holdEnv(dir, at string, extra ...string) []string {
	return baseEnv(append([]string{"CORRALTEST_HOLD_DIR=" + dir, "CORRALTEST_HOLD_AT=" + at}, extra...)...)
}

func (p *proc) hup() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		p.t.Fatalf("SIGHUP: %v", err)
	}
}

func (p *proc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func reloadCount(recs []map[string]any) int {
	n := 0
	for _, r := range recs {
		if str(r, "msg") == msgReloadFn {
			n++
		}
	}
	return n
}

func eventuallyFn(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

var _ = strings.TrimSpace
