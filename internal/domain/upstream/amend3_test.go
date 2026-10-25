package upstream_test

// SPEC-002 Amendment 3, AC-56 (unit part): member states in replica-set mode.
// Interface assumption (stated in the report): a ghost (a member answering
// `isreplicaset: true` with no setName) is reported through
// upstream.Report.IsReplicaSet.

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

func ghost(host string) upstream.Report {
	return upstream.Report{Host: host, IsReplicaSet: true, Params: params(0, 21)}
}

func noIncompatRecord(t *testing.T, h *harness) {
	t.Helper()
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if len(h.rec.incompat) != 0 {
		t.Fatalf("an upstream incompatible record was emitted: %v", h.rec.incompat)
	}
	for _, c := range h.changeKinds() {
		if c.Kind == upstream.BecameIncompatible {
			t.Fatal("BecameIncompatible reported")
		}
	}
}

// AC-56 ghost: B answers isreplicaset:true with no setName for several checks.
// It stays known and is re-checked (fast interval included), the upstream is
// not incompatible, and when B reports the set and then primary it becomes the
// target.
func TestGhostMemberStaysKnown_AC56(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B, C))
		c.set(ghost(B))
		c.set(secondary(C, "rs0", A, A, B, C))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(B, 1)
	nB := len(h.chk.of(B))
	for i := 0; i < 3; i++ {
		h.stepUntil(B, nB+i+1)
	}
	if s := h.m.State(); s.Incompatible || s.Target != A {
		t.Fatalf("state: %+v", s)
	}
	noIncompatRecord(t, h)
	// No usable target: the ghost is checked on the 500 ms fast interval.
	h.chk.down(A)
	h.chk.set(secondary(C, "rs0", "", A, B, C))
	h.stepUntilState("target lost", func(s upstream.State) bool { return !s.HasTarget })
	h.quiet()
	n0 := len(h.chk.of(B))
	for i := 1; i <= 4; i++ {
		h.stepUntil(B, n0+i)
	}
	cs := h.chk.of(B)
	for i := n0 + 1; i < len(cs); i++ {
		if d := cs[i].At.Sub(cs[i-1].At); d > 2*time.Second {
			t.Fatalf("ghost re-checked after %v during the fast interval", d)
		}
	}
	noIncompatRecord(t, h)
	// The ghost joins: reports the set, then primary with a larger pair.
	h.chk.set(secondary(B, "rs0", "", A, B, C))
	h.stepUntil(B, len(h.chk.of(B))+1)
	h.chk.set(primaryAt(B, "rs0", 3, 1, A, B, C))
	h.stepUntilState("B is the target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
}

// AC-56 standalone (and mongos) after a match: C stays known and is re-checked,
// is never the target, adds no discovery, and the upstream stays compatible.
func TestStandaloneOrMongosAfterMatch_AC56(t *testing.T) {
	for name, odd := range map[string]func() upstream.Report{
		"standalone": func() upstream.Report {
			r := standalone(C)
			r.Hosts = []string{"zzz.test:1"} // must not be used for discovery
			r.Primary = C
			return r
		},
		"mongos": func() upstream.Report {
			r := mongos(C)
			r.Hosts = []string{"zzz.test:1"}
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup := func(c *fakeChecker) {
				c.set(primaryAt(A, "rs0", 1, 1, A, B, C))
				c.set(secondary(B, "rs0", A, A, B, C))
				c.set(secondary(C, "rs0", A, A, B, C))
			}
			h := newHarness(t, rsCfg(A), setup, false).start()
			h.waitState("target", func(s upstream.State) bool { return s.Target == A })
			h.waitCount(C, 1)
			h.quiet()
			h.chk.set(odd())
			n := len(h.chk.of(C))
			for i := 0; i < 3; i++ {
				h.stepUntil(C, n+i+1)
			}
			if s := h.m.State(); s.Incompatible || s.Target != A {
				t.Fatalf("state: %+v", s)
			}
			if n := len(h.chk.of("zzz.test:1")); n != 0 {
				t.Fatalf("a non-matching member's hosts were used for discovery (%d checks)", n)
			}
			noIncompatRecord(t, h)
			if st := h.readiness(); !st.Ready {
				t.Fatalf("readiness: %+v", st)
			}
			// Even claiming primary, it is never the target.
			r := odd()
			r.IsWritablePrimary = true
			h.chk.set(r)
			h.stepUntil(C, len(h.chk.of(C))+1)
			if h.m.State().Target != A {
				t.Fatal("a non-matching member became the target")
			}
		})
	}
}

