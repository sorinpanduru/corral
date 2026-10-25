package proxy

import (
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
)

// IOBudget is the FR-57 client I/O time budget for one message: only time
// spent reading from the client is charged. Waiting for a lease, for the
// queue or for the buffer to drain is not.
type IOBudget struct {
	mu      sync.Mutex
	limit   time.Duration
	clk     clock.Clock
	used    time.Duration
	reading bool
	start   time.Time
}

// NewIOBudget returns a budget of limit per message.
func NewIOBudget(limit time.Duration, clk clock.Clock) *IOBudget {
	return &IOBudget{limit: limit, clk: clk}
}

func (b *IOBudget) remaining() time.Duration {
	used := b.used
	if b.reading {
		used += b.clk.Now().Sub(b.start)
	}
	if r := b.limit - used; r > 0 {
		return r
	}
	return 0
}

// BeginRead starts charging and returns the deadline for the read.
func (b *IOBudget) BeginRead() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reading {
		b.used += b.clk.Now().Sub(b.start)
	}
	b.reading, b.start = true, b.clk.Now()
	return b.start.Add(b.remaining())
}

// EndRead stops charging. Without a matching BeginRead it does nothing.
func (b *IOBudget) EndRead() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reading {
		b.used += b.clk.Now().Sub(b.start)
		b.reading = false
	}
}

// Remaining is the budget left, never negative.
func (b *IOBudget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining()
}

// Reset starts a new message with a full budget.
func (b *IOBudget) Reset() {
	b.mu.Lock()
	b.used, b.reading = 0, false
	b.mu.Unlock()
}
