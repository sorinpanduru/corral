package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

const window = 30 * time.Second

var start = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newLimiter() (*ratelimit.Limiter, *fakeclock.Clock) {
	c := fakeclock.New(start)
	return ratelimit.New(window, c.Now), c
}

// FR-33, AC-29: the first event is emitted with suppressed 0.
func TestFirstEventEmittedWithZeroSuppressed(t *testing.T) {
	l, _ := newLimiter()
	emit, sup := l.Allow()
	if !emit || sup != 0 {
		t.Fatalf("first Allow = (%v,%d), want (true,0)", emit, sup)
	}
}

// FR-33, AC-26, AC-29: 10 events inside the window give one emission, the
// next one after the window reports 9 suppressed.
func TestTenEventsOneEmittedThenSuppressedNine(t *testing.T) {
	l, c := newLimiter()
	emitted := 0
	for i := 0; i < 10; i++ {
		if e, _ := l.Allow(); e {
			emitted++
		}
		c.Advance(time.Second) // 10 events within 10s
	}
	if emitted != 1 {
		t.Fatalf("emitted %d of 10 within the window, want 1", emitted)
	}
	c.Advance(window)
	emit, sup := l.Allow()
	if !emit || sup != 9 {
		t.Fatalf("Allow after window = (%v,%d), want (true,9)", emit, sup)
	}
}

// FR-33: boundaries of the window.
func TestWindowBoundary(t *testing.T) {
	l, c := newLimiter()
	l.Allow()
	c.Advance(window - time.Nanosecond)
	if e, _ := l.Allow(); e {
		t.Fatal("emitted just before the window elapsed")
	}
	c.Advance(time.Nanosecond) // exactly window since the emitted event
	emit, sup := l.Allow()
	if !emit || sup != 1 {
		t.Fatalf("Allow at window boundary = (%v,%d), want (true,1)", emit, sup)
	}
}

// FR-33: the window starts at the last emitted event, not at suppressed ones,
// and the suppressed count resets after an emission.
func TestWindowAnchoredAtEmissionAndCountResets(t *testing.T) {
	l, c := newLimiter()
	l.Allow()                   // t0 emitted
	c.Advance(10 * time.Second) // t0+10
	l.Allow()                   // suppressed
	c.Advance(20 * time.Second) // t0+30
	emit, sup := l.Allow()
	if !emit || sup != 1 {
		t.Fatalf("at t0+30 = (%v,%d), want (true,1)", emit, sup)
	}
	c.Advance(29 * time.Second) // t0+59
	if e, _ := l.Allow(); e {
		t.Fatal("emitted at t0+59, window anchored at t0+30")
	}
	c.Advance(time.Second) // t0+60
	emit, sup = l.Allow()
	if !emit || sup != 1 {
		t.Fatalf("at t0+60 = (%v,%d), want (true,1)", emit, sup)
	}
	c.Advance(window)
	emit, sup = l.Allow()
	if !emit || sup != 0 {
		t.Fatalf("after quiet window = (%v,%d), want (true,0)", emit, sup)
	}
}

// FR-28/D7: limiters are independent instances.
func TestLimitersAreIndependent(t *testing.T) {
	c := fakeclock.New(start)
	a, b := ratelimit.New(window, c.Now), ratelimit.New(window, c.Now)
	a.Allow()
	for i := 0; i < 3; i++ {
		a.Allow()
	}
	emit, sup := b.Allow()
	if !emit || sup != 0 {
		t.Fatalf("second limiter = (%v,%d), want (true,0)", emit, sup)
	}
}

// Safe for concurrent use (make test runs with -race): exactly one emission
// per window and every other event is counted.
func TestConcurrentAllow(t *testing.T) {
	l, c := newLimiter()
	const n = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	emitted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e, _ := l.Allow(); e {
				mu.Lock()
				emitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if emitted != 1 {
		t.Fatalf("emitted %d, want 1", emitted)
	}
	c.Advance(window)
	emit, sup := l.Allow()
	if !emit || sup != n-1 {
		t.Fatalf("after window = (%v,%d), want (true,%d)", emit, sup, n-1)
	}
}
