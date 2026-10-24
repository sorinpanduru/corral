package upstream_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

var fr35Codes = []int32{10107, 13435, 13436, 11602, 11600, 189, 91}

func top(code int32) upstream.ReplyCodeInfo {
	return upstream.ReplyCodeInfo{HasTop: true, OKZero: true, Top: code, TopName: "Synthetic"}
}
func wce(code int32) upstream.ReplyCodeInfo {
	return upstream.ReplyCodeInfo{HasWCE: true, WCE: code}
}

func rsHarness(t *testing.T) *harness {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.waitCount(B, 1)
	h.quiet()
	return h
}

func mongosHarness(t *testing.T) *harness {
	h := newHarness(t, mongosCfg(A), func(c *fakeChecker) { c.set(mongos(A)) }, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.quiet()
	return h
}

// marks asserts the decision on a fresh harness per case.
func decide(t *testing.T, mk func(*testing.T) *harness, src upstream.ReplySource, f upstream.ReplyCodeInfo) bool {
	h := mk(t)
	return h.m.ReplyCodes(context.Background(), A, src, f)
}

// FR-35, AC-43 (unit): replica-set mode marks on every FR-35 code, as a
// top-level ok:0 code or as writeConcernError.code.
func TestReplyCodesReplicaSet_AC43(t *testing.T) {
	for _, c := range fr35Codes {
		t.Run(fmt.Sprint(c), func(t *testing.T) {
			if !decide(t, rsHarness, upstream.Forwarded, top(c)) {
				t.Errorf("top-level %d did not mark", c)
			}
			if !decide(t, rsHarness, upstream.Forwarded, wce(c)) {
				t.Errorf("writeConcernError %d did not mark", c)
			}
		})
	}
	for _, c := range []int32{0, 8, 13, 18, 11000, 262, 6, 10108, 190} {
		if decide(t, rsHarness, upstream.Forwarded, top(c)) {
			t.Errorf("top-level %d marked", c)
		}
		if decide(t, rsHarness, upstream.Forwarded, wce(c)) {
			t.Errorf("writeConcernError %d marked", c)
		}
	}
	// A top-level code in an ok:1 reply is not an ok:0 error: no marking.
	if decide(t, rsHarness, upstream.Forwarded, upstream.ReplyCodeInfo{HasTop: true, OKZero: false, Top: 10107}) {
		t.Error("top-level code with ok:1 marked")
	}
	// Nothing to inspect.
	if decide(t, rsHarness, upstream.Forwarded, upstream.ReplyCodeInfo{}) {
		t.Error("an empty ReplyCodeInfo marked")
	}
}

// FR-35 (G-13, AC-43 unit): single-mongos mode marks only on a top-level
// 91 or 11600 in an ok:0 reply.
func TestReplyCodesSingleMongos_AC43(t *testing.T) {
	for _, c := range []int32{10107, 13435, 189, 11602, 13436} {
		if decide(t, mongosHarness, upstream.Forwarded, top(c)) {
			t.Errorf("top-level %d marked the mongos", c)
		}
	}
	for _, c := range fr35Codes {
		if decide(t, mongosHarness, upstream.Forwarded, wce(c)) {
			t.Errorf("writeConcernError %d marked the mongos", c)
		}
	}
	for _, c := range []int32{91, 11600} {
		if !decide(t, mongosHarness, upstream.Forwarded, top(c)) {
			t.Errorf("top-level %d did not mark the mongos", c)
		}
	}
	// ok:1 with a top-level 91 is not an error reply.
	if decide(t, mongosHarness, upstream.Forwarded, upstream.ReplyCodeInfo{HasTop: true, OKZero: false, Top: 91}) {
		t.Error("ok:1 marked")
	}
}

// FR-29 (AC-42 unit): a barrier reply is checked on its top-level code only,
// with the same mode rule.
func TestReplyCodesBarrier_AC42(t *testing.T) {
	for _, c := range []int32{10107, 91} {
		if !decide(t, rsHarness, upstream.Barrier, top(c)) {
			t.Errorf("replica-set barrier %d did not mark", c)
		}
	}
	for _, c := range fr35Codes {
		if decide(t, rsHarness, upstream.Barrier, wce(c)) {
			t.Errorf("a barrier writeConcernError %d marked", c)
		}
	}
	if !decide(t, mongosHarness, upstream.Barrier, top(91)) {
		t.Error("mongos barrier 91 did not mark")
	}
	if decide(t, mongosHarness, upstream.Barrier, top(10107)) {
		t.Error("mongos barrier 10107 marked")
	}
}

// FR-35: marking takes effect at once. Admission waits, readiness says
// unavailable, observers and changes report it, an immediate check follows no
// earlier than 500 ms after the previous one, and a confirming check restores
// the target.
func TestMarkingEffects_FR35(t *testing.T) {
	h := rsHarness(t)
	n := len(h.chk.of(A))
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")
	if !h.m.ReplyCodes(ctx, A, upstream.Forwarded, upstream.ReplyCodeInfo{HasTop: true, OKZero: true, Top: 10107, TopName: "NotWritablePrimary"}) {
		t.Fatal("not marked")
	}
	s := h.m.State()
	if s.HasTarget {
		t.Fatalf("target still known: %+v", s)
	}
	if !s.Reached {
		t.Fatal("Reached must stay true")
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission: %+v", a)
	}
	if st := h.readiness(); st.Ready || st.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("readiness: %+v", st)
	}
	// Observer: host, code and codeName, with the caller's context.
	m := h.rec.snapshotMarked()
	if len(m) != 1 || m[0].Host != A || m[0].Code != 10107 || m[0].CodeName != "NotWritablePrimary" {
		t.Fatalf("observer: %+v", m)
	}
	h.rec.mu.Lock()
	var gotCtx context.Context
	for _, c := range h.rec.ctxs {
		if c.Value(ctxKey{}) == "caller" {
			gotCtx = c
		}
	}
	h.rec.mu.Unlock()
	if gotCtx == nil {
		t.Fatal("the observer did not receive the caller's context")
	}
	found := false
	for _, c := range h.changeKinds() {
		if c.Kind == upstream.HostNotPrimary && c.Host == A {
			found = true
		}
	}
	if !found {
		t.Fatalf("no HostNotPrimary change: %+v", h.changeKinds())
	}
	// Immediate check: not before 500 ms after the previous one.
	h.quiet()
	if len(h.chk.of(A)) != n {
		t.Fatal("immediate check ran inside the 500 ms floor")
	}
	h.advance(500 * time.Millisecond)
	h.waitCount(A, n+1)
	// The check confirms a primary (A again): usable again.
	h.waitState("target confirmed", func(s upstream.State) bool { return s.HasTarget && s.Target == A })
	if a := h.m.Admission(); a.Kind != upstream.Ready {
		t.Fatalf("admission after confirmation: %+v", a)
	}
}

