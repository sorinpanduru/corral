package upstream_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// FR-56: the admission decision in each state.
func TestAdmission_FR56(t *testing.T) {
	t.Run("never reached: fail fast upstream_unavailable", func(t *testing.T) {
		h := newHarness(t, rsCfg(A), nil, false).start() // nothing reachable
		h.waitCount(A, 1)
		h.quiet()
		a := h.m.Admission()
		if a.Kind != upstream.FailFast || a.Detail != "upstream_unavailable" {
			t.Fatalf("admission: %+v", a)
		}
		if _, d := h.m.HandshakeAdmission(); d != "upstream_unavailable" {
			t.Fatalf("handshake detail %q", d)
		}
		if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
			t.Fatalf("readiness: %+v", st)
		}
	})
	t.Run("election after reached: wait", func(t *testing.T) {
		h := newHarness(t, rsCfg(A), healthyRS, false).start()
		h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h.quiet()
		h.chk.set(secondary(A, "rs0", "", A, B, C))
		h.chk.set(secondary(B, "rs0", "", A, B, C))
		h.advance(10 * time.Second)
		h.waitState("no target", func(s upstream.State) bool { return !s.HasTarget })
		if a := h.m.Admission(); a.Kind != upstream.Wait {
			t.Fatalf("admission: %+v", a)
		}
		// Handshakes still succeed with the last known parameters.
		p, d := h.m.HandshakeAdmission()
		if d != "" || p.MaxMessageSizeBytes != 48000000 {
			t.Fatalf("handshake admission: %+v %q", p, d)
		}
	})
}

// FR-19 / FR-46 precedence: incompatible beats never reached.
func TestIncompatibleBeatsNeverReached_FR19(t *testing.T) {
	h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(standalone(A)) }, false).start()
	waitIncompat(h, "standalone", A)
	if s := h.m.State(); s.Reached {
		t.Fatal("an incompatible seed must not count as reaching the target")
	}
}

