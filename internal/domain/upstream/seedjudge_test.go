package upstream_test

import (
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// AC-61 (FR-18, FR-19, FR-46, FR-49, FR-56; Amendment 5), unit with a fake
// clock: in replica-set mode, incompatibility from seed results is judged only
// once every seed has finished its first check. Until then readiness and
// fail-fast report upstream_unavailable.

// gates holds the first check of chosen hosts until released.
type gates struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func newGates(hosts ...string) *gates {
	g := &gates{ch: map[string]chan struct{}{}}
	for _, h := range hosts {
		g.ch[h] = make(chan struct{})
	}
	return g
}

func (g *gates) hook(req upstream.CheckRequest) {
	g.mu.Lock()
	c := g.ch[req.Host]
	g.mu.Unlock()
	if c != nil {
		<-c
	}
}

func (g *gates) release(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.ch[host]; c != nil {
		close(c)
		delete(g.ch, host)
	}
}

func (g *gates) releaseAll() {
	g.mu.Lock()
	hosts := make([]string, 0, len(g.ch))
	for h := range g.ch {
		hosts = append(hosts, h)
	}
	g.mu.Unlock()
	for _, h := range hosts {
		g.release(h)
	}
}

func seedHarness(t *testing.T, g *gates, seeds []string, setup func(*fakeChecker)) *harness {
	t.Helper()
	h := newHarness(t, rsCfg(seeds...), setup, false)
	h.chk.mu.Lock()
	h.chk.hook = g.hook
	h.chk.mu.Unlock()
	t.Cleanup(g.releaseAll)
	return h.start()
}

func (h *harness) assertNotYetJudged(what string) {
	h.t.Helper()
	h.quiet()
	s := h.m.State()
	if s.Incompatible {
		h.t.Fatalf("%s: judged incompatible too early: %+v", what, s)
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		h.t.Fatalf("%s: readiness %+v, want upstream_unavailable", what, st)
	}
	if a := h.m.Admission(); a.Kind != upstream.FailFast || a.Detail != "upstream_unavailable" {
		h.t.Fatalf("%s: admission %+v, want fail-fast upstream_unavailable", what, a)
	}
	if _, d := h.m.HandshakeAdmission(); d != "upstream_unavailable" {
		h.t.Fatalf("%s: handshake admission detail %q", what, d)
	}
	h.rec.mu.Lock()
	n := len(h.rec.incompat)
	h.rec.mu.Unlock()
	if n != 0 {
		h.t.Fatalf("%s: %d upstream incompatible records", what, n)
	}
}

func (h *harness) incompatRecords() []string {
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	return append([]string(nil), h.rec.incompat...)
}

// A foreign seed answers first, a matching seed later: nothing is judged until
// the matching one has answered, and no incompatible record is ever emitted.
func TestForeignSeedFirstThenMatching_AC61(t *testing.T) {
	g := newGates(B)
	h := seedHarness(t, g, []string{A, B}, func(c *fakeChecker) {
		c.set(primary(A, "other", A))
		c.set(primary(B, "rs0", A, B))
	})
	h.waitCount(A, 1)
	h.assertNotYetJudged("A foreign answered, B pending")
	h.clk.Advance(3 * time.Second) // shorter than a connect timeout
	h.assertNotYetJudged("after 3 s")
	g.release(B)
	h.waitState("B answered", func(s upstream.State) bool { return s.HasTarget })
	if s := h.m.State(); s.Incompatible {
		t.Fatalf("incompatible: %+v", s)
	}
	if got := h.incompatRecords(); len(got) != 0 {
		t.Fatalf("records %v", got)
	}
}

// The reason does not depend on the order the checks finish: B foreign, A a
// standalone gives one record with reason standalone, after both checks, in
// either order.
func TestReasonIndependentOfAnswerOrder_AC61(t *testing.T) {
	for _, slow := range []string{A, B} {
		t.Run("slow seed "+slow, func(t *testing.T) {
			g := newGates(slow)
			h := seedHarness(t, g, []string{A, B}, func(c *fakeChecker) {
				c.set(standalone(A))
				c.set(primary(B, "other", B))
			})
			fast := B
			if slow == B {
				fast = A
			}
			h.waitCount(fast, 1)
			h.assertNotYetJudged("one seed pending")
			g.release(slow)
			s := waitIncompat(h, "standalone", A)
			if s.IncompatHost != A {
				t.Fatalf("host %q", s.IncompatHost)
			}
			h.quiet()
			if got := h.incompatRecords(); len(got) != 1 || got[0] != "standalone@"+A {
				t.Fatalf("records %v, want exactly [standalone@%s]", got, A)
			}
			if len(h.chk.of(A)) < 1 || len(h.chk.of(B)) < 1 {
				t.Fatal("the record was emitted before both first checks finished")
			}
		})
	}
}

// An unreachable seed counts once its first check fails (no FR-35 retry): the
// upstream is then incompatible with the first qualifying seed's reason.
func TestUnreachableSeedCountsWhenItFails_AC61(t *testing.T) {
	g := newGates(B)
	h := seedHarness(t, g, []string{A, B}, func(c *fakeChecker) {
		c.set(primary(A, "other", A))
		c.down(B)
	})
	h.waitCount(A, 1)
	h.assertNotYetJudged("B has not answered")
	h.clk.Advance(10 * time.Second) // upstream.connect_timeout elapses; the checker fails B
	g.release(B)
	waitIncompat(h, "replica_set_name_mismatch", A)
	h.quiet()
	if got := h.incompatRecords(); len(got) != 1 || got[0] != "replica_set_name_mismatch@"+A {
		t.Fatalf("records %v", got)
	}
	if n := len(h.chk.of(B)); n != 1 {
		t.Fatalf("B was checked %d times, want 1 (no retry of a first check)", n)
	}
}

// A ghost seed never causes incompatibility itself; with A foreign it is judged
// once both first checks have finished.
func TestGhostSeedWithForeignSeed_AC61(t *testing.T) {
	g := newGates(B)
	h := seedHarness(t, g, []string{A, B}, func(c *fakeChecker) {
		c.set(primary(A, "other", A))
		c.set(ghost(B))
	})
	h.waitCount(A, 1)
	h.assertNotYetJudged("ghost pending")
	g.release(B)
	waitIncompat(h, "replica_set_name_mismatch", A)
}

// FR-37 (Owner decision 2026-10-25): every seed's first check is started at
// startup, before any of them completes.
func TestFirstChecksStartConcurrently_AC61(t *testing.T) {
	g := newGates(A, B, C)
	h := seedHarness(t, g, []string{A, B, C}, nil)
	for _, host := range []string{A, B, C} {
		h.waitCount(host, 1) // every first check has started while all are blocked
	}
	if h.m.State().Incompatible {
		t.Fatal("judged before any check completed")
	}
}
