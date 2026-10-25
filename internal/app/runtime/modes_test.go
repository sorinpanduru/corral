package runtime_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/config/usage"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// SPEC-003 T12 (FR-21, FR-22, FR-23, FR-24, FR-27, AC-12 unit part): mode
// dispatch through runtime.Run. Help and version start nothing, read nothing,
// write no log record and return 0. Interface assumptions: help and version
// are dispatched before Hooks.BeforeLogger; BeforeLogger runs before the
// configuration is loaded in every other mode.

// countingReader is a reader that records every call (it holds no files).
type countingReader struct {
	fs    *memfs.FS
	calls atomic.Int32
}

func newCountingReader() *countingReader { return &countingReader{fs: memfs.New()} }

func (c *countingReader) read(p string, max int64) ([]byte, error) {
	c.calls.Add(1)
	return c.fs.Read(p, max)
}

var _ source.ReadFile = (*countingReader)(nil).read

// runSync runs Run to completion (non-serving cases) and returns the exit
// code and output.
func runSync(t *testing.T, o runOpts) (int, *syncBuf) {
	t.Helper()
	h := run(t, o)
	return h.wait(), h.out
}

func usageText(t *testing.T) string {
	t.Helper()
	r := obsOnlyRegistry(t) // the registry the harness runs with (ADR-0021)
	var b bytes.Buffer
	if err := usage.Write(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func sendHUPs(ch chan os.Signal, n int) {
	for i := 0; i < n; i++ {
		select {
		case ch <- syscall.SIGHUP:
		default:
		}
	}
}

// AC-12 (unit): help prints the usage text, exits 0, writes no record, starts
// nothing (the admin port stays free) and reads nothing.
func TestHelpMode(t *testing.T) {
	want := usageText(t)
	for _, args := range [][]string{
		{"--help"}, {"-help"}, {"-h"}, {"--help", "--version"}, {"--version", "-h"}, {"--check-config", "--help"},
		{"--help", "--bogus=1", "stray", "--log.level=bad"},
	} {
		addr := reserveAddr(t)
		rd := newCountingReader()
		hup := make(chan os.Signal, 16)
		sendHUPs(hup, 5) // SIGHUP during help mode must not terminate or produce a record
		code, out := runSync(t, runOpts{
			args: append(append([]string{}, args...), "--admin.listen="+addr), readFile: rd.read, hup: hup,
			env: map[string]string{"CORRAL_LOG_LEVEL": "bad", "CORRAL_TYPO": "x", "OTEL_TRACES_SAMPLER": "bad"},
		})
		if code != 0 {
			t.Errorf("args %v: exit code %d, want 0", args, code)
		}
		if got := string(out.bytes()); got != want {
			t.Errorf("args %v: output is not the usage text (length %d, want %d)", args, len(got), len(want))
		}
		if rd.calls.Load() != 0 {
			t.Errorf("args %v: the file reader was called in help mode", args)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("args %v: admin address is bound after help mode: %v", args, err)
		} else {
			ln.Close()
		}
	}
}

// FR-21: help output carries no value from the environment.
func TestHelpModeNeverPrintsEnvironmentValues(t *testing.T) {
	m := marker(t, "helpmark")
	_, out := runSync(t, runOpts{args: []string{"--help"}, env: map[string]string{"CORRAL_LOG_LEVEL": m, "CORRAL_ADMIN_LISTEN": m}})
	if strings.Contains(string(out.bytes()), m) {
		t.Fatal("help output carries an environment value")
	}
}

// FR-22: version prints exactly "corral <version>\n".
func TestVersionMode(t *testing.T) {
	for _, v := range []string{"devel", "1.2.3", "v0.9.0-rc.1"} {
		for _, args := range [][]string{{"--version"}, {"-version"}, {"--version", "--check-config"}, {"--version", "--bogus", "stray", "--log.level=bad"}} {
			rd := newCountingReader()
			hup := make(chan os.Signal, 16)
			sendHUPs(hup, 3)
			code, out := runSync(t, runOpts{args: args, version: v, readFile: rd.read, hup: hup,
				env: map[string]string{"CORRAL_LOG_LEVEL": "bad"}})
			if code != 0 {
				t.Errorf("args %v: exit code %d, want 0", args, code)
			}
			if got, want := string(out.bytes()), "corral "+v+"\n"; got != want {
				t.Errorf("args %v: output %q, want %q", args, got, want)
			}
			if rd.calls.Load() != 0 {
				t.Errorf("args %v: the file reader was called in version mode", args)
			}
		}
	}
}

// Help beats version (FR-27 precedence): the usage text, not the version.
func TestHelpBeatsVersion(t *testing.T) {
	_, out := runSync(t, runOpts{args: []string{"--version", "--help"}, version: "1.2.3"})
	if strings.HasPrefix(string(out.bytes()), "corral 1.2.3") || string(out.bytes()) != usageText(t) {
		t.Fatal("version printed although a help spelling is present")
	}
}

// Help and version dispatch before BeforeLogger (nothing about them can be held).
func TestHelpAndVersionDoNotReachHooks(t *testing.T) {
	var calls atomic.Int32
	hook := func(context.Context) { calls.Add(1) }
	for _, args := range [][]string{{"--help"}, {"--version"}} {
		runSync(t, runOpts{args: args, beforeLogger: hook, beforeCheck: hook, before: hook})
	}
	if calls.Load() != 0 {
		t.Fatalf("hooks ran %d times in help/version mode", calls.Load())
	}
}

// ---- check-config (FR-23, AC-13 unit, AC-19 in-process) ----

// check-config success: exit 0, nothing bound, no lifecycle record, no
// Started hook; BeforeCheckConfigExit runs once after the FR-20 record.
func TestCheckConfigSuccess(t *testing.T) {
	addr := reserveAddr(t)
	var checks, started atomic.Int32
	var linesAtHook []string
	out := &syncBuf{}
	h := run(t, runOpts{
		out:  out,
		args: []string{"--check-config", "--admin.listen=" + addr, "--log.level=debug"},
		env:  map[string]string{"CORRAL_TYPO": "x"},
		beforeCheck: func(context.Context) {
			checks.Add(1)
			linesAtHook = out.lines()
		},
		before: func(context.Context) { started.Add(1) },
	})
	if code := h.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if checks.Load() != 1 {
		t.Fatalf("BeforeCheckConfigExit ran %d times, want 1", checks.Load())
	}
	if started.Load() != 0 || h.startedCalls() != 0 {
		t.Error("the server started in check-config mode")
	}
	recs := out.records(t)
	var msgs []string
	for _, r := range recs {
		msgs = append(msgs, str(r, "msg"))
	}
	// FR-23: the FR-8 warning (unknown variable) and the FR-20 record; nothing else.
	want := []string{"unknown CORRAL_ environment variables ignored", "configuration loaded"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("records = %q, want %q", msgs, want)
	}
	if len(linesAtHook) != 2 {
		t.Errorf("at the hold point the output had %d records, want both already written", len(linesAtHook))
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("admin address bound in check-config mode: %v", err)
	}
	ln.Close()
}

// AC-13 (unit): check-config binds nothing even when the address is taken.
func TestCheckConfigBindsNothing(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	code, out := runSync(t, runOpts{args: []string{"--check-config", "--admin.listen=" + busy.Addr().String()}})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (nothing is bound, so no bind failure)", code)
	}
	if hasLifecycle(out.records(t), "starting") {
		t.Error("lifecycle starting record in check-config mode")
	}
}

// AC-13 (unit): the FR-41 warning appears in check-config mode when it applies.
func TestCheckConfigReportsTLSIgnored(t *testing.T) {
	cert, _ := t23Certs(t)
	code, out := runSync(t, runOpts{
		args: []string{"--check-config", "--tracing.enabled=true"},
		env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.invalid:4318", "OTEL_EXPORTER_OTLP_CERTIFICATE": cert},
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	var msgs []string
	for _, r := range out.records(t) {
		msgs = append(msgs, str(r, "msg"))
	}
	want := []string{t23Msg, "configuration loaded"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("records = %q, want %q", msgs, want)
	}
}

// check-config failure: exit 2, failure path records, no FR-20.
func TestCheckConfigFailure(t *testing.T) {
	var checks atomic.Int32
	code, out := runSync(t, runOpts{
		args:        []string{"--check-config", "--log.level=bad"},
		beforeCheck: func(context.Context) { checks.Add(1) },
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	recs := out.records(t)
	if len(recs) != 1 || str(recs[0], "setting") != "log.level" || str(recs[0], "level") != "ERROR" {
		t.Fatalf("records = %v", recs)
	}
	if checks.Load() != 0 {
		t.Error("BeforeCheckConfigExit ran although the configuration failed")
	}
}

// AC-19 (in-process): SIGHUPs sent while check-config is held after the FR-20
// record neither terminate the run nor produce a record; the output is
// exactly the FR-23 success records and the exit code is 0.
func TestAC19_CheckConfigSIGHUPNoRecord(t *testing.T) {
	hup := make(chan os.Signal, 64)
	out := &syncBuf{}
	h := run(t, runOpts{
		out: out, hup: hup,
		args: []string{"--check-config", "--admin.listen=" + reserveAddr(t)},
		env:  map[string]string{"CORRAL_TYPO": "x"},
		beforeLogger: func(context.Context) {
			sendHUPs(hup, 4) // before the configured logger exists
		},
		beforeCheck: func(context.Context) {
			sendHUPs(hup, 20) // after the configured logger is built
			time.Sleep(200 * time.Millisecond)
		},
	})
	if code := h.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var msgs []string
	for _, r := range out.records(t) {
		msgs = append(msgs, str(r, "msg"))
	}
	want := []string{"unknown CORRAL_ environment variables ignored", "configuration loaded"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("records = %q, want exactly the FR-23 records %q", msgs, want)
	}
	if strings.Contains(string(out.bytes()), "reload") {
		t.Error("a reload record was written in check-config mode")
	}
}

// SIGHUP never produces a record before the configured logger exists, in any
// mode that reaches BeforeLogger (failure path included).
func TestSIGHUPBeforeLoggerNoRecordOnFailure(t *testing.T) {
	hup := make(chan os.Signal, 64)
	code, out := runSync(t, runOpts{
		hup: hup, args: []string{"--log.level=bad"},
		beforeLogger: func(context.Context) { sendHUPs(hup, 10); time.Sleep(100 * time.Millisecond) },
	})
	if code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	recs := out.records(t)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want only the configuration error", len(recs))
	}
}

// Programming errors are not configuration errors: a registry without the
// tracing settings, or a nil file reader, is exit 1 (FR-24), never the FR-11
// records and never exit 2.
func TestProgrammingErrorsExitOne(t *testing.T) {
	only, err := registry.New(registry.Group{Spec: "T", Namespaces: []string{"t"}, Decls: []registry.Decl{
		{Name: "t.x", Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceOptional, Description: "synthetic"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	code, out := runSync(t, runOpts{registry: only})
	if code != 1 {
		t.Errorf("registry without tracing settings: exit %d, want 1", code)
	}
	for _, r := range jsonRecordsNoFail(out.lines()) {
		if str(r, "msg") == "invalid configuration" {
			t.Error("a programming error was reported as a configuration error")
		}
	}
	var buf syncBuf
	code = corralrt.Run(context.Background(), corralrt.Inputs{Args: []string{"--tracing.enabled=true"}, Stdout: &buf, Version: "test", Registry: obsOnlyRegistry(t)}, corralrt.Hooks{})
	if code != 1 {
		t.Errorf("nil file reader: exit %d, want 1", code)
	}
	for _, r := range jsonRecordsNoFail(buf.lines()) {
		if str(r, "msg") == "invalid configuration" {
			t.Error("a nil reader was reported as a configuration error")
		}
	}
}

// obsOnlyRegistry is the observability group alone (ADR-0021).
func obsOnlyRegistry(tb testing.TB) *registry.Registry {
	tb.Helper()
	r, err := settings.RegistryOf(settings.ObservabilityGroup())
	if err != nil {
		tb.Fatal(err)
	}
	return r
}
