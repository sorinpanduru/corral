package faultproxy

import (
	"strings"
	"sync"
	"time"
)

type faultKind int

const (
	fkResetAfterRequest faultKind = iota + 1
	fkDelay
	fkDelayReply
)

// Fault is what happens to a matching request.
type Fault struct {
	kind faultKind
	d    time.Duration
}

// ResetAfterRequest forwards the request to the upstream, then closes both
// sides (the client side with a TCP reset) before the reply.
func ResetAfterRequest() Fault { return Fault{kind: fkResetAfterRequest} }

// Delay holds the request for d before forwarding it.
func Delay(d time.Duration) Fault { return Fault{kind: fkDelay, d: d} }

// DelayReply forwards the request at once and holds its reply for d.
func DelayReply(d time.Duration) Fault { return Fault{kind: fkDelayReply, d: d} }

// Match selects requests. Zero fields match everything.
type Match struct {
	Command string // case-insensitive
	AppName string
	// OnlyConn restricts the fault to connection ConnIndex.
	OnlyConn  bool
	ConnIndex int
	// Nth, when > 0, applies the fault only to the Nth matching request
	// (1-based, counted across connections).
	Nth int
	// Once consumes the fault after its first application. A one-shot reset is
	// Match{Command: "insert", Once: true}.
	Once bool
}

// FaultHandle observes and removes an installed fault.
type FaultHandle struct {
	mu      sync.Mutex
	m       Match
	f       Fault
	seen    int
	hits    int
	used    bool
	removed bool
}

// Hits is how many requests the fault has been applied to.
func (h *FaultHandle) Hits() int { h.mu.Lock(); defer h.mu.Unlock(); return h.hits }

// Remove uninstalls the fault.
func (h *FaultHandle) Remove() { h.mu.Lock(); h.removed = true; h.mu.Unlock() }

// WaitHits waits until the fault was applied at least n times.
func (h *FaultHandle) WaitHits(n int, timeout time.Duration) bool {
	return poll(timeout, func() bool { return h.Hits() >= n })
}

// AddFault installs a fault; the first matching, unconsumed fault applies.
func (p *Proxy) AddFault(m Match, f Fault) *FaultHandle {
	h := &FaultHandle{m: m, f: f}
	p.mu.Lock()
	p.faults = append(p.faults, h)
	p.mu.Unlock()
	return h
}

// ClearFaults removes every installed fault.
func (p *Proxy) ClearFaults() {
	p.mu.Lock()
	fs := p.faults
	p.faults = nil
	p.mu.Unlock()
	for _, h := range fs {
		h.Remove()
	}
}

func (p *Proxy) matchFault(cmd, app string, conn int) *Fault {
	p.mu.Lock()
	fs := append([]*FaultHandle(nil), p.faults...)
	p.mu.Unlock()
	var picked *Fault
	for _, h := range fs {
		m := h.m
		if m.Command != "" && !strings.EqualFold(m.Command, cmd) {
			continue
		}
		if m.AppName != "" && m.AppName != app {
			continue
		}
		if m.OnlyConn && m.ConnIndex != conn {
			continue
		}
		h.mu.Lock()
		if h.removed || (m.Once && h.used) {
			h.mu.Unlock()
			continue
		}
		h.seen++
		if (m.Nth > 0 && h.seen != m.Nth) || picked != nil {
			// Not applied, but the request still counted towards Nth.
			h.mu.Unlock()
			continue
		}
		h.used = true
		h.hits++
		f := h.f
		h.mu.Unlock()
		picked = &f
	}
	return picked
}