// AC-41 (unit): authentication failing from startup, with successful hello
// checks.
func TestAuthFailingFromStartup_AC41(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	setup := func(c *fakeChecker) {
		r := primary(A, "rs0", A)
		r.AuthErr = &upstream.AuthFailed{Err: fakeServerErr{code: 18}}
		c.set(r)
	}
	h := newHarness(t, cfg, setup, false).start()
	s := h.waitState("auth failing", func(s upstream.State) bool { return s.AuthFailing })
	if !s.Reached || !s.HasParams || s.Params.MaxMessageSizeBytes != 48000000 {
		t.Fatalf("a successful hello must set Reached and the parameters: %+v", s)
	}
	// Handshakes succeed with the parameters.
	if p, d := h.m.HandshakeAdmission(); d != "" || p.MaxWriteBatchSize != 100000 {
		t.Fatalf("handshake admission: %+v %q", p, d)
	}
	// Requests fail fast.
	if a := h.m.Admission(); a.Kind != upstream.FailFast || a.Detail != "upstream_auth_failed" {
		t.Fatalf("admission: %+v", a)
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamAuthFailed {
		t.Fatalf("readiness: %+v", st)
	}
}

// FR-22 (AC-25): which errors set the failing state. Only an AuthFailed does.
func TestPoolAuthResult_AC25(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	ctx := context.Background()
	notAuth := map[string]error{
		"code 10107":    fakeServerErr{code: 10107},
		"code 91":       fakeServerErr{code: 91},
		"code 11600":    fakeServerErr{code: 11600},
		"other code":    fakeServerErr{code: 13},
		"network error": netErr(),
		"timeout":       context.DeadlineExceeded,
		"plain":         errors.New("synthetic"),
	}
	for name, e := range notAuth {
		t.Run("not failing/"+name, func(t *testing.T) {
			h := newHarness(t, cfg, healthyRS, false).start()
			h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
			h.m.PoolAuthResult(ctx, A, e)
			h.quiet()
			if h.m.State().AuthFailing {
				t.Fatalf("%v set the authentication state", name)
			}
			if st := h.readiness(); !st.Ready {
				t.Fatalf("readiness: %+v", st)
			}
		})
	}
	t.Run("AuthFailed from a pool connection", func(t *testing.T) {
		h := newHarness(t, cfg, healthyRS, false).start()
		h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h.m.PoolAuthResult(ctx, A, &upstream.AuthFailed{Err: fakeServerErr{code: 18}})
		s := h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
		if !s.HasTarget {
			t.Fatal("the target must stay known")
		}
		if a := h.m.Admission(); a.Kind != upstream.FailFast || a.Detail != "upstream_auth_failed" {
			t.Fatalf("admission: %+v", a)
		}
		if st := h.readiness(); st.Reason != health.ReasonUpstreamAuthFailed {
			t.Fatalf("readiness: %+v", st)
		}
		// A wrapped AuthFailed counts.
		h2 := newHarness(t, cfg, healthyRS, false).start()
		h2.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h2.m.PoolAuthResult(ctx, A, fmt.Errorf("auth: %w", &upstream.AuthFailed{Err: errors.New("x")}))
		h2.waitState("failing (wrapped)", func(s upstream.State) bool { return s.AuthFailing })
	})
	t.Run("a pool success clears it", func(t *testing.T) {
		h := newHarness(t, cfg, healthyRS, false).start()
		h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h.m.PoolAuthResult(ctx, A, &upstream.AuthFailed{})
		h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
		h.m.PoolAuthResult(ctx, A, nil)
		h.waitState("cleared", func(s upstream.State) bool { return !s.AuthFailing })
		if st := h.readiness(); !st.Ready {
			t.Fatalf("readiness: %+v", st)
		}
	})
	t.Run("a transient error does not clear it", func(t *testing.T) {
		h := newHarness(t, cfg, healthyRS, false).start()
		h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h.m.PoolAuthResult(ctx, A, &upstream.AuthFailed{})
		h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
		h.m.PoolAuthResult(ctx, A, netErr())
		h.quiet()
		if !h.m.State().AuthFailing {
			t.Fatal("a network error cleared the failing state")
		}
	})
}

// FR-22: while failing, each target check asks for a fresh authenticated
// connection; the first success clears it. Only the target authenticates.
func TestAuthRecoveryChecks_AC25(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	bad := primary(A, "rs0", A, B)
	bad.AuthErr = &upstream.AuthFailed{Err: fakeServerErr{code: 18}}
	setup := func(c *fakeChecker) {
		c.set(bad)
		c.set(secondary(B, "rs0", A, A, B))
	}
	h := newHarness(t, cfg, setup, false).start()
	h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
	h.waitCount(B, 1)
	h.quiet()
	n := len(h.chk.of(A))
	for i := 0; i < 3; i++ {
		h.advance(10 * time.Second)
		h.waitCount(A, n+i+1)
	}
	h.quiet()
	cs := h.chk.of(A)
	for i := n; i < len(cs); i++ {
		if !cs[i].Req.Fresh || !cs[i].Req.Authenticate {
			t.Fatalf("check %d while failing: Fresh=%v Authenticate=%v, want true/true", i, cs[i].Req.Fresh, cs[i].Req.Authenticate)
		}
	}
	// Non-target members never authenticate (FR-22).
	for _, c := range h.chk.of(B) {
		if c.Req.Authenticate {
			t.Fatal("a non-target monitoring connection was asked to authenticate")
		}
	}
	// Recovery: the next target check succeeds in authenticating.
	good := primary(A, "rs0", A, B)
	h.chk.set(good)
	h.advance(10 * time.Second)
	h.waitState("cleared", func(s upstream.State) bool { return !s.AuthFailing })
	if st := h.readiness(); !st.Ready {
		t.Fatalf("readiness: %+v", st)
	}
	// After recovery, checks go back to reusing connections.
	h.quiet()
	n = len(h.chk.of(A))
	h.advance(10 * time.Second)
	h.waitCount(A, n+1)
	if c := h.chk.of(A)[n]; c.Req.Fresh {
		t.Fatal("checks still ask for fresh connections after recovery")
	}
}

// Authenticate=false (mechanism none) never asks a check to authenticate.
func TestNoAuthenticationRequested_FR22(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(B, 1)
	for _, c := range h.chk.all() {
		if c.Req.Authenticate {
			t.Fatalf("check of %s asked to authenticate with mechanism none", c.Host)
		}
	}
}

// FR-46 (G-9, AC-41 unit): pool connect failures alone never change
// readiness; they trigger an immediate check (no earlier than 500 ms after
// the previous one). If the checks then fail too, the target goes unknown and
// readiness becomes upstream_unavailable.
func TestPoolConnectFailed_AC41(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(primary(A, "rs0", A)) }, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.quiet()
	n := len(h.chk.of(A))

	// Called right after the previous check: the immediate check waits for the
	// 500 ms floor.
	h.m.PoolConnectFailed(ctx, A, netErr())
	h.quiet()
	if got := len(h.chk.of(A)); got != n {
		t.Fatalf("immediate check ran inside the 500 ms floor (%d -> %d)", n, got)
	}
	h.advance(499 * time.Millisecond)
	h.quiet()
	if len(h.chk.of(A)) != n {
		t.Fatal("check before 500 ms")
	}
	h.clk.Advance(time.Millisecond)
	h.waitCount(A, n+1)
	if st := h.readiness(); !st.Ready {
		t.Fatalf("pool connect failure changed readiness: %+v", st)
	}
	if a := h.m.Admission(); a.Kind == upstream.FailFast {
		t.Fatalf("pool connect failure made admission fail fast: %+v", a)
	}
	if s := h.m.State(); !s.HasTarget || s.AuthFailing {
		t.Fatalf("state: %+v", s)
	}

	// Later, well past the floor: the immediate check runs at once.
	h.quiet()
	h.advance(5 * time.Second)
	h.quiet()
	n = len(h.chk.of(A))
	h.m.PoolConnectFailed(ctx, A, netErr())
	h.waitCount(A, n+1)

	// The checks fail as well (with their retry): target unknown.
	h.chk.down(A)
	h.quiet()
	h.m.PoolConnectFailed(ctx, A, netErr())
	h.advance(time.Second)
	h.waitState("unknown after failing checks", func(s upstream.State) bool { return !s.HasTarget })
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness: %+v", st)
	}
}

// PoolConnectFailed never sets the authentication state.
func TestPoolConnectFailedDoesNotSetAuth_FR22(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.m.PoolConnectFailed(context.Background(), A, &upstream.AuthFailed{})
	h.quiet()
	if h.m.State().AuthFailing {
		t.Fatal("PoolConnectFailed set the authentication state")
	}
}
