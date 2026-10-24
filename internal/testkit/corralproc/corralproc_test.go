package corralproc_test

// Self-test for corralproc. The API below is the interface fixed by this test
// (see the test-writer report). The process under test is a helper built from
// testdata/helper, a synthetic stand-in for the corral binary.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
	"github.com/sorinpanduru/corral/internal/testkit/corralproc"
)

var helperBin string

func TestMain(m *testing.M) {
	// Build once for all tests.
	dir, err := os.MkdirTemp("", "corralproc-helper")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	helperBin = filepath.Join(dir, "helper")
	if b, err := exec.Command("go", "build", "-o", helperBin, "./testdata/helper").CombinedOutput(); err != nil {
		panic("building helper: " + string(b))
	}
	os.Exit(m.Run())
}

// Args and environment reach the process; records and plain lines are split.
func TestStartCapturesOutput(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin, Args: []string{"--a=1", "--b=2"}, Env: []string{"HELPER_MARKER=m-1234", "HELPER_EXIT_CODE=5"}})
	code, err := p.Wait(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code != 5 {
		t.Fatalf("exit code = %d, want 5", code)
	}
	recs := p.Records()
	if len(recs) != 1 || recs[0].Msg() != "started" || recs[0].Component() != "lifecycle" {
		t.Fatalf("records = %v", recs)
	}
	if v, _ := recs[0]["args"].(string); v != "--a=1 --b=2" {
		t.Fatalf("args not passed: %q", v)
	}
	if v, _ := recs[0]["env_marker"].(string); v != "m-1234" {
		t.Fatalf("env not passed")
	}
	if !strings.Contains(string(p.Stdout()), "not json: plain stdout line") {
		t.Fatal("non-JSON stdout line lost from Stdout()")
	}
	if !strings.Contains(string(p.Stderr()), "stderr line") {
		t.Fatalf("stderr = %q", p.Stderr())
	}
}

// WaitRecord returns a record that arrives later, matching on msg and
// component, and times out with an error otherwise.
func TestWaitRecord(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin})
	r, err := p.WaitRecord("ready", "lifecycle", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg() != "ready" {
		t.Fatalf("got %v", r)
	}
	// Records that arrived before the call are still found.
	if _, err := p.WaitRecord("started", "lifecycle", time.Second); err != nil {
		t.Fatalf("earlier record not found: %v", err)
	}
	if _, err := p.WaitRecord("ready", "other_component", 200*time.Millisecond); err == nil {
		t.Fatal("component mismatch matched")
	}
	if _, err := p.WaitRecord("never", "", 200*time.Millisecond); err == nil {
		t.Fatal("missing record did not time out")
	}
}

// Signals: one signal, and repeated signals counted by the process.
func TestSignals(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin})
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait(10 * time.Second)
	if err != nil || code != 7 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestRepeatedSignals_FR63(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin, Env: []string{"HELPER_NEED_SIGNALS=3"}})
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.SignalRepeatedly(syscall.SIGTERM, 3, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait(10 * time.Second)
	if err != nil || code != 7 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	count := 0
	for _, r := range p.Records() {
		if r.Msg() == "signal" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("process saw %d signals, want 3", count)
	}
}

// Wait returns an error when the process outlives the timeout; t.Cleanup then
// kills it (no leaked process: the pid is gone after the test, checked by the
// next test run through the OS, here by Signal on an exited process).
func TestWaitTimeout(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin})
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(200 * time.Millisecond); err == nil {
		t.Fatal("Wait did not time out on a running process")
	}
	p.Signal(syscall.SIGTERM)
	if _, err := p.Wait(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// Admin endpoints: /readyz status and body, /metrics text.
func TestReadyzAndMetrics(t *testing.T) {
	addr := "127.0.0.1:" + itoa(corralargs.FreePort(t))
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin, Env: []string{"HELPER_ADMIN=" + addr}, AdminAddr: addr})
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var code int
	var body string
	var err error
	for i := 0; i < 100; i++ {
		code, body, err = p.Readyz()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || code != 200 || body != "ok\n" {
		t.Fatalf("readyz = %d %q %v", code, body, err)
	}
	os.WriteFile(filepath.Join(p.HoldDir(), "notready"), nil, 0o600)
	code, body, err = p.Readyz()
	if err != nil || code != 503 || body != "starting\n" {
		t.Fatalf("readyz after flip = %d %q %v", code, body, err)
	}
	m, err := p.Metrics()
	if err != nil || !strings.Contains(m, "helper_up 1") {
		t.Fatalf("metrics = %q %v", m, err)
	}
}

// Hold files: held-<point> appears, the process waits, release-<point> lets it
// go; several comma-separated points are released independently; the SPEC-003
// points use held / release.
func TestHoldPoints_ADR0022(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin, Env: []string{"CORRALTEST_HOLD_AT=laststep,shutdown_drain"}})
	if err := p.WaitHeld("laststep", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !p.Held("laststep") || p.Held("shutdown_drain") {
		t.Fatal("Held() wrong while at the first point")
	}
	if _, err := p.WaitRecord("ready", "lifecycle", 300*time.Millisecond); err == nil {
		t.Fatal("process continued while held")
	}
	if err := p.Release("laststep"); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitHeld("shutdown_drain", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.Release("shutdown_drain"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.HoldDir(), "release-laststep")); err != nil {
		t.Fatal("release file not created in the hold directory")
	}
}

func TestHoldPointsSpec003(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin, Env: []string{"CORRALTEST_HOLD_AT=logger"}})
	if err := p.WaitHeld("logger", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.HoldDir(), "held")); err != nil {
		t.Fatal("SPEC-003 point must use the file name 'held'")
	}
	if err := p.Release("logger"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.HoldDir(), "release")); err != nil {
		t.Fatal("SPEC-003 point must use the file name 'release'")
	}
	if _, err := p.WaitRecord("ready", "lifecycle", 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

// WaitHeld times out with an error when the point is never reached.
func TestWaitHeldTimeout(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin})
	if err := p.WaitHeld("laststep", 150*time.Millisecond); err == nil {
		t.Fatal("WaitHeld returned nil for an unreached point")
	}
}

// The test sets CORRALTEST_HOLD_DIR itself; the process sees a private,
// existing directory.
func TestHoldDirIsPrivate(t *testing.T) {
	p := corralproc.Start(t, corralproc.Opts{Binary: helperBin})
	st, err := os.Stat(p.HoldDir())
	if err != nil || !st.IsDir() {
		t.Fatalf("HoldDir %q: %v", p.HoldDir(), err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("hold dir permissions %v are not private", st.Mode().Perm())
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