// A mongos marked unusable follows the same path (single-mongos mode).
func TestMarkingMongos_FR35(t *testing.T) {
	h := mongosHarness(t)
	if !h.m.ReplyCodes(context.Background(), A, upstream.Forwarded, top(11600)) {
		t.Fatal("not marked")
	}
	if h.m.State().HasTarget {
		t.Fatal("target still known")
	}
	if a := h.m.Admission(); a.Kind != upstream.Wait {
		t.Fatalf("admission: %+v", a)
	}
	h.advance(500 * time.Millisecond)
	h.waitState("usable again", func(s upstream.State) bool { return s.HasTarget })
}

// After the primary changes, the old primary is reported (HostNotPrimary) and
// the new target is reported (TargetChanged), and the observer sees the new
// primary.
func TestPrimaryChangeReported_FR35(t *testing.T) {
	setup := func(c *fakeChecker) {
		c.set(primaryAt(A, "rs0", 1, 1, A, B))
		c.set(secondary(B, "rs0", A, A, B))
	}
	h := newHarness(t, rsCfg(A), setup, false).start()
	h.waitState("A is the target", func(s upstream.State) bool { return s.Target == A })
	h.waitCount(B, 1)
	h.quiet()
	h.chk.set(secondary(A, "rs0", B, A, B))
	h.chk.set(primaryAt(B, "rs0", 2, 1, A, B))
	h.advance(10 * time.Second)
	h.waitState("B is the target", func(s upstream.State) bool { return s.HasTarget && s.Target == B })
	var changedTo, oldGone bool
	for _, c := range h.changeKinds() {
		if c.Kind == upstream.TargetChanged && c.Host == B {
			changedTo = true
		}
		if (c.Kind == upstream.HostNotPrimary || c.Kind == upstream.HostUnknown) && c.Host == A {
			oldGone = true
		}
	}
	if !changedTo || !oldGone {
		t.Fatalf("changes: %+v", h.changeKinds())
	}
	eventually(t, "observer primary", func() bool {
		h.rec.mu.Lock()
		defer h.rec.mu.Unlock()
		return len(h.rec.primary) >= 2 && h.rec.primary[len(h.rec.primary)-1] == B
	})
}

