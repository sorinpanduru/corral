// Package clock is the domain time port (ADR-0015). Domain code takes a Clock
// instead of calling the time package's clock functions, so tests can drive
// time with internal/testkit/fakeclock.
//
// It imports the standard library only (SPEC-001 FR-1).
package clock

import "time"

// Timer is a pending one-shot callback.
type Timer interface {
	// Stop prevents the callback from running. It reports whether the call
	// stopped the timer (false if it already fired or was stopped).
	Stop() bool
}

// Clock supplies the current time and one-shot timers.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f in its own goroutine after d has elapsed.
	AfterFunc(d time.Duration, f func()) Timer
}

type system struct{}

func (system) Now() time.Time { return time.Now() }

func (system) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// System returns the Clock backed by time.Now and time.AfterFunc.
func System() Clock { return system{} }
