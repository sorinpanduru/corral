package upstream_test

// Wave 4 review findings (B1: Amendment 3 in the monitor; AC-56, AC-57).

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// AC-56: a single-seed replica-set member that restarts and answers as a ghost
// (or with no setName at all) after a match: requests wait, readiness is
// upstream_unavailable, and the upstream is never incompatible.
func TestSingleSeedRestartsAsGhost_AC56(t *testing.T) {
	for name, odd := range map[string]upstream.Report{
		"ghost (isreplicaset)":        ghost(A),
		"no setName, no isreplicaset": {Host: A, IsWritablePrimary: true, Params: params(0, 21)},
		"standalone-looking":          standalone(A),
		"mongos-looking":              mongos(A),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(primary(A, "rs0", A)) }, false).start()
			h.waitState("target", func(s upstream.State) bool { return s.Target == A })
			h.quiet()
			h.chk.set(odd)
			h.stepUntilState("target lost", func(s upstream.State) bool { return !s.HasTarget })
			for i := 0; i < 4; i++ {
				h.clk.WaitForTimers(1, time.Second)
				h.clk.Advance(time.Second)
				time.Sleep(2 * time.Millisecond)
			}
			s := h.m.State()
			if s.Incompatible || !s.Reached {
				t.Fatalf("state: %+v", s)
			}
			if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
				t.Fatalf("readiness: %+v", st)
			}
			if a := h.m.Admission(); a.Kind != upstream.Wait {
				t.Fatalf("admission: %+v", a)
			}
			noIncompatRecord(t, h)
			// The member returns to the set: usable again.
			h.chk.set(primaryAt(A, "rs0", 2, 1, A))
			h.stepUntilState("usable again", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
		})
	}
}

// AC-56 (removal rule): a server that a matching member no longer lists, and
// that is not a seed, leaves the known set: it is not checked again and its
// monitoring connection is closed through CloseHost(host, idle). Seeds are never
// removed.
func TestRemovedMembersLeaveKnownSet_AC56(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B, C))
		c.set(secondary(B, "rs0", A, A, B, C))
		c.set(secondary(C, "rs0", A, A, B, C))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(C, 1)
	h.quiet()
	// A's list drops C (and the others agree).
	h.chk.set(primaryAt(A, "rs0", 1, 2, A, B))
	h.chk.set(secondary(B, "rs0", A, A, B))
	eventually(t, "C removed and its connection closed", func() bool {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(500 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
		for _, c := range h.chk.closeCalls() {
			if c.Host == C {
				return true
			}
		}
		return false
	})
	for _, c := range h.chk.closeCalls() {
		if c.Host == C && c.Reason != "idle" {
			t.Fatalf("CloseHost(%s) with reason %q, want idle", C, c.Reason)
		}
		if c.Host == A {
			t.Fatal("a seed was removed")
		}
	}
	h.quiet()
	nC := len(h.chk.of(C))
	for i := 0; i < 30; i++ {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if got := len(h.chk.of(C)); got != nC {
		t.Fatalf("a removed member is still checked (%d -> %d)", nC, got)
	}
	// A foreign server is kept while a matching member lists it, removed when
	// it is no longer listed.
	h2 := newHarness(t, rsCfg(A), func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, D))
		c.set(primaryAt(D, "other", 9, 9, D))
	}, false).start()
	h2.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h2.waitCount(D, 1)
	n := len(h2.chk.of(D))
	h2.stepUntil(D, n+1) // kept: re-checked while listed
	if len(h2.chk.closeCalls()) != 0 {
		t.Fatalf("a listed foreign member was removed: %+v", h2.chk.closeCalls())
	}
	h2.chk.set(primaryAt(A, "rs0", 1, 2, A))
	eventually(t, "D removed", func() bool {
		h2.clk.WaitForTimers(1, time.Second)
		h2.clk.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
		for _, c := range h2.chk.closeCalls() {
			if c.Host == D && c.Reason == "idle" {
				return true
			}
		}
		return false
	})
	// A foreign seed is never removed.
	h3 := newHarness(t, rsCfg(A, B), func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A))
		c.set(primaryAt(B, "other", 9, 9, B))
	}, false).start()
	h3.waitState("target", func(s upstream.State) bool { return s.Target == A })
	for i := 0; i < 5; i++ {
		h3.clk.WaitForTimers(1, time.Second)
		h3.clk.Advance(5 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	for _, c := range h3.chk.closeCalls() {
		if c.Host == B {
			t.Fatal("a seed was removed from the known set")
		}
	}
}

// AC-57: an immediate authenticating check follows every target change, even
// while authentication is failing (the reconnect backoff must not delay it).
func TestAuthCheckOnEveryTargetChangeWhileFailing_AC57(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	bad := func(r upstream.Report) upstream.Report {
		r.AuthErr = &upstream.AuthFailed{Err: fakeServerErr{code: 18}}
		return r
	}
	setup := func(c *fakeChecker) {
		c.set(bad(primaryAt(A, "rs0", 1, 1, A, B)))
		c.set(secondary(B, "rs0", A, A, B))
	}
	h := newHarness(t, cfg, setup, false).start()
	h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
	// Let consecutive failures build up the backoff.
	for i := 0; i < 4; i++ {
		h.stepUntil(A, len(h.chk.of(A))+1)
	}
	// The primary moves to B (authentication still failing).
	h.chk.set(secondary(A, "rs0", B, A, B))
	h.chk.set(bad(primaryAt(B, "rs0", 2, 1, A, B)))
	h.stepUntilState("B is the target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
	changeAt := h.clk.Now()
	nB := len(h.chk.of(B))
	// Within the 500 ms floor B is checked, authenticating, on a fresh connection.
	h.clk.WaitForTimers(1, time.Second)
	h.clk.Advance(500 * time.Millisecond)
	eventually(t, "an authenticating check of the new target", func() bool {
		cs := h.chk.of(B)
		for _, c := range cs[nB:] {
			if c.Req.Authenticate && c.Req.Fresh && !c.At.Before(changeAt) {
				return true
			}
		}
		return false
	})
	if d := h.clk.Now().Sub(changeAt); d > time.Second {
		t.Fatalf("the check took %v of fake time after the change", d)
	}
}
