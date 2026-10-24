package fakeclock_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

var start = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// Compile-time: the fake satisfies the domain port.
var _ clock.Clock = (*fakeclock.Clock)(nil)

// Existing behaviour is unchanged: New, Now, Advance.
func TestNowAndAdvance(t *testing.T) {
	c := fakeclock.New(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}
	c.Advance(90 * time.Second)
	if want := start.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("Now() = %v, want %v", c.Now(), want)
	}
	c.Advance(0)
	if want := start.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("Advance(0) moved the clock")
	}
}

// FR-53/AC-29: timers fire in deadline order, ties in registration order, and
// only when due.
func TestFireOrder_AC29(t *testing.T) {
	c := fakeclock.New(start)
	var order []string
	c.AfterFunc(3*time.Second, func() { order = append(order, "c") })
	c.AfterFunc(1*time.Second, func() { order = append(order, "a") })
	c.AfterFunc(2*time.Second, func() { order = append(order, "b1") })
	c.AfterFunc(2*time.Second, func() { order = append(order, "b2") })
	c.AfterFunc(10*time.Second, func() { order = append(order, "late") })
	if c.Pending() != 5 {
		t.Fatalf("Pending() = %d, want 5", c.Pending())
	}
	c.Advance(500 * time.Millisecond)
	if len(order) != 0 {
		t.Fatalf("fired early: %v", order)
	}
	c.Advance(3 * time.Second)
	want := []string{"a", "b1", "b2", "c"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if c.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", c.Pending())
	}
}

// A timer due exactly at the new time fires (deadline <= now).
func TestFiresAtExactDeadline(t *testing.T) {
	c := fakeclock.New(start)
	var n int
	c.AfterFunc(time.Second, func() { n++ })
	c.Advance(time.Second - time.Nanosecond)
	if n != 0 {
		t.Fatal("fired before the deadline")
	}
	c.Advance(time.Nanosecond)
	if n != 1 {
		t.Fatalf("fired %d times at the deadline, want 1", n)
	}
}

// A callback observes Now() equal to its own deadline during a large Advance,
// so deadline-ordered work sees consistent time.
func TestCallbackSeesDeadlineTime(t *testing.T) {
	c := fakeclock.New(start)
	var seen []time.Time
	c.AfterFunc(time.Second, func() { seen = append(seen, c.Now()) })
	c.AfterFunc(5*time.Second, func() { seen = append(seen, c.Now()) })
	c.Advance(time.Minute)
	if len(seen) != 2 {
		t.Fatalf("callbacks = %d, want 2", len(seen))
	}
	if !seen[0].Equal(start.Add(time.Second)) || !seen[1].Equal(start.Add(5*time.Second)) {
		t.Fatalf("callback times = %v", seen)
	}
	if !c.Now().Equal(start.Add(time.Minute)) {
		t.Fatalf("final Now() = %v", c.Now())
	}
}

// Zero and negative durations are due at the next Advance (even Advance(0)).
func TestZeroAndNegativeDuration(t *testing.T) {
	c := fakeclock.New(start)
	var n int
	c.AfterFunc(0, func() { n++ })
	c.AfterFunc(-time.Second, func() { n++ })
	c.Advance(0)
	if n != 2 {
		t.Fatalf("fired %d, want 2", n)
	}
}

// Stop before firing: true, callback never runs, Pending drops; a second Stop
// is false.
func TestStopBeforeFire(t *testing.T) {
	c := fakeclock.New(start)
	var n int
	tm := c.AfterFunc(time.Second, func() { n++ })
	if c.Pending() != 1 {
		t.Fatalf("Pending() = %d", c.Pending())
	}
	if !tm.Stop() {
		t.Fatal("Stop() = false for a pending timer")
	}
	if c.Pending() != 0 {
		t.Fatalf("Pending() = %d after Stop", c.Pending())
	}
	if tm.Stop() {
		t.Fatal("second Stop() = true")
	}
	c.Advance(time.Minute)
	if n != 0 {
		t.Fatal("stopped timer fired")
	}
}

// Stop after firing: false.
func TestStopAfterFire(t *testing.T) {
	c := fakeclock.New(start)
	tm := c.AfterFunc(time.Second, func() {})
	c.Advance(time.Second)
	if tm.Stop() {
		t.Fatal("Stop() = true after firing")
	}
	if c.Pending() != 0 {
		t.Fatalf("Pending() = %d", c.Pending())
	}
}

// A callback may stop a later timer in the same Advance; it then does not fire.
func TestCallbackStopsLaterTimer(t *testing.T) {
	c := fakeclock.New(start)
	var later atomic.Int32
	var tm clock.Timer
	c.AfterFunc(time.Second, func() { tm.Stop() })
	tm = c.AfterFunc(2*time.Second, func() { later.Add(1) })
	c.Advance(time.Minute)
	if later.Load() != 0 {
		t.Fatal("timer stopped by an earlier callback still fired")
	}
}

