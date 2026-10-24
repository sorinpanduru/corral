package logbridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
	"go.opentelemetry.io/otel"
)

type otelErrorHandler struct {
	lg  *slog.Logger
	lim *ratelimit.Limiter
}

// NewOTelErrorHandler returns an otel.ErrorHandler logging at warn with
// component=otel, the fixed MsgOTelError and errclass.LogAttrs only (FR-28,
// FR-33).
func NewOTelErrorHandler(f *logging.Factory, now func() time.Time) otel.ErrorHandler {
	return &otelErrorHandler{lg: logger(f, logging.OTel), lim: newLimiter(now)}
}

func (h *otelErrorHandler) Handle(err error) {
	ctx := context.Background()
	if !enabled(ctx, h.lg, nil, slog.LevelWarn) {
		return
	}
	sc, ok := allow(h.lim)
	if !ok {
		return
	}
	attrs := append(errclass.LogAttrs(err), sc)
	h.lg.LogAttrs(ctx, slog.LevelWarn, MsgOTelError, attrs...)
}

type otelLogSink struct {
	lg   *slog.Logger
	info *ratelimit.Limiter
	errl *ratelimit.Limiter
}

// NewOTelLogSink returns a logr.LogSink for otel.SetLogger: Info at any
// verbosity maps to debug, Error to warn. Message, names and key/values are
// never forwarded (FR-28, D7).
func NewOTelLogSink(f *logging.Factory, now func() time.Time) logr.LogSink {
	return &otelLogSink{lg: logger(f, logging.OTel), info: newLimiter(now), errl: newLimiter(now)}
}

func (s *otelLogSink) Init(logr.RuntimeInfo) {}

// Enabled follows the factory handler at debug, the level Info maps to.
func (s *otelLogSink) Enabled(int) bool {
	return s.lg.Enabled(context.Background(), slog.LevelDebug)
}

func (s *otelLogSink) WithValues(...any) logr.LogSink { return s }
func (s *otelLogSink) WithName(string) logr.LogSink   { return s }

func (s *otelLogSink) Info(_ int, _ string, _ ...any) {
	ctx := context.Background()
	if !enabled(ctx, s.lg, nil, slog.LevelDebug) {
		return
	}
	sc, ok := allow(s.info)
	if !ok {
		return
	}
	s.lg.LogAttrs(ctx, slog.LevelDebug, MsgOTelLogDebug, sc)
}

func (s *otelLogSink) Error(err error, _ string, _ ...any) {
	ctx := context.Background()
	if !enabled(ctx, s.lg, nil, slog.LevelWarn) {
		return
	}
	sc, ok := allow(s.errl)
	if !ok {
		return
	}
	attrs := append(errclass.LogAttrs(err), sc)
	s.lg.LogAttrs(ctx, slog.LevelWarn, MsgOTelLogWarn, attrs...)
}
