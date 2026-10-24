package runtime_test

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// configRecord asserts out holds exactly one component=config record with the
// wanted setting at error level, and that "component" appears exactly once on
// its line (never a logger without component, never a doubled one).
func configRecord(t *testing.T, h *harness, wantSetting string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, l := range h.out.lines() {
		jr := jsonRecordsNoFail([]string{l})
		if len(jr) == 1 && str(jr[0], "component") == string(logging.Config) {
			found = append(found, jr[0])
			if n := strings.Count(l, `"component"`); n != 1 {
				t.Errorf("config record carries %d component keys, want exactly 1", n)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("component=config records = %d, want exactly 1 (error logged once)", len(found))
	}
	r := found[0]
	if str(r, "level") != "ERROR" {
		t.Errorf("config record level = %q, want ERROR", str(r, "level"))
	}
	if got := str(r, "setting"); got != wantSetting {
		t.Errorf("config record setting = %q, want %q", got, wantSetting)
	}
	return r
}

// FR-27, ADR-0006 steps 0-3, AC-21: settings errors are logged once through
// the bootstrap logger (JSON on stdout, component=config), no later startup
// step runs, exit is non-zero, the supplied value is not echoed.
func TestConfigErrorBootstrapLogger(t *testing.T) { // AC-21, FR-27
	cases := []struct {
		name    string
		args    func(m string) []string
		setting string
	}{
		{"log.level", func(m string) []string { return []string{"-log.level=" + m} }, "log.level"},
		{"log.format", func(m string) []string { return []string{"-log.format=" + m} }, "log.format"},
		{"admin.listen", func(m string) []string { return []string{"-admin.listen=:9187"} }, "admin.listen"},
		{"tracing.sample_ratio", func(m string) []string { return []string{"-tracing.sample_ratio=" + m} }, "tracing.sample_ratio"},
		{"unknown flag", func(m string) []string { return []string{"-" + m + "=1"} }, "unknown"},
		{"positional", func(m string) []string { return []string{m} }, "argument"},
		// A text-format request does not matter: settings parsing fails before
		// the configured handler exists, so the bootstrap JSON logger is used.
		{"bad setting with log.format=text", func(m string) []string {
			return []string{"-log.format=text", "-log.level=" + m}
		}, "log.level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := marker(t, "cfgmark")
			sc := markerscan.New()
			sc.AddMarker(t, m)
			var hooks atomic.Int32
			h := run(t, runOpts{
				args:   tc.args(m),
				before: func(context.Context) { hooks.Add(1) },
			})
			code := h.wait()
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero")
			}
			configRecord(t, h, tc.setting)
			if h.startedCalls() != 0 || hooks.Load() != 0 {
				t.Errorf("later startup steps ran after a config error (Started=%d, BeforeLastStep=%d)", h.startedCalls(), hooks.Load())
			}
			if hasLifecycle(h.out.records(t), "starting") {
				t.Errorf("lifecycle starting record emitted despite a config error (step 4 follows steps 0-3)")
			}
			sc.Add(markerscan.SourceStdout, h.out.bytes())
			sc.AssertClean(t)
		})
	}
}

// FR-27, ADR-0006 step 3: with tracing enabled, an invalid OTEL_* value is
// logged once through Factory.Logger(logging.Config): the configured format is
// honoured (text here), component=config appears exactly once, no value echo.
func TestConfigErrorFactoryLoggerAfterHandlerBuilt(t *testing.T) { // AC-21, FR-27
	m := marker(t, "otelmark")
	sc := markerscan.New()
	sc.AddMarker(t, m)
	var hooks atomic.Int32
	h := run(t, runOpts{
		args:   []string{"-tracing.enabled=true", "-log.format=text", "-admin.listen=" + reserveAddr(t)},
		env:    map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": m},
		before: func(context.Context) { hooks.Add(1) },
	})
	if code := h.wait(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if h.startedCalls() != 0 || hooks.Load() != 0 {
		t.Errorf("later startup steps ran after an OTEL_* error")
	}
	// Text format: the line is not JSON but carries exactly one component=config
	// and the offending variable name as the setting.
	var cfg []string
	for _, l := range h.out.lines() {
		if strings.Contains(l, "component=config") {
			cfg = append(cfg, l)
		}
	}
	if len(cfg) != 1 {
		t.Fatalf("component=config lines = %d, want exactly 1; lines=%d", len(cfg), len(h.out.lines()))
	}
	if n := strings.Count(cfg[0], "component="); n != 1 {
		t.Errorf("config line has %d component attributes, want 1", n)
	}
	if !strings.Contains(cfg[0], "setting=OTEL_EXPORTER_OTLP_PROTOCOL") {
		t.Errorf("config line does not name the offending variable as setting")
	}
	sc.Add(markerscan.SourceStdout, h.out.bytes())
	sc.AssertClean(t)

	// The same text-format request in JSON (default format) is JSON.
	h2 := run(t, runOpts{
		args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
		env:  map[string]string{"OTEL_TRACES_SAMPLER": "jaeger_remote"},
	})
	if code := h2.wait(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	configRecord(t, h2, "OTEL_TRACES_SAMPLER")
}

// With tracing off, OTEL_* values are ignored (FR-27) and startup succeeds.
func TestInvalidOTELIgnoredWhenTracingOff(t *testing.T) { // AC-21, FR-27
	h := run(t, runOpts{
		args: []string{"-admin.listen=" + reserveAddr(t)},
		env:  map[string]string{"OTEL_TRACES_SAMPLER": "jaeger_remote", "OTEL_EXPORTER_OTLP_PROTOCOL": "bogus-protocol-value"},
	})
	h.waitStarted()
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if n := len(byComponent(h.out.records(t), logging.Config)); n != 0 {
		t.Errorf("config records = %d with tracing off, want 0", n)
	}
}

// --name=value is accepted as well as -name=value (ADR-0003 amendment 1).
func TestDoubleDashFlagAccepted(t *testing.T) { // AC-21
	addr := reserveAddr(t)
	h := run(t, runOpts{args: []string{"--admin.listen=" + addr, "--log.level=DEBUG"}})
	hd := h.waitStarted()
	if hd.AdminAddr.String() != addr {
		t.Errorf("AdminAddr = %v, want %s", hd.AdminAddr, addr)
	}
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// AC-30 / FR-35: an admin.listen address already in use is a startup failure,
// logged through config.LogError (component=config, setting=admin.listen) with
// no address text, exit non-zero, no startup-complete record, no later steps.
func TestBindFailureEADDRINUSE(t *testing.T) { // AC-30, FR-35
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()
	addr := busy.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]

	var hooks atomic.Int32
	h := run(t, runOpts{
		args:   []string{"-admin.listen=" + addr},
		before: func(context.Context) { hooks.Add(1) },
	})
	if code := h.wait(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	r := configRecord(t, h, "admin.listen")
	out := string(h.out.bytes())
	for _, bad := range []string{addr, ":" + port, "127.0.0.1"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains address text %q", bad)
		}
	}
	if _, ok := r["error"]; !ok {
		t.Errorf("config record has no error class attribute (errclass via config.LogError)")
	}
	if hasLifecycle(h.out.records(t), "startup complete") {
		t.Errorf("startup-complete record after a bind failure")
	}
	if h.startedCalls() != 0 || hooks.Load() != 0 {
		t.Errorf("Started/BeforeLastStep ran after a bind failure")
	}
}
