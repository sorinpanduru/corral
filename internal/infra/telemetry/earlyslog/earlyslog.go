// Package earlyslog neutralises slog.Default() and the std log package during
// package initialisation (SPEC-001 FR-24, FR-28, Amendment 1 thirdparty;
// ADR-0008).
//
// Some dependencies (go.opentelemetry.io/auto/sdk reads OTEL_* limit variables
// in a package-level var initialiser) call slog.Warn before main runs. Their
// message embeds the raw environment value and the stdlib default handler
// writes it to stderr. This package has only standard-library imports and an
// import path that sorts before every third-party module that logs at init, so
// the Go runtime (dependency order, then import-path order) initialises it
// first; its init replaces the default slog handler and the log package output
// with sinks that retain nothing but a per-source, per-level record count. The
// text, attributes and values of those records are dropped at once (GDPR data
// minimisation: no raw values are kept in memory or output).
//
// logbridge.Install calls Handoff, which forwards later records of handlers
// captured during init to the real handler and returns the counts so Install
// can replay them through the rate-limited, fixed-message bridges.
package earlyslog

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Counts is the number of records seen per source and level bucket before
// Handoff or Drain. StdLog counts writes by the std log package.
type Counts struct{ Debug, Info, Warn, Error, StdLog uint64 }

var (
	counters [5]atomic.Uint64

	// mu orders counting against Handoff/Drain so no record is lost between
	// the target switch and the counter reset.
	mu     sync.RWMutex
	target slog.Handler // guarded by mu; nil while counting
)

type handler struct{}

func (handler) Enabled(context.Context, slog.Level) bool { return true }

func (handler) Handle(ctx context.Context, r slog.Record) error {
	mu.RLock()
	t := target
	if t == nil {
		defer mu.RUnlock()
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
	mu.RUnlock()
	return t.Handle(ctx, r)
}

func (h handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h handler) WithGroup(string) slog.Handler      { return h }

type stdWriter struct{}

func (stdWriter) Write(p []byte) (int, error) {
	counters[4].Add(1)
	return len(p), nil // bytes are intentionally dropped
}

func init() {
	slog.SetDefault(slog.New(handler{})) // also points log at the handler...
	log.SetOutput(stdWriter{})           // ...so give log its own source count
}

func take() Counts {
	return Counts{
		Debug: counters[0].Swap(0), Info: counters[1].Swap(0),
		Warn: counters[2].Swap(0), Error: counters[3].Swap(0),
		StdLog: counters[4].Swap(0),
	}
}

// Handoff makes records that still reach the early handler (for example a
// *slog.Logger captured during init) flow to h, and returns and resets the
// counts collected so far. h must be a fixed-message bridge handler that never
// forwards record text. Handing off to the early handler itself would recurse
// forever, so that case is a no-op that only returns the counts.
func Handoff(h slog.Handler) Counts {
	mu.Lock()
	defer mu.Unlock()
	if _, self := h.(handler); !self {
		target = h
	}
	return take()
}

// Drain returns and resets the counts and returns the early handler to
// counting mode. It is a test-only reset: it clears the forward target, so it
// must not be called after Install in production.
func Drain() Counts {
	mu.Lock()
	defer mu.Unlock()
	target = nil
	return take()
}