// AC-2 Part B: every result is the same with Noop and with a recording
// observer, and the recorder receives the caller's context.
func TestObserverEquivalence_AC2(t *testing.T) {
	scenario := func(noop bool) (string, *harness) {
		h := newHarness(t, rsCfg(A), healthyRS, noop).start()
		h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
		h.waitCount(B, 1)
		h.quiet()
		var out string
		marked := h.m.ReplyCodes(context.WithValue(context.Background(), ctxKey{}, "caller"), A, upstream.Forwarded, top(91))
		out += fmt.Sprintf("marked=%v state=%+v adm=%+v ", marked, h.m.State(), h.m.Admission())
		out += fmt.Sprintf("ready=%+v ", h.readiness())
		h.advance(500 * time.Millisecond)
		h.waitState("recovered", func(s upstream.State) bool { return s.HasTarget })
		out += fmt.Sprintf("after=%+v", h.m.State())
		return out, h
	}
	a, _ := scenario(true)
	b, h := scenario(false)
	if a != b {
		t.Fatalf("results differ:\n noop: %s\n rec:  %s", a, b)
	}
	if got := h.rec.snapshotMarked(); len(got) != 1 || got[0].Host != A || got[0].Code != 91 {
		t.Fatalf("recorder: %+v", got)
	}
}

// Observer callbacks run outside the monitor's lock: the recorder re-enters
// State, Readiness (see helpers) without deadlock, across availability flips.
func TestObserverCallbacksOutsideLock_AC2(t *testing.T) {
	h := newHarness(t, rsCfg(A), healthyRS, false).start()
	h.waitState("target", func(s upstream.State) bool { return s.HasTarget })
	h.m.ReplyCodes(context.Background(), A, upstream.Forwarded, top(91))
	h.advance(500 * time.Millisecond)
	h.waitState("back", func(s upstream.State) bool { return s.HasTarget })
	eventually(t, "availability callbacks", func() bool {
		h.rec.mu.Lock()
		defer h.rec.mu.Unlock()
		return len(h.rec.availOK) >= 2
	})
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	// The sequence of availability reports is ready=true, then not ready with
	// upstream_unavailable, then ready again.
	sawDown := false
	for i, ok := range h.rec.availOK {
		if !ok {
			sawDown = true
			if h.rec.avail[i] != health.ReasonUpstreamUnavailable {
				t.Fatalf("unavailable reported with reason %q", h.rec.avail[i])
			}
		}
	}
	if !sawDown {
		t.Fatal("no unavailable transition reported")
	}
}
