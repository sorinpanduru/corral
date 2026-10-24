package logbridge_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
)

// AC-26 (FR-28): grpclog warning -> one warn record, component=grpc, fixed
// message, marker text never forwarded (all call variants).
func TestGRPCWarningRouting(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	l.Warning(m, m)
	expect(t, e.only(), slog.LevelWarn, "grpc", 0)
	first := e.last().Msg
	e.clk.Advance(win)
	l.Warningln(m, m)
	e.clk.Advance(win)
	l.Warningf("%s %d", m, 1)
	rs := e.records()
	if len(rs) != 3 {
		t.Fatalf("records = %d, want 3", len(rs))
	}
	for _, r := range rs {
		if r.Msg != first {
			t.Errorf("message %q differs from %q: must be one constant per level", r.Msg, first)
		}
	}
	e.noMarker(m)
}

// AC-26 (FR-28): grpclog info -> debug, not emitted at info, emitted at debug.
func TestGRPCInfoMapsToDebug(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	l.Info(m)
	l.Infoln(m)
	l.Infof("%s", m)
	if e.count() != 0 {
		t.Fatalf("grpclog info emitted %d records at info level, want 0", e.count())
	}
	e.lv.Set(slog.LevelDebug)
	l.Info(m)
	expect(t, e.only(), slog.LevelDebug, "grpc", 0) // 0: filtered ones did not consume the slot
	e.noMarker(m)
}

// FR-28: error maps to error.
func TestGRPCErrorMapsToError(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	l.Error(m)
	expect(t, e.only(), slog.LevelError, "grpc", 0)
	e.clk.Advance(win)
	l.Errorln(m)
	e.clk.Advance(win)
	l.Errorf("%s", m)
	if e.count() != 3 {
		t.Fatalf("records = %d, want 3", e.count())
	}
	e.noMarker(m)
}

// AC-26 (FR-28, FR-33): 10 warnings within 30 s -> 1 record, next one
// reports suppressed_count 9.
func TestAC26GRPCWarningLimit(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	for i := 0; i < 10; i++ {
		l.Warning("w")
		e.clk.Advance(2 * time.Second)
	}
	expect(t, e.only(), slog.LevelWarn, "grpc", 0)
	e.clk.Advance(win)
	l.Warning("w")
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2", e.count())
	}
	expect(t, e.last(), slog.LevelWarn, "grpc", 9)
}

// D7: one limiter per level; Warning/Warningln/Warningf share one, and a burst
// of warnings does not suppress an error.
func TestGRPCLimiterPerLevel(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	l.Warning("a")
	l.Warningln("b")
	l.Warningf("c")
	l.Error("d")
	l.Info("e")
	rs := e.records()
	if len(rs) != 3 {
		t.Fatalf("records = %d, want 3 (warn, error, debug)", len(rs))
	}
	seen := map[slog.Level]bool{}
	for _, r := range rs {
		seen[r.Level] = true
		expect(t, r, r.Level, "grpc", 0)
	}
	for _, lvl := range []slog.Level{slog.LevelWarn, slog.LevelError, slog.LevelDebug} {
		if !seen[lvl] {
			t.Errorf("no record at %v", lvl)
		}
	}
}

// FR-28: V(l) reports according to the configured level.
func TestGRPCVerbosityFollowsLevel(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	l := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	if l.V(0) {
		t.Error("V(0) true at info level: grpc info maps to debug which is disabled")
	}
	e.lv.Set(slog.LevelDebug)
	if !l.V(0) {
		t.Error("V(0) false at debug level")
	}
}