// A callback that re-arms a timer: the new timer fires in the same Advance if
// due, and is left pending if not.
func TestCallbackRearms_AC29(t *testing.T) {
	c := fakeclock.New(start)
	var ticks []time.Duration
	var arm func()
	arm = func() {
		ticks = append(ticks, c.Now().Sub(start))
		if len(ticks) < 3 {
			c.AfterFunc(time.Second, arm)
		}
	}
	c.AfterFunc(time.Second, arm)
	c.Advance(10 * time.Second)
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(ticks) != 3 {
		t.Fatalf("ticks = %v, want %v", ticks, want)
	}
	for i := range want {
		if ticks[i] != want[i] {
			t.Fatalf("ticks = %v, want %v", ticks, want)
		}
	}
	if c.Pending() != 0 {
		t.Fatalf("Pending() = %d", c.Pending())
	}

	// Not yet due: stays pending.
	c2 := fakeclock.New(start)
	var fired bool
	c2.AfterFunc(time.Second, func() { c2.AfterFunc(time.Hour, func() { fired = true }) })
	c2.Advance(2 * time.Second)
	if fired || c2.Pending() != 1 {
		t.Fatalf("fired=%v pending=%d, want false and 1", fired, c2.Pending())
	}
}

// Callbacks run outside the lock: they can call Now, AfterFunc, Pending,
// Stop and WaitForTimers without deadlocking.
func TestCallbacksRunOutsideLock(t *testing.T) {
	c := fakeclock.New(start)
	done := make(chan struct{})
	c.AfterFunc(time.Second, func() {
		_ = c.Now()
		_ = c.Pending()
		c.AfterFunc(time.Hour, func() {}).Stop()
		c.WaitForTimers(0, time.Millisecond)
		close(done)
	})
	go c.Advance(time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("callback deadlocked calling back into the clock")
	}
}

// WaitForTimers returns true at once when enough timers are pending.
func TestWaitForTimersImmediate(t *testing.T) {
	c := fakeclock.New(start)
	c.AfterFunc(time.Hour, func() {})
	c.AfterFunc(time.Hour, func() {})
	if !c.WaitForTimers(2, time.Second) {
		t.Fatal("WaitForTimers(2) = false with 2 pending")
	}
	if !c.WaitForTimers(1, time.Second) {
		t.Fatal("WaitForTimers(1) = false with 2 pending (at least n)")
	}
	if !c.WaitForTimers(0, 0) {
		t.Fatal("WaitForTimers(0) = false")
	}
}

// WaitForTimers times out with false when not enough timers arrive.
func TestWaitForTimersTimeout(t *testing.T) {
	c := fakeclock.New(start)
	c.AfterFunc(time.Hour, func() {})
	begin := time.Now()
	if c.WaitForTimers(2, 50*time.Millisecond) {
		t.Fatal("WaitForTimers(2) = true with 1 pending")
	}
	if time.Since(begin) < 40*time.Millisecond {
		t.Fatal("WaitForTimers returned before its timeout")
	}
}

// WaitForTimers wakes when another goroutine arms a timer.
func TestWaitForTimersWakes(t *testing.T) {
	c := fakeclock.New(start)
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.AfterFunc(time.Hour, func() {})
	}()
	if !c.WaitForTimers(1, 5*time.Second) {
		t.Fatal("WaitForTimers did not see a timer armed by another goroutine")
	}
}

// Fake time does not move with the wall clock.
func TestNowDoesNotDrift(t *testing.T) {
	c := fakeclock.New(start)
	time.Sleep(10 * time.Millisecond)
	if !c.Now().Equal(start) {
		t.Fatal("fake clock drifted with the wall clock")
	}
}

// Concurrent AfterFunc, Stop, Advance and Pending, run under -race. Every
// timer that is not stopped fires exactly once.
func TestConcurrent_Race_AC29(t *testing.T) {
	c := fakeclock.New(start)
	const workers, per = 8, 200
	var fired, stopped atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				tm := c.AfterFunc(time.Duration(i%7)*time.Millisecond, func() { fired.Add(1) })
				if i%5 == 0 && tm.Stop() {
					stopped.Add(1)
				}
				_ = c.Pending()
				_ = c.Now()
			}
		}()
	}
	stop := make(chan struct{})
	adv := make(chan struct{})
	go func() {
		defer close(adv)
		for {
			select {
			case <-stop:
				return
			default:
				c.Advance(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-adv
	c.Advance(time.Hour)
	if got, want := fired.Load()+stopped.Load(), int64(workers*per); got != want {
		t.Fatalf("fired+stopped = %d, want %d", got, want)
	}
	if c.Pending() != 0 {
		t.Fatalf("Pending() = %d", c.Pending())
	}
}
