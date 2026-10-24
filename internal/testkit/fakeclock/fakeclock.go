// Package fakeclock is a manually advanced clock for tests (ADR-0005).
// It satisfies clock.Clock. It must not be imported by production packages.
package fakeclock

import (
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
)

type timer struct {
	c        *Clock
	deadline time.Time
	seq      uint64
	f        func()
	active   bool
}

// Stop implements clock.Timer.
func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	delete(t.c.timers, t)
	return true
}

// Clock is a fake clock safe for concurrent use.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers map[*timer]struct{}
}

var _ clock.Clock = (*Clock)(nil)

// New returns a clock set to start.
func New(start time.Time) *Clock {
	return &Clock{now: start, timers: map[*timer]struct{}{}}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc registers f to run during the Advance that makes d elapse. A
// zero or negative d is due at the next Advance (even Advance(0)).
func (c *Clock) AfterFunc(d time.Duration, f func()) clock.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &timer{c: c, deadline: c.now.Add(d), seq: c.seq, f: f, active: true}
	c.timers[t] = struct{}{}
	return t
}

// Pending returns the number of armed timers.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// WaitForTimers waits up to timeout (real time) until at least n timers are
// pending and reports whether that happened.
func (c *Clock) WaitForTimers(n int, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for {
		if c.Pending() >= n {
			return true
		}
		if !time.Now().Before(end) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// Advance moves the clock forward by d, firing due timers in deadline order
// (ties in registration order) outside the lock. A callback sees Now equal to
// its own deadline, and timers it arms fire in the same call when due.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *timer
		for t := range c.timers {
			if t.deadline.After(target) {
				continue
			}
			if next == nil || t.deadline.Before(next.deadline) ||
				(t.deadline.Equal(next.deadline) && t.seq < next.seq) {
				next = t
			}
		}
		if next == nil {
			if target.After(c.now) {
				c.now = target
			}
			c.mu.Unlock()
			return
		}
		next.active = false
		delete(c.timers, next)
		if next.deadline.After(c.now) {
			c.now = next.deadline
		}
		f := next.f
		c.mu.Unlock()
		f()
	}
}
