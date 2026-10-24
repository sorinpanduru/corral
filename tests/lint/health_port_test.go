package lint_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
)

type fakeReadiness struct{ st health.Status }

func (f fakeReadiness) Readiness(context.Context) health.Status { return f.st }

var _ health.Readiness = fakeReadiness{}

// FR-18: reason constants have the fixed wire strings (D6).
func TestFR18_ReasonValues(t *testing.T) {
	cases := map[health.Reason]string{
		health.ReasonStarting:     "starting",
		health.ReasonShuttingDown: "shutting_down",
		health.ReasonUnknown:      "unknown",
	}
	for r, want := range cases {
		if string(r) != want {
			t.Errorf("reason = %q, want %q", r, want)
		}
	}
}

// FR-18: Reasons() is the full defined set, without duplicates, and contains
// only short fixed lowercase strings.
func TestFR18_ReasonsSet(t *testing.T) {
	got := health.Reasons()
	seen := map[health.Reason]bool{}
	shape := regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
	for _, r := range got {
		if seen[r] {
			t.Errorf("duplicate reason %q", r)
		}
		seen[r] = true
		if !shape.MatchString(string(r)) {
			t.Errorf("reason %q is not a short fixed lowercase token", r)
		}
	}
	for _, want := range []health.Reason{health.ReasonStarting, health.ReasonShuttingDown, health.ReasonUnknown} {
		if !seen[want] {
			t.Errorf("Reasons() is missing %q", want)
		}
	}
}

// FR-18 / AC-13 (port shape): the port returns Ready and Reason through Status.
func TestFR18_ReadinessPortShape(t *testing.T) {
	var p health.Readiness = fakeReadiness{health.Status{Ready: true}}
	if s := p.Readiness(context.Background()); !s.Ready {
		t.Errorf("ready fake reported not ready: %+v", s)
	}
	p = fakeReadiness{health.Status{Ready: false, Reason: health.ReasonStarting}}
	if s := p.Readiness(context.Background()); s.Ready || s.Reason != health.ReasonStarting {
		t.Errorf("not-ready fake status = %+v", s)
	}
}

// FR-18: the zero Status is not ready.
func TestFR18_ZeroStatusNotReady(t *testing.T) {
	var s health.Status
	if s.Ready {
		t.Error("zero Status must not be ready")
	}
}
