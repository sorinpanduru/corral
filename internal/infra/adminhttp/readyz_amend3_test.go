package adminhttp_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
)

// SPEC-001 FR-18 (Amendment 3), AC-41: each new upstream reason is passed
// through as 503 with exactly that token, not mapped to unknown.
func TestReadyzUpstreamReasons_Amendment3(t *testing.T) {
	cases := map[health.Reason]string{
		health.ReasonUpstreamUnavailable:  "upstream_unavailable",
		health.ReasonUpstreamAuthFailed:   "upstream_auth_failed",
		health.ReasonUpstreamIncompatible: "upstream_incompatible",
	}
	for reason, token := range cases {
		t.Run(token, func(t *testing.T) {
			ready := &fakeReadiness{status: health.Status{Ready: false, Reason: reason}}
			r := start(t, ready, nil)
			r.srv.MarkStarted()
			expectReadyz(t, r, 503, token+"\n")
		})
	}
}
