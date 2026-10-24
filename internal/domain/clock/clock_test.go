package clock_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
)

// SPEC-002 FR-53 (testability): System() satisfies Clock and tracks time.Now.
func TestSystemNow_FR53(t *testing.T) {
	var c clock.Clock = clock.System()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("System().Now() is far from time.Now()")
	}
}

// System().AfterFunc fires a short timer.
func TestSystemAfterFuncFires_FR53(t *testing.T) {
	c := clock.System()
	done := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("1 ms timer did not fire within 5 s")
	}
}

// Stop before firing returns true and prevents the callback.
func TestSystemStopBeforeFire_FR53(t *testing.T) {
	c := clock.System()
	var fired atomic.Bool
	tm := c.AfterFunc(time.Hour, func() { fired.Store(true) })
	if !tm.Stop() {
		t.Fatal("Stop() = false for a pending timer")
	}
	if tm.Stop() {
		t.Fatal("second Stop() = true")
	}
	if fired.Load() {
		t.Fatal("callback ran after Stop")
	}
}

// Stop after firing returns false.
func TestSystemStopAfterFire_FR53(t *testing.T) {
	c := clock.System()
	done := make(chan struct{})
	tm := c.AfterFunc(time.Millisecond, func() { close(done) })
	<-done
	if tm.Stop() {
		t.Fatal("Stop() = true after the timer fired")
	}
}
