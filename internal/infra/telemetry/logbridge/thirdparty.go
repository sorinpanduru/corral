package logbridge

import (
	"context"
	"io"
	"log"
	"log/slog"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/earlyslog"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
)

// levelLimiters holds one limiter per level bucket.
type levelLimiters [4]*ratelimit.Limiter

func newLevelLimiters(now func() time.Time) *levelLimiters {
	var l levelLimiters
	for i := range l {
		l[i] = newLimiter(now)
	}
	return &l
}

func (l *levelLimiters) of(lvl slog.Level) *ratelimit.Limiter {
	switch {
	case lvl < slog.LevelInfo:
		return l[0]
	case lvl < slog.LevelWarn:
		return l[1]
	case lvl < slog.LevelError:
		return l[2]
	default:
		return l[3]
	}
}

// bridgeHandler is a slog.Handler that discards the record's message and
// attributes and emits one fixed message, limited per level. The emit closures exist so
// that each LogAttrs call site passes a constant message the lint can see.
type bridgeHandler struct {
	lg    *slog.Logger
	level slog.Leveler
	lim   *levelLimiters
	// emit writes the constant message at the given level.
	emit func(ctx context.Context, lg *slog.Logger, lvl slog.Level, sc slog.Attr)
}

func (h *bridgeHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return enabled(ctx, h.lg, h.level, lvl)
}

func (h *bridgeHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}
	if sc, ok := allow(h.lim.of(r.Level)); ok {
		h.emit(ctx, h.lg, r.Level, sc)
	}
	return nil
}

// WithAttrs and WithGroup drop their arguments: third-party state is never
// forwarded (FR-28).
func (h *bridgeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *bridgeHandler) WithGroup(string) slog.Handler      { return h }

// NewSlogDefaultHandler returns the handler for slog.Default(): the caller's
// level is kept, message and attributes are replaced by MsgThirdPartySlogDflt
// with component=thirdparty (FR-28, D5).
func NewSlogDefaultHandler(f *logging.Factory, level slog.Leveler, now func() time.Time) slog.Handler {
	return &bridgeHandler{
		lg: logger(f, logging.ThirdParty), level: level, lim: newLevelLimiters(now),
		emit: func(ctx context.Context, lg *slog.Logger, lvl slog.Level, sc slog.Attr) {
			lg.LogAttrs(ctx, lvl, MsgThirdPartySlogDflt, sc)
		},
	}
}

// NewHTTPErrorLog returns the admin http.Server ErrorLog: every record is
// emitted at warn with component=admin_http and the fixed MsgAdminHTTPError.
func NewHTTPErrorLog(f *logging.Factory, now func() time.Time) *log.Logger {
	h := &bridgeHandler{
		lg: logger(f, logging.AdminHTTP), lim: newLevelLimiters(now),
		emit: func(ctx context.Context, lg *slog.Logger, lvl slog.Level, sc slog.Attr) {
			lg.LogAttrs(ctx, lvl, MsgAdminHTTPError, sc)
		},
	}
	return slog.NewLogLogger(h, slog.LevelWarn)
}

type stdLogWriter struct {
	h *bridgeHandler
}

// NewStdLogWriter returns the io.Writer for log.SetOutput: each write is one
// info record, component=thirdparty, fixed MsgThirdPartyStdLog. The written
// bytes are discarded (FR-28, D5).
func NewStdLogWriter(f *logging.Factory, level slog.Leveler, now func() time.Time) io.Writer {
	return &stdLogWriter{h: &bridgeHandler{
		lg: logger(f, logging.ThirdParty), level: level, lim: newLevelLimiters(now),
		emit: func(ctx context.Context, lg *slog.Logger, lvl slog.Level, sc slog.Attr) {
			lg.LogAttrs(ctx, lvl, MsgThirdPartyStdLog, sc)
		},
	}}
}

func (w *stdLogWriter) Write(p []byte) (int, error) {
	ctx := context.Background()
	if w.h.Enabled(ctx, slog.LevelInfo) {
		if sc, ok := allow(w.h.lim.of(slog.LevelInfo)); ok {
			w.h.emit(ctx, w.h.lg, slog.LevelInfo, sc)
		}
	}
	return len(p), nil
}

// Install wires every source in order: otel error handler, otel logger,
// grpclog, slog.Default() bridge, then the std log bridge. slog.SetDefault
// redirects the log package into the default handler, so the std log output
// is pointed at its own writer afterwards to keep the two sources distinct.
func Install(f *logging.Factory, level slog.Leveler, now func() time.Time) {
	installOTel(f, now)
	installGRPC(f, level, now)
	dh := NewSlogDefaultHandler(f, level, now)
	slog.SetDefault(slog.New(dh))
	replayEarly(dh, earlyslog.Drain())
	log.SetOutput(NewStdLogWriter(f, level, now))
}

// replayEarly reports records that third-party package initialisers logged
// before Install (see package earlyslog). Only the per-level occurrence is
// replayed, once per level, through the rate-limited fixed-message handler.
func replayEarly(h slog.Handler, c earlyslog.Counts) {
	ctx := context.Background()
	for _, e := range []struct {
		n   uint64
		lvl slog.Level
	}{{c.Debug, slog.LevelDebug}, {c.Info, slog.LevelInfo}, {c.Warn, slog.LevelWarn}, {c.Error, slog.LevelError}} {
		if e.n > 0 {
			_ = h.Handle(ctx, slog.NewRecord(time.Time{}, e.lvl, "", 0))
		}
	}
}
