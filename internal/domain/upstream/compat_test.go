package upstream_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

func waitIncompat(h *harness, reason string, host string) upstream.State {
	h.t.Helper()
	s := h.waitState("incompatible "+reason, func(s upstream.State) bool { return s.Incompatible })
	if string(s.IncompatReason) != reason {
		h.t.Fatalf("reason %q, want %q", s.IncompatReason, reason)
	}
	if host != "" && s.IncompatHost != host {
		h.t.Fatalf("incompatible host %q, want %q", s.IncompatHost, host)
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamIncompatible {
		h.t.Fatalf("readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.FailFast || a.Detail != "upstream_incompatible" {
		h.t.Fatalf("admission: %+v", a)
	}
	if _, d := h.m.HandshakeAdmission(); d != "upstream_incompatible" {
		h.t.Fatalf("handshake admission detail %q", d)
	}
	return s
}

// FR-18, AC-10: replica-set mode, the three seed conditions.
func TestReplicaSetModeIncompatibility_AC10(t *testing.T) {
	cases := map[string]struct {
		report upstream.Report
		reason string
	}{
		"different setName":            {primary(A, "other-set", A), "replica_set_name_mismatch"},
		"standalone":                   {standalone(A), "standalone"},
		"mongos":                       {mongos(A), "topology_mismatch"},
		"secondary of a different set": {secondary(A, "other-set", ""), "replica_set_name_mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(tc.report) }, false).start()
			waitIncompat(h, tc.reason, A)
			if h.rec != nil {
				eventually(t, "observer", func() bool {
					h.rec.mu.Lock()
					defer h.rec.mu.Unlock()
					return len(h.rec.incompat) == 1
				})
				h.rec.mu.Lock()
				got := h.rec.incompat[0]
				h.rec.mu.Unlock()
				if got != tc.reason+"@"+A {
					t.Fatalf("observer got %q", got)
				}
			}
			kinds := h.changeKinds()
			found := false
			for _, c := range kinds {
				if c.Kind == upstream.BecameIncompatible && c.Host == A && c.Reason == tc.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("no BecameIncompatible change: %+v", kinds)
			}
		})
	}
}

// FR-18: with several qualifying seeds the reason comes from the first in
// upstream.hosts order.
func TestFirstQualifyingSeedWins_AC10(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(mongos(A))                  // topology_mismatch
		c.set(standalone(B))              // standalone
		c.set(primary(C, "other-set", C)) // replica_set_name_mismatch
	}
	orders := map[string][]string{
		"A,B,C": {A, B, C}, "B,C,A": {B, C, A}, "C,A,B": {C, A, B},
	}
	want := map[string]string{A: "topology_mismatch", B: "standalone", C: "replica_set_name_mismatch"}
	for name, seeds := range orders {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, rsCfg(seeds...), setup, false).start()
			eventually(t, "all seeds checked", func() bool {
				return len(h.chk.of(A)) > 0 && len(h.chk.of(B)) > 0 && len(h.chk.of(C)) > 0
			})
			h.quiet()
			waitIncompat(h, want[seeds[0]], seeds[0])
		})
	}
	// An unreachable first seed does not qualify: the first reachable one does.
	t.Run("unreachable first", func(t *testing.T) {
		h := newHarness(t, rsCfg(D, B, A), setup, false).start()
		h.waitCount(D, 1)
		h.waitCount(B, 1)
		h.waitCount(A, 1)
		h.quiet()
		waitIncompat(h, "standalone", B)
	})
}

// FR-18: single-mongos mode.
func TestSingleMongosMode_AC10(t *testing.T) {
	h := newHarness(t, mongosCfg(A), func(c *fakeChecker) { c.set(mongos(A)) }, false).start()
	s := h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	if s.Target != A || s.Incompatible || !s.Reached {
		t.Fatalf("mongos: %+v", s)
	}
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness: %+v", st)
	}
	bad := map[string]struct {
		r      upstream.Report
		reason string
	}{
		"replica-set member": {primary(A, "rs0", A), "topology_mismatch"},
		"secondary member":   {secondary(A, "rs0", ""), "topology_mismatch"},
		"standalone":         {standalone(A), "standalone"},
		"mongos wire 6-17":   {func() upstream.Report { r := mongos(A); r.Params = params(6, 17); return r }(), "wire_version"},
		"mongos wire 26-30":  {func() upstream.Report { r := mongos(A); r.Params = params(26, 30); return r }(), "wire_version"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, mongosCfg(A), func(c *fakeChecker) { c.set(tc.r) }, false).start()
			waitIncompat(h, tc.reason, A)
		})
	}
	good := map[string][2]int32{"6-27": {6, 27}, "21-21": {21, 21}, "25-30": {25, 30}}
	for name, w := range good {
		t.Run("wire "+name, func(t *testing.T) {
			r := mongos(A)
			r.Params = params(w[0], w[1])
			h := newHarness(t, mongosCfg(A), func(c *fakeChecker) { c.set(r) }, false).start()
			s := h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
			if s.Incompatible {
				t.Fatalf("wire %v rejected", w)
			}
		})
	}
}

