package upstream_test

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// Wave 2 review fixes.

func indexOfChange(cs []upstream.Change, kind upstream.ChangeKind, host string) int {
	for i, c := range cs {
		if c.Kind == kind && (host == "" || c.Host == host) {
			return i
		}
	}
	return -1
}

// M1: when the current primary becomes incompatible, BecameIncompatible comes
// first (naming the old target's host, so the pool closes its connections with
// reason incompatible) and no HostNotPrimary or HostUnknown change precedes it.
func TestChangeOrderOnIncompatibility_M1(t *testing.T) {
	h := newHarness(t, rsCfg(A), func(c *fakeChecker) { c.set(primary(A, "rs0", A)) }, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	r := primary(A, "rs0", A)
	r.Params = params(0, 17)
	h.chk.set(r)
	h.advance(10 * time.Second)
	waitIncompat(h, "wire_version", A)
	cs := h.changeKinds()
	inc := indexOfChange(cs, upstream.BecameIncompatible, A)
	if inc < 0 {
		t.Fatalf("no BecameIncompatible for the old target: %+v", cs)
	}
	for i := 0; i < inc; i++ {
		if cs[i].Kind == upstream.HostNotPrimary || cs[i].Kind == upstream.HostUnknown {
			t.Fatalf("change %d (%v) precedes BecameIncompatible: %+v", i, cs[i].Kind, cs)
		}
	}
	// The same transition in single-mongos mode: the host is re-pointed at a
	// replica-set member (Amendment 3: a match is sticky in replica-set mode,
	// so the name-mismatch route is no longer available after a match).
	h2 := newHarness(t, mongosCfg(A), func(c *fakeChecker) { c.set(mongos(A)) }, false).start()
	h2.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h2.chk.set(primary(A, "rs0", A))
	h2.advance(10 * time.Second)
	waitIncompat(h2, "topology_mismatch", A)
	cs = h2.changeKinds()
	inc = indexOfChange(cs, upstream.BecameIncompatible, A)
	if inc < 0 {
		t.Fatalf("no BecameIncompatible for the old target: %+v", cs)
	}
	for i := 0; i < inc; i++ {
		if cs[i].Kind == upstream.HostNotPrimary {
			t.Fatalf("HostNotPrimary precedes BecameIncompatible: %+v", cs)
		}
	}
}

// ReplyCodes: a writeConcernError code counts only in an ok:1 reply; in an ok:0
// reply only the top-level code counts.
func TestReplyCodesWriteConcernOnlyWithOk1_FR35(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		f    upstream.ReplyCodeInfo
		want bool
	}{
		{"ok:1 with WCE 91", upstream.ReplyCodeInfo{HasWCE: true, WCE: 91}, true},
		{"ok:0 with WCE 91 and no top-level code", upstream.ReplyCodeInfo{OKZero: true, HasWCE: true, WCE: 91}, false},
		{"ok:0 with a non-marking top code and WCE 91", upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: 11000, HasWCE: true, WCE: 91}, false},
		{"ok:0 with a marking top code", upstream.ReplyCodeInfo{OKZero: true, HasTop: true, Top: 10107, HasWCE: true, WCE: 5}, true},
		{"ok:1 with a non-marking WCE", upstream.ReplyCodeInfo{HasWCE: true, WCE: 64}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := rsHarness(t)
			if got := h.m.ReplyCodes(ctx, A, upstream.Forwarded, tc.f); got != tc.want {
				t.Fatalf("marked = %v, want %v", got, tc.want)
			}
		})
	}
	// Single-mongos mode never marks on a writeConcernError.
	hm := mongosHarness(t)
	if hm.m.ReplyCodes(ctx, A, upstream.Forwarded, upstream.ReplyCodeInfo{HasWCE: true, WCE: 91}) {
		t.Fatal("a mongos was marked from a writeConcernError")
	}
}

type tlsErr struct{}

