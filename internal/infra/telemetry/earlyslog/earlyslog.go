// Package earlyslog neutralises slog.Default() during package initialisation
// (SPEC-001 FR-24, FR-28, Amendment 1 thirdparty; ADR-0006).
//
// Some dependencies (go.opentelemetry.io/auto/sdk reads OTEL_* limit variables
// in a package-level var initialiser) call slog.Warn before main runs. Their
// message embeds the raw environment value and the stdlib default handler
// writes it to stderr. This package has only standard-library imports and a
// import path that sorts before every third-party module that logs at init, so
// the Go runtime (dependency order, then import-path order) initialises it
// first; its init replaces the default handler with one that retains nothing
// but a per-level record count. The text, attributes and values of those
// records are dropped at once (GDPR data minimisation: no raw values are
// kept in memory or output).
//
// logbridge.Install later calls Drain and reports each non-zero level once,
// with the fixed third-party message.
package earlyslog

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// Counts is the number of records seen per level bucket before Drain.
type Counts struct{ Debug, Info, Warn, Error uint64 }

var counters [4]atomic.Uint64

type handler struct{}

func (handler) Enabled(context.Context, slog.Level) bool { return true }

func (handler) Handle(_ context.Context, r slog.Record) error {
	switch {
	case r.Level < slog.LevelInfo:
		counters[0].Add(1)
	case r.Level < slog.LevelWarn:
		counters[1].Add(1)
	case r.Level < slog.LevelError:
		counters[2].Add(1)
	default:
		counters[3].Add(1)
	}
	return nil // message and attributes are intentionally dropped
}

func (h handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h handler) WithGroup(string) slog.Handler      { return h }

func init() {
	// slog.SetDefault also redirects the log package into this handler.
	slog.SetDefault(slog.New(handler{}))
}

// Drain returns the counts collected so far and resets them.
func Drain() Counts {
	return Counts{
		Debug: counters[0].Swap(0), Info: counters[1].Swap(0),
		Warn: counters[2].Swap(0), Error: counters[3].Swap(0),
	}
}