// AC-10: the wire-version rule on the replica-set primary.
func TestReplicaSetPrimaryWireVersion_AC10(t *testing.T) {
	for name, w := range map[string][2]int32{"6-17": {6, 17}, "26-30": {26, 30}, "0-20": {0, 20}} {
		t.Run("reject "+name, func(t *testing.T) {
			r := primary(A, "rs0", A)
			r.Params = params(w[0], w[1])
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(r) }, false).start()
			waitIncompat(h, "wire_version", A)
		})
	}
	for name, w := range map[string][2]int32{"6-27": {6, 27}, "21-21": {21, 21}, "25-30": {25, 30}} {
		t.Run("accept "+name, func(t *testing.T) {
			r := primary(A, "rs0", A)
			r.Params = params(w[0], w[1])
			h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(r) }, false).start()
			s := h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
			if s.Incompatible {
				t.Fatalf("wire %v rejected", w)
			}
		})
	}
}

// FR-18 and the Owner rule (SPEC-002 Amendment 3): a non-matching seed next to
// a matching member is ignored (never the target, no incompatibility) but is
// still re-checked on schedule.
func TestNonMatchingSeedIgnoredWithMatchingMember_AC10(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primary(A, "other-set", A))
		c.set(primary(B, "rs0", B))
	}
	h := newHarness(t, rsCfg(A, B), setup, false).start()
	s := h.waitState("target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
	h.waitCount(A, 1)
	h.quiet()
	if s.Incompatible {
		t.Fatalf("incompatible although a matching member is known: %+v", s)
	}
	nA := len(h.chk.of(A))
	h.advance(10 * 1e9)
	h.waitCount(A, nA+1)
	h.waitCount(B, 2)
	if got := len(h.chk.of(A)); got < nA+1 {
		t.Fatalf("a non-matching seed is no longer re-checked (%d -> %d)", nA, got)
	}
	if st := h.m.State(); st.Incompatible || st.Target != B {
		t.Fatalf("state: %+v", st)
	}
	// It is re-admitted when it starts matching (with a larger pair it wins).
	h.chk.set(primaryAt(A, "rs0", 8, 1, A, B))
	h.advance(10 * 1e9)
	h.waitState("A re-admitted", func(s upstream.State) bool { return s.Target == A })
}

// FR-18: losing all matching members later is transient unavailability, not
// incompatibility, even though a non-matching seed stays reachable.
func TestLosingAllMatchingMembersIsUnavailable_AC10(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primary(A, "other-set", A))
		c.set(primary(B, "rs0", B))
	}
	h := newHarness(t, rsCfg(A, B), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
	h.quiet()
	h.chk.down(B)
	h.advance(10 * 1e9)
	s := h.waitState("target lost", func(s upstream.State) bool { return !s.HasTarget })
	h.quiet()
	s = h.m.State()
	if s.Incompatible {
		t.Fatalf("incompatible after losing the matching member: %+v", s)
	}
	if st := h.readiness(); st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission: %+v", a)
	}
}

// FR-18: becomes compatible again as soon as a later check no longer meets a
// condition; one record per transition into the state.
func TestIncompatibleThenCompatible_AC10(t *testing.T) {
	h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(standalone(A)) }, false).start()
	waitIncompat(h, "standalone", A)
	h.chk.set(primary(A, "rs0", A))
	h.advance(500 * 1e6)
	s := h.waitState("compatible again", func(s upstream.State) bool { return !s.Incompatible && s.HasTarget })
	if s.Target != A {
		t.Fatalf("state: %+v", s)
	}
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness: %+v", st)
	}
	// And back again: a second transition, a second record.
	// Amendment 3: the match is sticky, so the second transition is a wire
	// version change (still allowed after a match).
	r := primary(A, "rs0", A)
	r.Params = params(0, 17)
	h.chk.set(r)
	h.advance(10 * 1e9)
	h.waitState("incompatible again", func(s upstream.State) bool { return s.Incompatible })
	eventually(t, "two incompatible records", func() bool {
		h.rec.mu.Lock()
		defer h.rec.mu.Unlock()
		return len(h.rec.incompat) == 2
	})
	h.rec.mu.Lock()
	second := h.rec.incompat[1]
	h.rec.mu.Unlock()
	if second != "wire_version@"+A {
		t.Fatalf("second record %q", second)
	}
}

// FR-18: while incompatible and compatible → incompatible, waiting requests
// and idle connections are the pool's concern; the monitor reports it through
// OnChange.
func TestBecameIncompatibleChange_FR18(t *testing.T) {
	h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(primary(A, "rs0", A)) }, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.chk.set(primary(A, "rs0", A))
	r := primary(A, "rs0", A)
	r.Params = params(0, 17) // a rolling change to an unsupported version
	h.chk.set(r)
	h.advance(10 * 1e9)
	waitIncompat(h, "wire_version", A)
	found := false
	for _, c := range h.changeKinds() {
		if c.Kind == upstream.BecameIncompatible && c.Host == A {
			found = true
		}
	}
	if !found {
		t.Fatalf("no BecameIncompatible: %+v", h.changeKinds())
	}
}
