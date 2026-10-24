// Package sighup implements the SPEC-003 FR-18 SIGHUP behaviour: signals are
// drained from process start, ignored until a logger exists, then reported as
// rate-limited warn records. Configuration is never reloaded.
//
// This package does not register for signals; the entry point owns
// signal.Notify (ADR-0013).
package sighup

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
)

const window = 30 * time.Second

// Reporter emits at most one warn record per 30 seconds; a record that follows
// dropped signals carries suppressed_count (an int greater than zero). It is
// safe for concurrent use. It adds no component: the caller binds it.
type Reporter struct {
	l   *slog.Logger
	lim *ratelimit.Limiter
}

// NewReporter returns a Reporter writing to l. A nil now uses time.Now.
func NewReporter(l *slog.Logger, now func() time.Time) *Reporter {
	return &Reporter{l: l, lim: ratelimit.New(window, now)}
}

// Signal reports one SIGHUP, synchronously.
func (r *Reporter) Signal() {
	emit, suppressed := r.lim.Allow()
	if !emit {
		return
	}
	var attrs []slog.Attr
	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed_count", suppressed))
	}
	r.l.LogAttrs(context.Background(), slog.LevelWarn, config.MsgReloadUnsupported, attrs...)
}

// Watcher drains a signal channel in a goroutine.
type Watcher struct {
	now      func() time.Time
	reporter atomic.Pointer[Reporter]
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

// Start begins draining ch. Signals are dropped (not counted, not replayed)
// until SetLogger is called. A nil now uses time.Now.
func Start(ch <-chan os.Signal, now func() time.Time) *Watcher {
	w := &Watcher{now: now, stop: make(chan struct{}), done: make(chan struct{})}
	go w.run(ch)
	return w
}

func (w *Watcher) run(ch <-chan os.Signal) {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			if r := w.reporter.Load(); r != nil {
				r.Signal()
			}
		}
	}
}

// SetLogger installs a fresh Reporter (new rate-limit state) on l. The latest
// logger wins.
func (w *Watcher) SetLogger(l *slog.Logger) {
	w.reporter.Store(NewReporter(l, w.now))
}

// Stop ends the goroutine and waits for it. It is idempotent.
func (w *Watcher) Stop() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}
