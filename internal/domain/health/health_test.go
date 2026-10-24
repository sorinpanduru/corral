package health_test

import (
	"sort"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
)

// SPEC-001 FR-18 (Amendment 3), AC-41: the readiness reason set is exactly
// the six values.
func TestReasonsExactSet_FR18(t *testing.T) {
	want := []string{
		"starting", "shutting_down", "unknown",
		"upstream_unavailable", "upstream_auth_failed", "upstream_incompatible",
	}
	var got []string
	for _, r := range health.Reasons() {
		got = append(got, string(r))
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Reasons() = %v, want %v", got, want)
		}
	}
}

func TestReasonConstantValues_FR18(t *testing.T) {
	cases := map[health.Reason]string{
		health.ReasonStarting:             "starting",
		health.ReasonShuttingDown:         "shutting_down",
		health.ReasonUnknown:              "unknown",
		health.ReasonUpstreamUnavailable:  "upstream_unavailable",
		health.ReasonUpstreamAuthFailed:   "upstream_auth_failed",
		health.ReasonUpstreamIncompatible: "upstream_incompatible",
	}
	for r, v := range cases {
		if string(r) != v {
			t.Errorf("reason constant = %q, want %q", r, v)
		}
	}
}

// The caller owns the slice: mutating it does not change later results.
func TestReasonsReturnsFreshSlice(t *testing.T) {
	a := health.Reasons()
	a[0] = "tampered"
	if health.Reasons()[0] == "tampered" {
		t.Fatal("Reasons() shares its backing array")
	}
}
