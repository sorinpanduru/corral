package proxy

import (
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
)

// Action is what an open connection does at FR-47 step 3.
type Action int

const (
	// ActionCloseNow: no exchange in progress, close at once.
	ActionCloseNow Action = iota
	// ActionFinishThenClose: let the exchange run to completion, then close.
	ActionFinishThenClose
	// ActionRefuseThenClose: the first byte arrived after step 3: answer 91,
	// then close.
	ActionRefuseThenClose
)

// Shutdown is the FR-47 state machine: the listener-close delay, the phase
// after the listener closes, the drain timer, and the second-signal force
// (FR-63). It is safe for concurrent use.
type Shutdown struct {
	delay, drain time.Duration
	clk          clock.Clock

	mu         sync.Mutex
	begun      bool
	forced     bool
	phase      Phase
	delayCh    chan struct{}
	drainCh    chan struct{}
	delayDone  bool
	drainDone  bool
	delayTimer clock.Timer
	drainTimer clock.Timer
}

// NewShutdown returns a state machine in the serving phase.
func NewShutdown(delay, drain time.Duration, clk clock.Clock) *Shutdown {
	return &Shutdown{delay: delay, drain: drain, clk: clk,
		delayCh: make(chan struct{}), drainCh: make(chan struct{})}
}

func (s *Shutdown) closeDelay() {
	if !s.delayDone {
		s.delayDone = true
		close(s.delayCh)
	}
}

func (s *Shutdown) closeDrain() {
	if !s.drainDone {
		s.drainDone = true
		close(s.drainCh)
	}
}

// Begin starts the listener-close delay. Service continues normally during it.
func (s *Shutdown) Begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.begun {
		return
	}
	s.begun = true
	if s.delay <= 0 || s.forced {
		s.closeDelay()
		return
	}
	s.delayTimer = s.clk.AfterFunc(s.delay, func() {
		s.mu.Lock()
		s.closeDelay()
		s.mu.Unlock()
	})
}

// ListenerClosed records FR-47 step 2: the phase becomes Late and the drain
// timer starts.
func (s *Shutdown) ListenerClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = PhaseLate
	if s.forced || s.drain <= 0 {
		s.closeDrain()
		return
	}
	if s.drainTimer == nil && !s.drainDone {
		s.drainTimer = s.clk.AfterFunc(s.drain, func() {
			s.mu.Lock()
			s.closeDrain()
			s.mu.Unlock()
		})
	}
}

// Force is the second signal: it skips the rest of the delay and ends the
// drain at once. It is idempotent, and does nothing before Begin.
func (s *Shutdown) Force() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.begun {
		return
	}
	s.forced = true
	s.closeDelay()
	s.closeDrain()
	if s.delayTimer != nil {
		s.delayTimer.Stop()
	}
	if s.drainTimer != nil {
		s.drainTimer.Stop()
	}
}

// Phase returns the current phase.
func (s *Shutdown) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// DelayExpired is closed when the listener-close delay is over (or forced).
func (s *Shutdown) DelayExpired() <-chan struct{} { return s.delayCh }

// DrainExpired is closed when the drain is over (or forced).
func (s *Shutdown) DrainExpired() <-chan struct{} { return s.drainCh }

// ConnAction is the FR-47 step 3 decision for one connection. Precedence: an
// exchange in progress always finishes first (finish-then-close), even if a
// later byte has already arrived; only an idle connection whose first byte
// came after step 3 is refused with 91; any other idle connection closes at
// once.
func (s *Shutdown) ConnAction(exchangeInProgress, firstByteReadAfterStep3 bool) Action {
	switch {
	case exchangeInProgress:
		return ActionFinishThenClose
	case firstByteReadAfterStep3:
		return ActionRefuseThenClose
	}
	return ActionCloseNow
}
