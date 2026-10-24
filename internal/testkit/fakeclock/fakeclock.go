// Package fakeclock is a manually advanced clock for tests (ADR-0005).
// It must not be imported by production packages.
package fakeclock

import (
	"sync"
	"time"
)

// Clock is a fake clock safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// New returns a clock set to start.
func New(start time.Time) *Clock { return &Clock{now: start} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
