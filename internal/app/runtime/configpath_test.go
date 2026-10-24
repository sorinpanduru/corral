package runtime_test

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// SPEC-003 T12 (FR-8, FR-11, FR-13, FR-19, FR-20, FR-24, FR-29, ADR-0013):
// the failure path and the success path of runtime.Run.
//
// Compliance (FR-11, FR-29): supplied values are markers; nothing supplied is
// echoed.

const (
	msgUnknownEnv = "unknown CORRAL_ environment variables ignored"
	msgLoaded     = "configuration loaded"
	msgReload     = "configuration reload not supported; restart required"
)

func msgsOf(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		out = append(out, str(r, "msg"))
	}
	return out
}

func indexOfMsg(recs []map[string]any, msg string) int {
	for i, r := range recs {
		if str(r, "msg") == msg {
			return i
		}
	}
	return -1
}

// ---- failure path ----

// AC-5 (unit): an unknown variable plus an invalid setting with
// --log.format=text: the FR-8 warning first, then the error, both JSON.
func TestFailurePath_WarnThenErrorBothJSON(t *testing.T) {
	m := marker(t, "failmark")
	sc := markerscan.New()
	sc.AddMarker(t, m)
	code, out := runSync(t, runOpts{
		args: []string{"--log.format=text", "--log.level=" + m},
		env:  map[string]string{"CORRAL_SERVICE_HOST": m},
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	lines := out.lines()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	recs := out.records(t) // fails the test on any non-JSON line
	w, e := recs[0], recs[1]
	if str(w, "msg") != msgUnknownEnv || str(w, "level") != "WARN" || str(w, "component") != "config" || str(w, "setting") != "CORRAL_SERVICE_HOST" {
		t.Errorf("first record is not the FR-8 warning: %v", w)
	}
	if str(e, "msg") != "invalid configuration" || str(e, "level") != "ERROR" || str(e, "component") != "config" || str(e, "setting") != "log.level" {
		t.Errorf("second record is not the error: %v", e)
	}
	sc.Add(markerscan.SourceStdout, out.bytes())
	sc.AssertClean(t)
}

// FR-29: the bootstrap logger filters nothing, whatever log.level says.
func TestFailurePath_NoLevelFiltering(t *testing.T) {
	code, out := runSync(t, runOpts{
		args: []string{"--log.level=error", "--admin.listen=:9187"},
		env:  map[string]string{"CORRAL_TYPO": "x"},
	})
	if code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	recs := out.records(t)
	if got := msgsOf(recs); len(got) != 2 || got[0] != msgUnknownEnv {
		t.Fatalf("records = %q, want the FR-8 warning (not filtered by log.level=error) then the error", got)
	}
}

// FR-13 / FR-29: the four groups in order, all through the bootstrap JSON logger.
func TestFailurePath_RecordOrderAcrossGroups(t *testing.T) {
	m := marker(t, "ordmark")
	code, out := runSync(t, runOpts{
		args: []string{"--log.format=text", "--bogus=" + m, "--log.level=" + m, "--admin.listen=" + m, "--tracing.enabled=true"},
		env: map[string]string{
			"CORRAL_TRACING_SAMPLE_RATIO": m, "CORRAL_ZZ": m,
			"OTEL_TRACES_SAMPLER": "bad" + m, "OTEL_BSP_SCHEDULE_DELAY": "x" + m,
		},
	})
	if code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	recs := out.records(t)
	var got []string
	for _, r := range recs {
		got = append(got, str(r, "setting"))
	}
	want := []string{
		"CORRAL_ZZ",                                                // FR-8 warning
		"unknown",                                                  // group 1
		"CORRAL_TRACING_SAMPLE_RATIO", "admin.listen", "log.level", // group 2
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_TRACES_SAMPLER", // group 4
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("settings = %v, want %v", got, want)
	}
	for _, r := range recs[1:] {
		if str(r, "level") != "ERROR" || str(r, "msg") != "invalid configuration" {
			t.Errorf("record %v is not an FR-11 error record", r)
		}
	}
	if strings.Contains(string(out.bytes()), m) {
		t.Error("output echoes a supplied value")
	}
}

// ADR-0013: no configured logger, no FR-41, no FR-20, no lifecycle record, no
// later step on the failure path.
func TestFailurePath_NothingElse(t *testing.T) {
	cert, _ := t23Certs(t)
	var after atomic.Int32
	h := run(t, runOpts{
		args:   []string{"--tracing.enabled=true", "--log.level=bad", "--admin.listen=" + reserveAddr(t)},
		env:    map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.invalid:4318", "OTEL_EXPORTER_OTLP_CERTIFICATE": cert},
		before: func(context.Context) { after.Add(1) },
	})
	if code := h.wait(); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	for _, msg := range msgsOf(h.out.records(t)) {
		switch msg {
		case t23Msg, msgLoaded, "corral starting", "corral startup complete":
			t.Errorf("record %q on the failure path", msg)
		}
	}
	if h.startedCalls() != 0 || after.Load() != 0 {
		t.Error("startup continued after a configuration error")
	}
}

// AC-7 (unit): an invalid setting with the admin port pre-bound exits 2 (not
// 1) and emits no lifecycle record; the port is left alone.
func TestAC7_InvalidSettingWithBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	code, out := runSync(t, runOpts{args: []string{"--admin.listen=" + busy.Addr().String(), "--log.level=bad"}})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if hasLifecycle(out.records(t), "starting") || hasLifecycle(out.records(t), "startup") {
		t.Error("a lifecycle record on the failure path")
	}
}

// BeforeLogger runs before the configuration is loaded, so it is reached on
// the failure path as well.
func TestBeforeLoggerRunsOnceBeforeLoad(t *testing.T) {
	var n atomic.Int32
	out := &syncBuf{}
	var linesAtHook int
	code, _ := runSync(t, runOpts{
		out: out, args: []string{"--log.level=bad"},
		beforeLogger: func(context.Context) { n.Add(1); linesAtHook = len(out.lines()) },
	})
	if code != 2 || n.Load() != 1 {
		t.Fatalf("exit %d, BeforeLogger calls %d", code, n.Load())
	}
	if linesAtHook != 0 {
		t.Errorf("%d records existed before BeforeLogger", linesAtHook)
	}
}

// ---- success path ----

// AC-11 (unit): FR-8 warning, FR-41 warning, FR-20 info, lifecycle starting,
// in that order; FR-20 names the supplied settings only.
func TestSuccessPath_OrderAndFR20Record(t *testing.T) {
	m := marker(t, "okmark")
	cert, _ := t23Certs(t)
	addr := reserveAddr(t)
	h := run(t, runOpts{
		args: []string{"--log.level=debug", "--tracing.enabled=true"},
		env: map[string]string{
			"CORRAL_ADMIN_LISTEN": addr, "CORRAL_ZZ_TYPO": m,
			"OTEL_EXPORTER_OTLP_ENDPOINT":    "http://" + otlpcollector.UnreachableEndpoint(t),
			"OTEL_EXPORTER_OTLP_CERTIFICATE": cert,
		},
	})
	h.waitStarted()
	if code := h.stop(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	recs := h.out.records(t)
	order := []string{msgUnknownEnv, t23Msg, msgLoaded, "corral starting"}
	last := -1
	for _, msg := range order {
		i := indexOfMsg(recs, msg)
		if i < 0 {
			t.Fatalf("record %q missing; got %q", msg, msgsOf(recs))
		}
		if i <= last {
			t.Fatalf("record %q out of order; got %q", msg, msgsOf(recs))
		}
		last = i
	}
	r := recs[indexOfMsg(recs, msgLoaded)]
	if str(r, "level") != "INFO" || str(r, "component") != "config" {
		t.Errorf("FR-20 record level/component = %v/%v", r["level"], r["component"])
	}
	if got := str(r, "setting"); got != "admin.listen,log.level,tracing.enabled" {
		t.Errorf("FR-20 setting = %q, want admin.listen,log.level,tracing.enabled", got)
	}
	n := 0
	for _, x := range recs {
		if str(x, "msg") == msgLoaded {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d FR-20 records, want exactly 1", n)
	}
	if strings.Contains(string(h.out.bytes()), m) || strings.Contains(string(h.out.bytes()), addr) {
		t.Error("output echoes a supplied value")
	}
}

// AC-11: at log.level=warn the FR-20 record is hidden; the warnings remain.
func TestSuccessPath_LevelWarnHidesFR20(t *testing.T) {
	cert, _ := t23Certs(t)
	h := run(t, runOpts{
		args: []string{"--log.level=warn", "--tracing.enabled=true", "--admin.listen=" + reserveAddr(t)},
		env: map[string]string{
			"CORRAL_ZZ_TYPO": "x", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + otlpcollector.UnreachableEndpoint(t),
			"OTEL_EXPORTER_OTLP_CERTIFICATE": cert,
		},
	})
	h.waitStarted()
	h.stop()
	recs := h.out.records(t)
	if indexOfMsg(recs, msgLoaded) >= 0 {
		t.Error("FR-20 record emitted at log.level=warn")
	}
	if indexOfMsg(recs, msgUnknownEnv) < 0 || indexOfMsg(recs, t23Msg) < 0 {
		t.Errorf("warnings missing at log.level=warn: %q", msgsOf(recs))
	}
}

// FR-20 on a successful start at the default level; no FR-8/FR-41 records
// when they do not apply; the success path honours log.format.
func TestSuccessPath_FR20DefaultLevelAndTextFormat(t *testing.T) {
	addr := reserveAddr(t)
	h := run(t, runOpts{args: []string{"--admin.listen=" + addr, "--log.format=text"}})
	h.waitStarted()
	h.stop()
	var cfg []string
	for _, l := range h.out.lines() {
		if strings.Contains(l, "component=config") {
			cfg = append(cfg, l)
		}
	}
	if len(cfg) != 1 || !strings.Contains(cfg[0], msgLoaded) || !strings.Contains(cfg[0], "setting=admin.listen") {
		t.Fatalf("config lines = %q, want exactly the FR-20 record in text format", cfg)
	}
}

// FR-20: the record lists names, never values.
func TestSuccessPath_FR20NoValues(t *testing.T) {
	addr := reserveAddr(t)
	h := run(t, runOpts{args: []string{"--admin.listen=" + addr, "--tracing.sample_ratio=0.25"}})
	h.waitStarted()
	h.stop()
	recs := h.out.records(t)
	r := recs[indexOfMsg(recs, msgLoaded)]
	if got := str(r, "setting"); got != "admin.listen,tracing.sample_ratio" {
		t.Errorf("setting = %q", got)
	}
	if strings.Contains(string(h.out.bytes()), addr) || strings.Contains(string(h.out.bytes()), "0.25") {
		t.Error("a value appears in the output")
	}
}

// ---- exit codes (FR-24) ----

func TestExitCodes(t *testing.T) {
	// 2: configuration error; 0: graceful; 0/2: check-config.
	if code, _ := runSync(t, runOpts{args: []string{"--bogus"}}); code != 2 {
		t.Errorf("config error: exit %d, want 2", code)
	}
	if code, _ := runSync(t, runOpts{args: []string{"--check-config"}}); code != 0 {
		t.Errorf("check-config success: exit %d, want 0", code)
	}
	if code, _ := runSync(t, runOpts{args: []string{"--check-config", "stray"}}); code != 2 {
		t.Errorf("check-config failure: exit %d, want 2", code)
	}
	h := run(t, runOpts{args: []string{"--admin.listen=" + reserveAddr(t)}})
	h.waitStarted()
	if code := h.stop(); code != 0 {
		t.Errorf("graceful shutdown: exit %d, want 0", code)
	}
	// 1: admin bind failure (configuration valid), through the configured logger.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	code, out := runSync(t, runOpts{args: []string{"--admin.listen=" + busy.Addr().String()}})
	if code != 1 {
		t.Errorf("bind failure: exit %d, want 1", code)
	}
	recs := out.records(t)
	last := recs[len(recs)-1]
	if str(last, "setting") != "admin.listen" || str(last, "level") != "ERROR" || str(last, "component") != "config" {
		t.Errorf("last record = %v, want the admin.listen bind-failure error", last)
	}
	if str(last, "msg") == "invalid configuration" {
		t.Error("a bind failure is logged as an invalid configuration (exit 2 records)")
	}
	if indexOfMsg(recs, msgLoaded) < 0 {
		t.Error("the FR-20 record is missing before the bind failure (configuration was valid)")
	}
}

// ---- SIGHUP in a normal run (FR-18, AC-10 unit part) ----

func reloadRecords(out *syncBuf) []map[string]any {
	var rs []map[string]any
	for _, r := range jsonRecordsNoFail(out.lines()) {
		if str(r, "msg") == msgReload {
			rs = append(rs, r)
		}
	}
	return rs
}

// A SIGHUP during BeforeLogger produces no record (and is not replayed); one
// after the configured logger exists produces one warn record, and the
// process keeps running.
func TestSIGHUPNormalRun(t *testing.T) {
	addr := reserveAddr(t)
	hup := make(chan os.Signal, 64)
	clk := fakeclock.New(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	h := run(t, runOpts{
		hup: hup, now: clk.Now,
		args:         []string{"--admin.listen=" + addr},
		beforeLogger: func(context.Context) { sendHUPs(hup, 5); time.Sleep(100 * time.Millisecond) },
	})
	h.waitStarted()
	time.Sleep(100 * time.Millisecond)
	if n := len(reloadRecords(h.out)); n != 0 {
		t.Fatalf("%d reload records for SIGHUPs sent before the logger existed", n)
	}
	// After start: 6 signals at one instant give one record (AC-10 unit part).
	sendHUPs(hup, 6)
	eventually(t, "the reload record", func() bool { return len(reloadRecords(h.out)) == 1 })
	time.Sleep(100 * time.Millisecond)
	recs := reloadRecords(h.out)
	if len(recs) != 1 {
		t.Fatalf("%d reload records for 6 signals, want 1", len(recs))
	}
	if r := recs[0]; str(r, "level") != "WARN" || str(r, "component") != "config" {
		t.Errorf("reload record level/component = %v/%v", r["level"], r["component"])
	} else if _, has := r["suppressed_count"]; has {
		t.Error("first reload record carries suppressed_count")
	}
	// The process continues serving.
	if r, err := get(addr, "/healthz"); err != nil || r.status != 200 {
		t.Fatalf("/healthz after SIGHUP: %v %d", err, r.status)
	}
	// 30 s later: the next signal carries suppressed_count=5.
	clk.Advance(30 * time.Second)
	sendHUPs(hup, 1)
	eventually(t, "the second reload record", func() bool { return len(reloadRecords(h.out)) == 2 })
	if n, _ := reloadRecords(h.out)[1]["suppressed_count"].(float64); n != 5 {
		t.Errorf("suppressed_count = %v, want 5", n)
	}
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
