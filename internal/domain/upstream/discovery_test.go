package upstream_test

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

const (
	A = "a.test:27017"
	B = "b.test:27017"
	C = "c.test:27017"
	D = "d.test:27017"
	X = "arb.test:27017"
)

func healthyRS(c *fakeChecker) {
	c.set(primary(A, "rs0", A, B, C))
	c.set(secondary(B, "rs0", A, A, B, C))
	c.set(secondary(C, "rs0", A, A, B, C))
}

// FR-37, FR-35: a healthy replica set. The target is the primary, members
// learned from `hosts` are checked, readiness is ready.
func TestHealthyReplicaSet_AC18(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	s := h.waitState("target known", func(s upstream.State) bool { return s.HasTarget })
	if s.Target != A || !s.Reached || s.Incompatible || s.AuthFailing || !s.HasParams {
		t.Fatalf("state: %+v", s)
	}
	if s.Params.MaxMessageSizeBytes != 48000000 || s.Params.MaxWire != 21 {
		t.Fatalf("params: %+v", s.Params)
	}
	h.waitCount(B, 1)
	h.waitCount(C, 1)
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.Ready || a.Target != A {
		t.Fatalf("admission: %+v", a)
	}
	p, detail := h.m.HandshakeAdmission()
	if detail != "" || p.MaxMessageSizeBytes != 48000000 {
		t.Fatalf("handshake admission: %+v %q", p, detail)
	}
}

// FR-37: while a target is known every member is checked each heartbeat.
func TestHeartbeatSchedule_FR37(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(B, 1)
	h.waitCount(C, 1)
	h.quiet()
	base := map[string]int{A: len(h.chk.of(A)), B: len(h.chk.of(B)), C: len(h.chk.of(C))}
	h.advance(10*time.Second - time.Millisecond)
	h.quiet()
	for _, host := range []string{A, B, C} {
		if got := len(h.chk.of(host)); got != base[host] {
			t.Fatalf("%s checked early: %d -> %d", host, base[host], got)
		}
	}
	h.clk.Advance(time.Millisecond)
	for _, host := range []string{A, B, C} {
		h.waitCount(host, base[host]+1)
	}
	h.quiet()
	for _, host := range []string{A, B, C} {
		if got := len(h.chk.of(host)); got != base[host]+1 {
			t.Fatalf("%s: %d checks, want exactly %d", host, got, base[host]+1)
		}
	}
}

// AC-18 (unit): discovery from a secondary's hosts, passives and primary,
// while no primary is known. Arbiters are dropped.
func TestDiscoveryFromSecondary_AC18(t *testing.T) {
	setup := func(c *fakeChecker) {
		s := secondary(A, "rs0", D, A, B, X)
		s.Passives = []string{C}
		c.set(s)
		c.set(secondary(B, "rs0", D, A, B))
		c.set(secondary(C, "rs0", D, A, B))
		c.set(upstream.Report{Host: X, ArbiterOnly: true, SetName: "rs0", HasSetName: true, Hosts: []string{A, B, X}, Params: params(0, 21)})
		c.set(primary(D, "rs0", A, B, D))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	for _, host := range []string{B, C, D, X} {
		h.waitCount(host, 1) // B via hosts, C via passives, D via primary, X via hosts
	}
	h.waitState("primary found via the primary field", func(s upstream.State) bool { return s.HasTarget && s.Target == D })
	// The arbiter is not monitored further.
	h.quiet()
	h.stepUntil(B, 2)
	h.quiet()
	if n := len(h.chk.of(X)); n != 1 {
		t.Fatalf("arbiter checked %d times, want 1", n)
	}
}

// Owner rule (SPEC-002 Amendment 3): a server learned from a member's list that
// reports a different setName is ignored (never a candidate, never the target)
// but stays known: it is re-checked on schedule and re-admitted when it matches.
func TestNonMatchingServerIgnoredButRechecked_FR37(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primary(A, "rs0", A, D))
		c.set(primaryAt(D, "other", 9, 9, D)) // listed by A, claims primary of another set
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(D, 1)
	h.quiet()
	nD := len(h.chk.of(D))
	for i := 0; i < 3; i++ {
		h.stepUntil(D, nD+i+1)
	}
	if s := h.m.State(); s.Target != A || s.Incompatible {
		t.Fatalf("a member of another set affected the state: %+v", s)
	}
	// D now reports the right set, with a larger (electionId, setVersion): it is
	// re-admitted and wins.
	h.chk.set(primaryAt(D, "rs0", 7, 1, A, D))
	h.chk.set(secondary(A, "rs0", D, A, D))
	h.advance(10 * time.Second)
	h.waitState("D re-admitted as the target", func(s upstream.State) bool { return s.HasTarget && s.Target == D })
}

