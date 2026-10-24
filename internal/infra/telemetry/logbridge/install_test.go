package logbridge_test

import (
	"errors"
	"io"
	"log"
	"log/slog"
	"testing"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"
)

// restoreGlobals puts the process-wide sinks back after Install. Tests using it
// must not run in parallel.
func restoreGlobals(t *testing.T) {
	t.Helper()
	oldDef, oldW, oldF, oldP := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(oldDef)
		log.SetOutput(oldW)
		log.SetFlags(oldF)
		log.SetPrefix(oldP)
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
		otel.SetLogger(logr.Discard())
		grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
	})
}

// FR-28: Install wires every source; std log and slog.Default stay distinct
// (separate limiters), and component/level routing holds through the globals.
func TestInstallRoutesAllSources(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	logbridge.Install(e.f, e.lv, e.clk.Now)

	otel.Handle(errors.New(m))
	expect(t, e.last(), slog.LevelWarn, "otel", 0)

	// NOTE: the otel logger (otel.SetLogger) has no public getter in otel
	// v1.47.0, so its Install wiring is covered only via NewOTelLogSink.

	grpclog.Warning(m)
	expect(t, e.last(), slog.LevelWarn, "grpc", 0)

	slog.Warn(m, m, m)
	expect(t, e.last(), slog.LevelWarn, "thirdparty", 0)

	log.Print(m)
	expect(t, e.last(), slog.LevelInfo, "thirdparty", 0)

	if e.count() != 4 {
		t.Fatalf("records = %d, want 4", e.count())
	}
	e.noMarker(m)
}

// D5/amendment: after Install, a burst of std log calls does not suppress
// slog.Default (std log output is not redirected into the default handler),
// and the reverse.
func TestInstallKeepsStdLogAndSlogDefaultDistinct(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelInfo)
	logbridge.Install(e.f, e.lv, e.clk.Now)

	for i := 0; i < 5; i++ {
		log.Print("x")
	}
	slog.Info("y")
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2: std log burst suppressed slog.Default", e.count())
	}
	e.clk.Advance(win)
	for i := 0; i < 5; i++ {
		slog.Info("y")
	}
	log.Print("x")
	// window elapsed: slog.Default emits again (suppressed 0 first? 0 events were
	// dropped for it), the std log bridge reports its 4 suppressed.
	rs := e.records()
	if len(rs) != 4 {
		t.Fatalf("records = %d, want 4", len(rs))
	}
	if got := rs[2].suppressed(t); got != 0 {
		t.Errorf("slog.Default suppressed_count = %d, want 0", got)
	}
	if got := rs[3].suppressed(t); got != 4 {
		t.Errorf("std log suppressed_count = %d, want 4", got)
	}
}

// FR-28: the Install level applies (grpc info is debug, filtered at info).
func TestInstallHonoursLevel(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelInfo)
	logbridge.Install(e.f, e.lv, e.clk.Now)
	grpclog.Info("x")
	slog.Debug("x")
	if e.count() != 0 {
		t.Fatalf("records = %d at info level, want 0", e.count())
	}
}
