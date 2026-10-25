package upstream_test

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

// FR-22, AC-39: only the monitor reports authentication events for monitoring
// checks (the checker reports fields only). A failed authentication on the
// target's check is reported as an AuthFailed event of kind "monitor", and a
// successful one as AuthSucceeded.
func TestMonitorReportsAuthEvents_AC25(t *testing.T) {
	cfg := rsCfg(A)
	cfg.Authenticate = true
	bad := primary(A, "rs0", A)
	bad.AuthErr = &upstream.AuthFailed{Err: fakeServerErr{code: 18}}
	h := newHarness(t, cfg, func(c *fakeChecker) { c.set(bad) }, false).start()
	h.waitState("failing", func(s upstream.State) bool { return s.AuthFailing })
	h.rec.mu.Lock()
	if len(h.rec.authKO) < 1 || h.rec.authKO[0] != "monitor" {
		t.Fatalf("AuthFailed kinds: %v", h.rec.authKO)
	}
	if !upstream.IsAuthFailed(h.rec.authErrs[0]) {
		t.Fatalf("AuthFailed error: %v", h.rec.authErrs[0])
	}
	if len(h.rec.authOK) != 0 {
		t.Fatalf("AuthSucceeded during failure: %v", h.rec.authOK)
	}
	h.rec.mu.Unlock()

	h.chk.set(primary(A, "rs0", A))
	h.stepUntilState("cleared", func(s upstream.State) bool { return !s.AuthFailing })
	h.advance(time.Second)
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if len(h.rec.authOK) < 1 || h.rec.authOK[0] != "monitor" {
		t.Fatalf("AuthSucceeded kinds: %v", h.rec.authOK)
	}
}
