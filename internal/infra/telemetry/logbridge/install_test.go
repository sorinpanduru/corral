package logbridge_test

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"io"
	"log"
	"log/slog"
	"strings"
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
	oldH := otel.GetErrorHandler()
	t.Cleanup(func() {
		slog.SetDefault(oldDef)
		log.SetOutput(oldW)
		log.SetFlags(oldF)
		log.SetPrefix(oldP)
		otel.SetErrorHandler(oldH)
		// otel.SetLogger and grpclog have no getters, so the previous values
		// cannot be captured: reset them to discarding sinks.
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

	grpclog.Warning(m)
	expect(t, e.last(), slog.LevelWarn, "grpc", 0)

	//constmsg:allow simulated third-party caller
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
	// Window elapsed. slog.Default dropped nothing in the first window, so its
	// next record reports 0; the std log bridge dropped 4, so it reports 4.
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

// FR-28 (T8 review): Install wires the otel logger. The SDK logs "TracerProvider
// created" with its configuration through the otel global logger; that must
// arrive as exactly one component=otel debug record with the fixed message and
// none of the configuration text (a marker seeded in the resource).
func TestInstallRoutesOTelLogger(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	logbridge.Install(e.f, e.lv, e.clk.Now)

	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attribute.String("synthetic.key", m))))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	r := e.only()
	expect(t, r, slog.LevelDebug, "otel", 0)
	if r.Msg != logbridge.MsgOTelLogDebug {
		t.Errorf("message = %q, want %q", r.Msg, logbridge.MsgOTelLogDebug)
	}
	e.noMarker(m)
	if strings.Contains(e.buf.String(), "config") {
		t.Errorf("configuration text in output: %s", e.buf.String())
	}
}