// Owner rule: a member reporting no setName, or failing, stays known and is
// re-checked on schedule; it is admitted when it later matches.
func TestUnreadyAndFailingMembersStayKnown_FR37(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primary(A, "rs0", A, B, C))
		c.set(upstream.Report{Host: B, Params: params(0, 21)}) // reports no setName (not yet initialised)
		// C is unreachable.
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(B, 1)
	h.waitCount(C, 1)
	h.quiet()
	nB, nC := len(h.chk.of(B)), len(h.chk.of(C))
	for i := 0; i < 3; i++ {
		h.stepUntil(B, nB+i+1)
		h.waitCount(C, nC+i+1)
	}
	if s := h.m.State(); s.Incompatible || s.Target != A {
		t.Fatalf("state: %+v", s)
	}
	// Both come up as members of the set and are then followed like any member.
	h.chk.set(secondary(B, "rs0", A, A, B, C))
	h.chk.set(secondary(C, "rs0", A, A, B, C))
	h.stepUntil(B, nB+4)
	h.waitCount(C, nC+4)
	// A steps down, B becomes primary with a larger pair: B is the target.
	h.chk.set(secondary(A, "rs0", B, A, B, C))
	h.chk.set(primaryAt(B, "rs0", 5, 1, A, B, C))
	h.advance(10 * time.Second)
	h.waitState("B is the target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
}

// AC-18: while no target is known every known member is checked at least
// 500 ms apart, and exactly that often on the fake clock.
func TestFastScheduleWithoutTarget_AC18(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(secondary(A, "rs0", "", A, B))
		c.set(secondary(B, "rs0", "", A, B))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitCount(A, 1)
	h.waitCount(B, 1)
	h.quiet()
	for step := 2; step <= 6; step++ {
		h.advance(499 * time.Millisecond)
		h.quiet()
		if n := len(h.chk.of(A)); n != step-1 {
			t.Fatalf("step %d: A checked early (%d)", step, n)
		}
		h.clk.Advance(time.Millisecond)
		h.waitCount(A, step)
		h.waitCount(B, step)
		h.quiet()
	}
	// Every pair of consecutive checks of one host is at least 500 ms apart.
	for _, host := range []string{A, B} {
		cs := h.chk.of(host)
		for i := 1; i < len(cs); i++ {
			if d := cs[i].At.Sub(cs[i-1].At); d < upstream.MinHeartbeat {
				t.Fatalf("%s checks %d and %d are %v apart", host, i-1, i, d)
			}
		}
	}
	if s := h.m.State(); s.HasTarget || s.Reached {
		t.Fatalf("state with no primary: %+v", s)
	}
}

