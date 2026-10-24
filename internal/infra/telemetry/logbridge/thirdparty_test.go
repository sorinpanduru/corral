package logbridge_test

import (
	"context"
	"log"
	"log/slog"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
)

// AC-26 (FR-28): admin HTTP error log -> one warn record, component=admin_http,
// fixed message, text never forwarded. Control characters in the text must not
// leak either.
func TestHTTPErrorLogRouting(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	l := logbridge.NewHTTPErrorLog(e.f, e.clk.Now)
	l.Printf("http: TLS handshake error from 192.0.2.1:1234: %s\n\x1b[31m\r\n", m)
	expect(t, e.only(), slog.LevelWarn, "admin_http", 0)
	first := e.last().Msg
	e.clk.Advance(win)
	l.Println(m, "second")
	if e.count() != 2 || e.last().Msg != first {
		t.Fatalf("message must be one constant: %+v", e.records())
	}
	e.noMarker(m)
}

// AC-26 (FR-28, FR-33): admin HTTP limit, 10 in 30 s -> 1, then 9 suppressed.
func TestHTTPErrorLogLimit(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	l := logbridge.NewHTTPErrorLog(e.f, e.clk.Now)
	for i := 0; i < 10; i++ {
		l.Print("x")
	}
	expect(t, e.only(), slog.LevelWarn, "admin_http", 0)
	e.clk.Advance(win)
	l.Print("x")
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2", e.count())
	}
	expect(t, e.last(), slog.LevelWarn, "admin_http", 9)
}

// D7: warn records are filtered at error level before consuming the slot.
func TestHTTPErrorLogLevelFilterBeforeLimiter(t *testing.T) {
	e := newEnv(t, slog.LevelError)
	l := logbridge.NewHTTPErrorLog(e.f, e.clk.Now)
	l.Print("x")
	l.Print("x")
	if e.count() != 0 {
		t.Fatalf("records = %d, want 0", e.count())
	}
	e.lv.Set(slog.LevelInfo)
	l.Print("x")
	expect(t, e.only(), slog.LevelWarn, "admin_http", 0)
}

// D5 (FR-28 amendment): std log bridge -> info, component=thirdparty, fixed
// message; text, prefix and flags never forwarded.
func TestStdLogBridgeRouting(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	w := logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now)
	l := log.New(w, m+": ", log.LstdFlags|log.Lshortfile)
	l.Printf("%s\n\x1b[31m", m)
	expect(t, e.only(), slog.LevelInfo, "thirdparty", 0)
	first := e.last().Msg
	e.clk.Advance(win)
	l.Print(marker(t))
	if e.count() != 2 || e.last().Msg != first {
		t.Fatalf("message must be a constant: %+v", e.records())
	}
	e.noMarker(m)
}

// The writer honours the io.Writer contract.
func TestStdLogWriterReturnsLength(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	w := logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now)
	p := []byte("hello\n")
	if n, err := w.Write(p); n != len(p) || err != nil {
		t.Fatalf("Write = (%d,%v), want (%d,nil)", n, err, len(p))
	}
}

// D5: std log records follow the configured level (info emitted at info and
// debug; filtered at warn and error) and filtered records keep the slot free.
func TestStdLogBridgeFollowsConfiguredLevel(t *testing.T) {
	e := newEnv(t, slog.LevelWarn)
	l := log.New(logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now), "", 0)
	l.Print("x")
	if e.count() != 0 {
		t.Fatalf("std log emitted at warn level: %d records", e.count())
	}
	e.lv.Set(slog.LevelDebug)
	l.Print("x")
	expect(t, e.only(), slog.LevelInfo, "thirdparty", 0)
}

// FR-33: std log bridge limit.
func TestStdLogBridgeLimit(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	l := log.New(logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now), "", 0)
	for i := 0; i < 10; i++ {
		l.Print("x")
	}
	expect(t, e.only(), slog.LevelInfo, "thirdparty", 0)
	e.clk.Advance(win)
	l.Print("x")
	expect(t, e.last(), slog.LevelInfo, "thirdparty", 9)
}

// D5, AC-26: slog.Default() bridge keeps the caller's level, replaces the
// message, drops attributes, groups and With* state; a caller-supplied
// "component" never reaches the record.
func TestSlogDefaultBridgeKeepsLevelAndDropsContent(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	lg := slog.New(logbridge.NewSlogDefaultHandler(e.f, e.lv, e.clk.Now))
	lg = lg.With(m, m, "component", m).WithGroup(m)

	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for _, lvl := range levels {
		lg.Log(context.Background(), lvl, m, m, m, slog.Group(m, slog.String(m, m)))
	}
	rs := e.records()
	if len(rs) != len(levels) {
		t.Fatalf("records = %d, want %d (limiter is per level)", len(rs), len(levels))
	}
	msg := rs[0].Msg
	for i, r := range rs {
		expect(t, r, levels[i], "thirdparty", 0)
		if r.Msg != msg {
			t.Errorf("message %q differs from %q", r.Msg, msg)
		}
	}
	e.noMarker(m)
}

// D5: follows the configured level, no warn-and-above rule.
func TestSlogDefaultBridgeFollowsConfiguredLevel(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	lg := slog.New(logbridge.NewSlogDefaultHandler(e.f, e.lv, e.clk.Now))
	lg.Debug("x")
	if e.count() != 0 {
		t.Fatalf("debug emitted at info level")
	}
	lg.Info("x")
	expect(t, e.only(), slog.LevelInfo, "thirdparty", 0)
	if lg.Handler().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(debug) true at info level")
	}
}

// FR-33: slog.Default bridge limit is per level.
func TestSlogDefaultBridgeLimit(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	lg := slog.New(logbridge.NewSlogDefaultHandler(e.f, e.lv, e.clk.Now))
	for i := 0; i < 10; i++ {
		lg.Info("x")
	}
	lg.Warn("x") // different level, own slot
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2", e.count())
	}
	e.clk.Advance(win)
	lg.Info("x")
	expect(t, e.last(), slog.LevelInfo, "thirdparty", 9)
}

// D7/amendment: the std log bridge and slog.Default bridge have separate
// limiters, so a burst on one does not suppress the other, in both directions.
func TestStdAndSlogDefaultBridgesAreIndependent(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	std := log.New(logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now), "", 0)
	def := slog.New(logbridge.NewSlogDefaultHandler(e.f, e.lv, e.clk.Now))
	for i := 0; i < 5; i++ {
		std.Print("x")
	}
	def.Info("y") // same level (info), same component, other source
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2 (std burst must not suppress slog.Default)", e.count())
	}
	expect(t, e.last(), slog.LevelInfo, "thirdparty", 0)

	e2 := newEnv(t, slog.LevelInfo)
	std2 := log.New(logbridge.NewStdLogWriter(e2.f, e2.lv, e2.clk.Now), "", 0)
	def2 := slog.New(logbridge.NewSlogDefaultHandler(e2.f, e2.lv, e2.clk.Now))
	for i := 0; i < 5; i++ {
		def2.Info("y")
	}
	std2.Print("x")
	if e2.count() != 2 {
		t.Fatalf("records = %d, want 2 (slog.Default burst must not suppress std log)", e2.count())
	}
	expect(t, e2.last(), slog.LevelInfo, "thirdparty", 0)
}
