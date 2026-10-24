// Package ratelimit implements the FR-33 window limiter: at most one event is
// emitted per window, and each emitted event reports how many events were
// dropped since the previous emission.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is safe for concurrent use.
type Limiter struct {
	window time.Duration
	now    func() time.Time

	mu         sync.Mutex
	started    bool
	last       time.Time
	suppressed int
}

// New returns a limiter allowing one event per window. now is the clock; a nil
// now uses time.Now.
func New(window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{window: window, now: now}
}

// Allow reports whether the event should be emitted. When emit is true,
// suppressed is the number of events dropped since the last emitted one. The
// window is anchored at the last emitted event.
func (l *Limiter) Allow() (emit bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if l.started && t.Sub(l.last) < l.window {
		l.suppressed++
		return false, 0
	}
	suppressed = l.suppressed
	l.started = true
	l.last = t
	l.suppressed = 0
	return true, suppressed
}