// AC-56 unreachable: C stops answering, stays known and is re-checked, and is
// used again as soon as a check succeeds with the matching setName.
func TestUnreachableMemberReused_AC56(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B, C))
		c.set(secondary(B, "rs0", A, A, B, C))
		c.set(secondary(C, "rs0", A, A, B, C))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(C, 1)
	h.quiet()
	h.chk.down(C)
	n := len(h.chk.of(C))
	for i := 0; i < 3; i++ {
		h.stepUntil(C, n+i+1)
	}
	h.chk.set(primaryAt(C, "rs0", 6, 1, A, B, C))
	h.chk.set(secondary(A, "rs0", C, A, B, C))
	h.stepUntilState("C used as the target", func(s upstream.State) bool { return s.HasTarget && s.Target == C })
}

// AC-56 foreign: B reports a different setName: never the target, its hosts are
// not used for discovery, it is re-checked every heartbeat even while the fast
// interval applies to the others, it is re-admitted when it matches, and the
// upstream stays compatible throughout.
func TestForeignMember_AC56(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B))
		f := primaryAt(B, "other", 9, 9, B, "zzz.test:1")
		c.set(f)
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(B, 1)
	h.quiet()
	if n := len(h.chk.of("zzz.test:1")); n != 0 {
		t.Fatalf("a foreign member's hosts were used for discovery (%d)", n)
	}
	// While the others run on the fast interval, the foreign member stays on the
	// heartbeat schedule.
	h.chk.down(A)
	h.stepUntilState("target lost", func(s upstream.State) bool { return !s.HasTarget })
	nB := len(h.chk.of(B))
	nA := len(h.chk.of(A))
	for i := 0; i < 16 && len(h.chk.of(A)) < nA+10; i++ {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(500 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	if len(h.chk.of(A)) < nA+4 {
		t.Fatalf("A was not checked on the fast interval (%d new checks)", len(h.chk.of(A))-nA)
	}
	if got := len(h.chk.of(B)) - nB; got > 1 {
		t.Fatalf("the foreign member was checked %d times in about 8 s; want at most one (heartbeat is 10 s)", got)
	}
	if s := h.m.State(); s.Incompatible || s.Target == B {
		t.Fatalf("state: %+v", s)
	}
	noIncompatRecord(t, h)
	// Re-admission.
	h.chk.set(primaryAt(B, "rs0", 7, 1, A, B))
	h.stepUntilState("B re-admitted", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
}

// AC-56 all members lost after a match: transient unavailability, never
// incompatible.
func TestAllMembersLostAfterMatch_AC56(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B, C))
		c.set(secondary(B, "rs0", A, A, B, C))
		c.set(secondary(C, "rs0", A, A, B, C))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(B, 1)
	h.waitCount(C, 1)
	h.quiet()
	h.chk.down(A)
	h.chk.set(primaryAt(B, "other", 2, 2, B)) // foreign
	h.chk.set(standalone(C))                  // answers as a standalone
	h.stepUntilState("unavailable", func(s upstream.State) bool { return !s.HasTarget })
	for i := 0; i < 6; i++ {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	s := h.m.State()
	if s.Incompatible {
		t.Fatalf("incompatible after losing every matching member: %+v", s)
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission: %+v", a)
	}
	noIncompatRecord(t, h)
}

// AC-56 seed rules at startup (no matching member ever found): a ghost or an
// unreachable seed causes no incompatibility; a foreign, standalone or mongos
// seed does.
func TestSeedRulesAtStartup_AC56(t *testing.T) {
	for name, rep := range map[string]*upstream.Report{
		"ghost":       ptr(ghost(A)),
		"unreachable": nil,
	} {
		t.Run("no incompatibility/"+name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) {
				if rep != nil {
					c.set(*rep)
				}
			}, false).start()
			h.waitCount(A, 1)
			for i := 0; i < 3; i++ {
				h.stepUntil(A, len(h.chk.of(A))+1)
			}
			if s := h.m.State(); s.Incompatible {
				t.Fatalf("a %s seed made the upstream incompatible", name)
			}
			noIncompatRecord(t, h)
			if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
				t.Fatalf("readiness: %+v", st)
			}
		})
	}
	for name, rep := range map[string]upstream.Report{
		"foreign":    primary(A, "other", A),
		"standalone": standalone(A),
		"mongos":     mongos(A),
	} {
		t.Run("incompatible/"+name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(rep) }, false).start()
			h.waitState("incompatible", func(s upstream.State) bool { return s.Incompatible })
		})
	}
}

func ptr(r upstream.Report) *upstream.Report { return &r }
