package logbridge

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
	"google.golang.org/grpc/grpclog"
)

type grpcLogger struct {
	lg                  *slog.Logger
	level               slog.Leveler
	debug, warn, errLim *ratelimit.Limiter
}

// NewGRPCLogger returns a grpclog.LoggerV2: info maps to debug, warning to
// warn, error to error. Fatal* writes an error record exempt from the limiter,
// flushes stdout and exits with status 1 (FR-28).
func NewGRPCLogger(f *logging.Factory, level slog.Leveler, now func() time.Time) grpclog.LoggerV2 {
	return &grpcLogger{
		lg: logger(f, logging.GRPC), level: level,
		debug: newLimiter(now), warn: newLimiter(now), errLim: newLimiter(now),
	}
}

func (g *grpcLogger) info() {
	ctx := context.Background()
	if !enabled(ctx, g.lg, g.level, slog.LevelDebug) {
		return
	}
	if sc, ok := allow(g.debug); ok {
		g.lg.LogAttrs(ctx, slog.LevelDebug, MsgGRPCDebug, sc)
	}
}

func (g *grpcLogger) warning() {
	ctx := context.Background()
	if !enabled(ctx, g.lg, g.level, slog.LevelWarn) {
		return
	}
	if sc, ok := allow(g.warn); ok {
		g.lg.LogAttrs(ctx, slog.LevelWarn, MsgGRPCWarn, sc)
	}
}

func (g *grpcLogger) error() {
	ctx := context.Background()
	if !enabled(ctx, g.lg, g.level, slog.LevelError) {
		return
	}
	if sc, ok := allow(g.errLim); ok {
		g.lg.LogAttrs(ctx, slog.LevelError, MsgGRPCError, sc)
	}
}

// fatal is exempt from the limiter; the record is written and flushed before
// the process exits.
func (g *grpcLogger) fatal() {
	g.lg.LogAttrs(context.Background(), slog.LevelError, MsgGRPCFatal)
	_ = os.Stdout.Sync() // best effort: fails harmlessly on pipes
	os.Exit(1)
}

func (g *grpcLogger) Info(...any)             { g.info() }
func (g *grpcLogger) Infoln(...any)           { g.info() }
func (g *grpcLogger) Infof(string, ...any)    { g.info() }
func (g *grpcLogger) Warning(...any)          { g.warning() }
func (g *grpcLogger) Warningln(...any)        { g.warning() }
func (g *grpcLogger) Warningf(string, ...any) { g.warning() }
func (g *grpcLogger) Error(...any)            { g.error() }
func (g *grpcLogger) Errorln(...any)          { g.error() }
func (g *grpcLogger) Errorf(string, ...any)   { g.error() }
func (g *grpcLogger) Fatal(...any)            { g.fatal() }
func (g *grpcLogger) Fatalln(...any)          { g.fatal() }
func (g *grpcLogger) Fatalf(string, ...any)   { g.fatal() }

// V reports whether grpc info output (mapped to debug) would be emitted.
func (g *grpcLogger) V(int) bool {
	return g.level != nil && g.level.Level() <= slog.LevelDebug && g.lg.Enabled(context.Background(), slog.LevelDebug)
}
