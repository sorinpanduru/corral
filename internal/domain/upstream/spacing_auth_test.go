package upstream_test

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// FR-37 (SDAM): checks of one member are at least 500 ms apart, counted from
// the END of the previous check (not its start), so a check that took a whole
// connect timeout is not followed at once. A silent seed gets exactly one
// check before the AC-61 judgement.
func TestMinimumSpacingFromEndOfPreviousCheck_FR37(t *testing.T) {
	g := newGates(A)
	h := seedHarness(t, g, []string{A, B}, func(c *fakeChecker) {
		c.set(primary(B, "other", B))
		c.down(A)
	})
	h.waitCount(A, 1)
	h.waitCount(B, 1)
	h.assertNotYetJudged("the silent seed has not finished")
	if n := len(h.chk.of(A)); n != 1 {
		t.Fatalf("the silent seed was checked %d times before the judgement, want exactly 1", n)
	}
	started := h.chk.of(A)[0].At
	h.clk.Advance(2 * time.Second) // upstream.connect_timeout: the first check ends here
	ended := h.clk.Now()
	g.release(A) // the check fails
	waitIncompat(h, "replica_set_name_mismatch", B)
	// The next check of A starts no earlier than 500 ms after the first ended.
	h.clk.Advance(400 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if n := len(h.chk.of(A)); n != 1 {
		t.Fatalf("A was checked again %d ms after its first check ended", 400)
	}
	h.stepUntil(A, 2)
	second := h.chk.of(A)[1].At
	if second.Sub(ended) < 500*time.Millisecond {
		t.Fatalf("the second check started %v after the first ended (started %v), want at least 500 ms", second.Sub(ended), started)
	}
}

// FR-22, FR-46: a transient failure of the target's authenticating check (here a
// timeout, as when SCRAM stalls) leaves the target unusable until an
// authenticating check succeeds; the checks that follow keep asking for
// authentication, and readiness is upstream_unavailable, not ready.
func TestFailedAuthCheckKeepsTargetUnusableUntilSuccess_FR22(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	var stall atomic.Bool
	h := newHarness(t, cfg, healthyRS, false)
	h.chk.mu.Lock()
	h.chk.hook = func(req upstream.CheckRequest) {
		if req.Host != A {
			return
		}
		if req.Authenticate && stall.Load() {
			h.chk.failWith(A, os.ErrDeadlineExceeded) // an authenticating check times out
		} else {
			h.chk.set(primary(A, "rs0", A, B, C)) // the hello alone succeeds
		}
	}
	h.chk.mu.Unlock()
	h.start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
	h.quiet()

	stall.Store(true)
	h.stepUntilState("the target lost after a failed authenticating check", func(s upstream.State) bool { return !s.HasTarget })
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness %+v, want upstream_unavailable", st)
	}
	// While the stall lasts, A never becomes usable again, although a check
	// without authentication would succeed, and every check asks to authenticate.
	n0 := len(h.chk.of(A))
	for i := 0; i < 20; i++ {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(500 * time.Millisecond)
		time.Sleep(3 * time.Millisecond)
		if st := h.m.State(); st.HasTarget {
			t.Fatal("the target became usable while authenticating checks keep failing")
		} else if st.AuthFailing {
			t.Fatal("a transient authentication stall set the authentication-failing state")
		}
	}
	cs := h.chk.of(A)
	if len(cs) <= n0 {
		t.Fatal("A was not checked again during the stall")
	}
	for i := n0; i < len(cs); i++ {
		if !cs[i].Req.Authenticate {
			t.Fatalf("check %d during the stall did not ask to authenticate", i)
		}
	}
	// The stall ends: the next authenticating check succeeds and A is usable.
	stall.Store(false)
	h.stepUntilState("the target usable again", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness %+v after recovery", st)
	}
	// A transient stall is no authentication failure: no state, no event.
	if h.m.State().AuthFailing {
		t.Fatal("AuthFailing after a transient stall")
	}
	h.rec.mu.Lock()
	ko := append([]string(nil), h.rec.authKO...)
	h.rec.mu.Unlock()
	if len(ko) != 0 {
		t.Fatalf("AuthFailed observer calls for a transient stall: %v", ko)
	}
}
