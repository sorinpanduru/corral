package upstream_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
)

func TestConstants(t *testing.T) {
	if upstream.MinHeartbeat != 500*time.Millisecond || upstream.WireMin != 21 || upstream.WireMax != 25 {
		t.Fatalf("constants: %v %d %d", upstream.MinHeartbeat, upstream.WireMin, upstream.WireMax)
	}
}

// FR-18 (AC-10): the wire-version overlap rule 21..25.
func TestWireAccepted_AC10(t *testing.T) {
	cases := []struct {
		min, max int32
		want     bool
	}{
		{6, 17, false}, {0, 20, false}, {26, 30, false}, {26, 26, false},
		{6, 27, true}, {21, 21, true}, {25, 30, true}, {21, 25, true}, {0, 21, true},
		{25, 25, true}, {0, 25, true}, {22, 24, true}, {0, 0, false}, {25, 26, true},
	}
	for _, tc := range cases {
		if got := upstream.WireAccepted(tc.min, tc.max); got != tc.want {
			t.Errorf("WireAccepted(%d, %d) = %v, want %v", tc.min, tc.max, got, tc.want)
		}
	}
}

// FR-46 (AC-25): readiness over every combination, with the precedence
// incompatible > auth failing > unavailable, and exactly one reason.
func TestEvaluateReadinessCombinations_AC25(t *testing.T) {
	bools := []bool{false, true}
	n := 0
	for _, reached := range bools {
		for _, incompat := range bools {
			for _, hasTarget := range bools {
				for _, authFailing := range bools {
					s := upstream.State{Reached: reached, Incompatible: incompat, HasTarget: hasTarget, AuthFailing: authFailing}
					got := upstream.EvaluateReadiness(s)
					n++
					ready := reached && !incompat && hasTarget && !authFailing
					if got.Ready != ready {
						t.Fatalf("%+v: Ready = %v, want %v", s, got.Ready, ready)
					}
					if ready {
						if got.Reason != "" {
							t.Fatalf("%+v: ready with reason %q", s, got.Reason)
						}
						continue
					}
					var want health.Reason
					switch {
					case incompat:
						want = health.ReasonUpstreamIncompatible
					case authFailing:
						want = health.ReasonUpstreamAuthFailed
					default:
						want = health.ReasonUpstreamUnavailable
					}
					if got.Reason != want {
						t.Fatalf("%+v: reason %q, want %q", s, got.Reason, want)
					}
					in := false
					for _, r := range health.Reasons() {
						in = in || r == got.Reason
					}
					if !in {
						t.Fatalf("reason %q is outside the readiness set", got.Reason)
					}
				}
			}
		}
	}
	if n != 16 {
		t.Fatalf("combinations = %d", n)
	}
}

// The zero State is not ready, with upstream_unavailable.
func TestEvaluateReadinessZero(t *testing.T) {
	got := upstream.EvaluateReadiness(upstream.State{})
	if got.Ready || got.Reason != health.ReasonUpstreamUnavailable {
		t.Fatalf("zero state: %+v", got)
	}
}

type fakeServerErr struct{ code int32 }

func (e fakeServerErr) Error() string          { return "synthetic server error" }
func (e fakeServerErr) ServerCode() int32      { return e.code }
func (e fakeServerErr) ServerCodeName() string { return "Synthetic" }

// FR-22: AuthFailed carries the detail, unwraps to its cause and is found
// through wrapping; other errors are not auth failures.
func TestAuthFailedType_FR22(t *testing.T) {
	cause := fakeServerErr{code: 18}
	e := &upstream.AuthFailed{Err: cause}
	if e.CorralErrorDetail() != "upstream_auth_failed" {
		t.Fatalf("detail %q", e.CorralErrorDetail())
	}
	if !errors.Is(e, cause) || errors.Unwrap(e) != error(cause) {
		t.Fatal("AuthFailed does not unwrap to its cause")
	}
	if e.Error() == "" {
		t.Fatal("empty error text")
	}
	if !upstream.IsAuthFailed(e) || !upstream.IsAuthFailed(fmt.Errorf("outer: %w", e)) {
		t.Fatal("IsAuthFailed missed an AuthFailed")
	}
	for _, other := range []error{nil, errors.New("x"), netErr(), fakeServerErr{code: 10107}, fakeServerErr{code: 18}, fakeServerErr{code: 91}} {
		if upstream.IsAuthFailed(other) {
			t.Fatalf("IsAuthFailed(%v) = true", other)
		}
	}
}
