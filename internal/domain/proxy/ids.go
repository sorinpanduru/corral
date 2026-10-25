// Package proxy is the domain policy of the wire proxy: connection IDs, the
// handshake reply, the synthesized-error table, the per-message decision
// (FR-61), the request outcome, the client I/O budget and the shutdown state
// machine (SPEC-002; ADR-0015). It imports the standard library and other
// domain packages only, and does no I/O.
package proxy

import (
	"math"
	"sync"
)

// RequestBufferLimit is L, the bytes of a request held before a lease,
// counting the 16-byte header (FR-38).
const RequestBufferLimit = 65536

// ConnIDs allocates connection IDs (FR-3): positive, increasing, wrapping to 1
// after 2,147,483,647, never handing out an ID that is still open.
type ConnIDs struct {
	mu   sync.Mutex
	next int32
	open map[int32]struct{}
}

// NewConnIDs returns an allocator whose first ID is 1.
func NewConnIDs() *ConnIDs { return NewConnIDsAt(1) }

// NewConnIDsAt returns an allocator whose first ID is next (for tests of the
// wrap).
func NewConnIDsAt(next int32) *ConnIDs {
	if next < 1 {
		next = 1
	}
	return &ConnIDs{next: next, open: map[int32]struct{}{}}
}

// Acquire returns a fresh ID, or false if every ID is open.
func (c *ConnIDs) Acquire() (int32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.open) >= math.MaxInt32 {
		return 0, false
	}
	for {
		id := c.next
		if c.next == math.MaxInt32 {
			c.next = 1
		} else {
			c.next++
		}
		if _, used := c.open[id]; !used {
			c.open[id] = struct{}{}
			return id, true
		}
	}
}

// Release frees an ID. Releasing an ID that is not open does nothing.
func (c *ConnIDs) Release(id int32) {
	c.mu.Lock()
	delete(c.open, id)
	c.mu.Unlock()
}