// FR-35 (AC-18): the larger (electionId, setVersion) wins; a later smaller
// report is ignored. electionId is compared first.
func TestStalePrimaryRule_AC18(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 5, A, B))
		c.set(primaryAt(B, "rs0", 2, 1, A, B))
	}
	h := newHarness(t, rsCfg(A, B), setup, false).start()
	s := h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(A, 1)
	h.waitCount(B, 1)
	h.waitState("B wins on the larger electionId", func(s upstream.State) bool { return s.Target == B })
	_ = s

	step := func(a upstream.Report, wantTarget string) {
		t.Helper()
		n := len(h.chk.of(a.Host))
		h.chk.set(a)
		h.advance(10 * time.Second)
		h.waitCount(a.Host, n+1)
		h.quiet()
		if got := h.m.State().Target; got != wantTarget {
			t.Fatalf("after %s claims (%v,%d): target %q, want %q", a.Host, a.ElectionID[11], a.SetVersion, got, wantTarget)
		}
	}
	// A claims a smaller electionId with a much larger setVersion: stale.
	step(primaryAt(A, "rs0", 1, 99, A, B), B)
	// A claims the same electionId and a smaller setVersion: stale.
	step(primaryAt(A, "rs0", 2, 0, A, B), B)
	// A claims a larger setVersion at the same electionId: wins.
	step(primaryAt(A, "rs0", 2, 3, A, B), A)
	// Later the old, smaller report from B is ignored.
	step(primaryAt(B, "rs0", 1, 1, A, B), A)
}

// FR-35: the retry-once rule. One failed check of a usable target followed by
// a successful retry leaves everything unchanged; two failures mark it unknown.
func TestRetryOnce_AC32(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(A, 1)
	h.quiet()
	n0 := len(h.chk.of(A))

	// A's next check fails once; the retry (Fresh, on a new connection) is held
	// until the test lets it succeed.
	release := make(chan struct{})
	h.chk.mu.Lock()
	h.chk.hook = func(req upstream.CheckRequest) {
		if req.Host == A && req.Fresh {
			<-release
		}
	}
	h.chk.mu.Unlock()
	h.chk.down(A)
	h.advance(10 * time.Second)
	h.waitCount(A, n0+2) // the failed check and its immediate retry, no clock advance between
	if s := h.m.State(); !s.HasTarget || s.Target != A {
		t.Fatalf("target lost during the retry: %+v", s)
	}
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness changed during the retry: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.Ready {
		t.Fatalf("admission changed during the retry: %+v", a)
	}
	cs := h.chk.of(A)
	if cs[n0].Req.Fresh || !cs[n0+1].Req.Fresh {
		t.Fatalf("first check Fresh=%v, retry Fresh=%v; want false and true", cs[n0].Req.Fresh, cs[n0+1].Req.Fresh)
	}
	if !cs[n0+1].At.Equal(cs[n0].At) {
		t.Fatal("the retry was not immediate")
	}
	h.chk.set(primary(A, "rs0", A, B, C)) // the retry will succeed
	close(release)
	h.quiet()
	if s := h.m.State(); !s.HasTarget || s.Target != A {
		t.Fatalf("target lost after a successful retry: %+v", s)
	}
}

func TestTwoFailuresMarkUnknown_AC32(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(A, 1)
	h.quiet()
	n0 := len(h.chk.of(A))
	h.chk.down(A)
	h.stepUntil(A, n0+2)
	s := h.waitState("target unknown", func(s upstream.State) bool { return !s.HasTarget })
	if !s.Reached {
		t.Fatal("Reached must stay true after losing the target")
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission after the target was lost: %+v", a)
	}
	// Fast schedule: the next check is 500 ms later, and a failed check of a
	// target that was not usable gets no immediate retry.
	h.quiet()
	n1 := len(h.chk.of(A))
	h.advance(499 * time.Millisecond)
	h.quiet()
	if len(h.chk.of(A)) != n1 {
		t.Fatal("checked before 500 ms")
	}
	h.clk.Advance(time.Millisecond)
	h.waitCount(A, n1+1)
	h.quiet()
	if got := len(h.chk.of(A)); got != n1+1 {
		t.Fatalf("a failed check of an unknown target was retried at once (%d -> %d)", n1, got)
	}
	// Recovery.
	h.chk.set(primary(A, "rs0", A, B, C))
	h.advance(500 * time.Millisecond)
	h.waitState("target back", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
	if st := h.readiness(); !st.Ready {
		t.Fatalf("not ready after recovery: %+v", st)
	}
}
