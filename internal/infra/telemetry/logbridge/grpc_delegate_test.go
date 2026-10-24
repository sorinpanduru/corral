package logbridge_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"google.golang.org/grpc/grpclog"
)

// FR-28 (T18 review): a second Install swaps the grpclog target: grpc logs
// route to the newest factory only. Not parallel (process-wide state).
func TestInstallSecondCallRoutesGRPCToNewestFactory(t *testing.T) {
	restoreGlobals(t)
	first := newEnv(t, slog.LevelDebug)
	second := newEnv(t, slog.LevelDebug)
	m := marker(t)

	logbridge.Install(first.f, first.lv, first.clk.Now)
	grpclog.Warning(m)
	expect(t, first.only(), slog.LevelWarn, "grpc", 0)

	logbridge.Install(second.f, second.lv, second.clk.Now)
	grpclog.Error(m)
	expect(t, second.only(), slog.LevelError, "grpc", 0)
	if first.count() != 1 {
		t.Errorf("first factory received %d records after the second Install, want it to stay at 1", first.count())
	}
	first.noMarker(m)
	second.noMarker(m)
}

// FR-28 (T18 review): Install re-takes the grpclog global after a foreign
// grpclog.SetLoggerV2, so grpc logs never reach the foreign logger (which
// would write to stderr by default).
func TestInstallRetakesGRPCAfterForeignSetLoggerV2(t *testing.T) {
	restoreGlobals(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	logbridge.Install(e.f, e.lv, e.clk.Now)
	grpclog.Warning(m)
	if e.count() != 1 {
		t.Fatalf("records = %d before the foreign logger, want 1", e.count())
	}

	var foreign countingW
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(&foreign, &foreign, &foreign))
	grpclog.Warning(m) // goes to the foreign logger: sanity check of the setup
	if foreign.n == 0 {
		t.Fatal("test setup: foreign grpclog logger received nothing")
	}
	foreign.n = 0

	e2 := newEnv(t, slog.LevelDebug)
	logbridge.Install(e2.f, e2.lv, e2.clk.Now)
	grpclog.Warning(m)
	grpclog.Error(m)
	if foreign.n != 0 {
		t.Errorf("foreign grpclog logger received %d writes after Install, want 0", foreign.n)
	}
	expect(t, e2.last(), slog.LevelError, "grpc", 0)
	if e2.count() != 2 {
		t.Errorf("records on the new factory = %d, want 2 (warn and error)", e2.count())
	}
	e2.noMarker(m)
}

type countingW struct{ n int }

func (c *countingW) Write(p []byte) (int, error) { c.n++; return len(p), nil }

var _ io.Writer = (*countingW)(nil)