func (tlsErr) Error() string { return "synthetic tls failure" }

// FR-35: the immediate retry applies only to network, TLS and timeout errors.
// Anything else (an error reply, a protocol error) marks the target unknown on
// the first failure, with no retry.
func TestRetryOnlyForTransportErrors_FR35(t *testing.T) {
	retried := map[string]error{
		"network":   netErr(),
		"tls":       tls.AlertError(40),
		"timeout":   os.ErrDeadlineExceeded,
		"timeout 2": context.DeadlineExceeded,
	}
	for name, e := range retried {
		t.Run("retry/"+name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), healthyRS, false).start()
			h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
			h.quiet()
			n0 := len(h.chk.of(A))
			release := make(chan struct{})
			h.chk.mu.Lock()
			h.chk.hook = func(req upstream.CheckRequest) {
				if req.Host == A && req.Fresh {
					<-release
				}
			}
			h.chk.mu.Unlock()
			h.chk.failWith(A, e)
			h.stepUntil(A, n0+2)
			if !h.chk.of(A)[n0+1].Req.Fresh {
				t.Fatal("the second check is not a fresh retry")
			}
			if !h.m.State().HasTarget {
				t.Fatal("target lost during the retry")
			}
			close(release)
		})
	}
	other := map[string]error{
		"server error reply": fakeServerErr{code: 11600},
		"plain error":        errors.New("synthetic non-transport failure"),
		"protocol error":     protoErr{},
	}
	for name, e := range other {
		t.Run("no retry/"+name, func(t *testing.T) {
			h := newHarness(t, rsCfg(A), healthyRS, false).start()
			h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
			h.quiet()
			n0 := len(h.chk.of(A))
			h.chk.failWith(A, e)
			h.stepUntil(A, n0+1)
			h.waitState("target unknown", func(s upstream.State) bool { return !s.HasTarget })
			h.quiet()
			if got := len(h.chk.of(A)); got != n0+1 {
				t.Fatalf("a non-transport failure was retried (%d -> %d checks)", n0, got)
			}
		})
	}
}

type protoErr struct{}

func (protoErr) Error() string             { return "synthetic protocol failure" }
func (protoErr) CorralErrorDetail() string { return "malformed_message" }

