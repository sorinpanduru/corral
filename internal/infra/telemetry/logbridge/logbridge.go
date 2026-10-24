// Package logbridge routes third-party and global log output (otel, grpclog,
// the admin http.Server error log, Go's log package and slog.Default()) through
// the corral slog factory (SPEC-001 FR-28, FR-33).
//
// Compliance (FR-24, FR-28, GDPR data minimisation): every source emits a
// fixed, constant message. The third-party message text, key/value arguments,
// names and prefixes are never forwarded. Errors are reported only through
// errclass.LogAttrs. Each source has one rate limiter per level (1 record per
// 30 s with suppressed_count); level filtering happens before limiting.
package logbridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
)

// Window is the FR-33 limiter window.
const Window = 30 * time.Second

// Fixed messages, one per source and level.
const (
	MsgOTelError          = "opentelemetry error"
	MsgOTelLogDebug       = "opentelemetry log message"
	MsgOTelLogWarn        = "opentelemetry logger error"
	MsgGRPCDebug          = "grpc log message"
	MsgGRPCWarn           = "grpc warning"
	MsgGRPCError          = "grpc error"
	MsgGRPCFatal          = "grpc fatal error"
	MsgAdminHTTPError     = "admin http server error"
	MsgThirdPartyStdLog   = "standard log package message"
	MsgThirdPartySlogDflt = "default slog logger message"
)

// logger returns the factory logger for c. The components used here are
// constants in the fixed set, so an error cannot occur; a discard logger is
// the safe fallback.
func logger(f *logging.Factory, c logging.Component) *slog.Logger {
	l, err := f.Logger(c)
	if err != nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}

func newLimiter(now func() time.Time) *ratelimit.Limiter { return ratelimit.New(Window, now) }

// allow consults l and returns the attributes to attach, or ok=false when the
// record is suppressed. suppressed_count is always an int64.
func allow(l *ratelimit.Limiter) (slog.Attr, bool) {
	emit, n := l.Allow()
	return slog.Int64(attr.SuppressedCount, int64(n)), emit
}

// enabled applies level filtering before the limiter is consulted (D7).
func enabled(ctx context.Context, lg *slog.Logger, min slog.Leveler, lvl slog.Level) bool {
	if min != nil && lvl < min.Level() {
		return false
	}
	return lg.Enabled(ctx, lvl)
}