// An immediate authenticating check of the new target follows a target change:
// the new primary's monitoring connection authenticates without waiting for the
// next heartbeat.
func TestAuthenticatingCheckOnTargetChange_FR22(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B))
		c.set(secondary(B, "rs0", A, A, B))
	}
	h := newHarness(t, cfg, setup, false).start()
	h.waitState("A target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(B, 1)
	h.quiet()
	for _, c := range h.chk.of(B) {
		if c.Req.Authenticate {
			t.Fatal("a non-target was asked to authenticate")
		}
	}
	h.chk.set(secondary(A, "rs0", B, A, B))
	h.chk.set(primaryAt(B, "rs0", 2, 1, A, B))
	h.advance(10 * time.Second)
	h.waitState("B target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
	changeAt := h.clk.Now()
	// Within the 500 ms floor (not another heartbeat) B is checked again with
	// authentication.
	h.advance(500 * time.Millisecond)
	eventually(t, "an authenticating check of the new target", func() bool {
		for _, c := range h.chk.of(B) {
			if c.Req.Authenticate && !c.At.Before(changeAt) {
				return true
			}
		}
		return false
	})
	if h.clk.Now().Sub(changeAt) > 2*time.Second {
		t.Fatal("test setup: the clock advanced too far")
	}
}

// FR-22, FR-56: while authentication is failing, successive target checks are
// delayed further by the reconnect backoff, and the spacing returns to the
// heartbeat interval once authentication succeeds.
func TestFailingStateChecksUseBackoff_FR56(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	cfg.HeartbeatInterval = 2 * time.Second
	bad := primary(A, "rs0", A)
	bad.AuthErr = &upstream.AuthFailed{Err: fakeServerErr{code: 18}}
	h := newHarness(t, cfg, func(c *fakeChecker) { c.set(bad) }, false).start()
	h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
	h.waitCount(A, 1)
	const failures = 7
	for len(h.chk.of(A)) < failures {
		n := len(h.chk.of(A))
		for i := 0; i < 200 && len(h.chk.of(A)) == n; i++ {
			h.clk.WaitForTimers(1, time.Second)
			h.clk.Advance(100 * time.Millisecond)
			time.Sleep(2 * time.Millisecond)
		}
		if len(h.chk.of(A)) == n {
			t.Fatalf("check %d never ran", n+1)
		}
	}
	cs := h.chk.of(A)
	gap := func(i int) time.Duration { return cs[i].At.Sub(cs[i-1].At) }
	// The first gap is AC-57's immediate authenticating check of a new target
	// (at least 500 ms after the previous one); only steady-state checks while
	// auth is failing are spaced by the heartbeat and the backoff.
	for i := 2; i < len(cs); i++ {
		if gap(i) < cfg.HeartbeatInterval {
			t.Fatalf("gap %d = %v is below the heartbeat interval", i, gap(i))
		}
	}
	if g := gap(len(cs) - 1); g < cfg.HeartbeatInterval+time.Second {
		t.Fatalf("after %d consecutive failures the gap is %v: the backoff is not applied (heartbeat %v)", len(cs)-1, g, cfg.HeartbeatInterval)
	}
	// Recovery: authentication succeeds, spacing is the heartbeat again.
	good := primary(A, "rs0", A)
	h.chk.set(good)
	h.waitState("failing cleared", func(s upstream.State) bool {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(500 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
		return !s.AuthFailing
	})
	h.quiet()
	n := len(h.chk.of(A))
	var last time.Time
	for round := 0; round < 3; round++ {
		before := len(h.chk.of(A))
		for i := 0; i < 400 && len(h.chk.of(A)) == before; i++ {
			h.clk.WaitForTimers(1, time.Second)
			h.clk.Advance(50 * time.Millisecond)
			time.Sleep(2 * time.Millisecond)
		}
		got := h.chk.of(A)
		if len(got) == before {
			t.Fatal("no check after recovery")
		}
		if !last.IsZero() {
			if d := got[len(got)-1].At.Sub(last); d > cfg.HeartbeatInterval+200*time.Millisecond {
				t.Fatalf("spacing after recovery = %v, want about the heartbeat %v", d, cfg.HeartbeatInterval)
			}
		}
		last = got[len(got)-1].At
	}
	_ = n
}

// A report from a check that was launched before the marking does not clear
// it: the host stays marked until a check launched after the marking confirms a
// primary.
func TestStaleReportDoesNotClearMarking_FR35(t *testing.T) {
	h := rsHarness(t)
	n0 := len(h.chk.of(A))
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	h.chk.mu.Lock()
	h.chk.hook = func(req upstream.CheckRequest) {
		if req.Host == A {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}
	}
	h.chk.mu.Unlock()
	h.advance(10 * time.Second) // launches a check of A that blocks in the hook
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight check never started")
	}
	_ = n0
	if !h.m.ReplyCodes(context.Background(), A, upstream.Forwarded, top(10107)) {
		t.Fatal("not marked")
	}
	if h.m.State().HasTarget {
		t.Fatal("still a target right after marking")
	}
	// The in-flight (pre-marking) check now completes reporting A as primary.
	h.chk.mu.Lock()
	h.chk.hook = nil
	h.chk.mu.Unlock()
	close(release)
	h.quiet()
	if h.m.State().HasTarget {
		t.Fatal("a report from a check launched before the marking cleared it")
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission %+v, want Wait until a later check confirms", a)
	}
	// A check launched after the marking confirms.
	h.advance(500 * time.Millisecond)
	h.waitState("confirmed", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
}
