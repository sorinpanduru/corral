package metrics_test

import (
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// SPEC-001 FR-11 (Amendment 3), ADR-0004, AC-41: the outcome and cause labels
// hold non-error categories. No value of outcome, reason, state, side, opcode
// or kind may equal an FR-29 class or detail value. Each metric below is
// otherwise valid and small (under the FR-13 cap).

func lintLabel(t *testing.T, label string, values ...string) []metrics.Problem {
	t.Helper()
	c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
		Labels: []labelSpec{{Name: label, Values: values}}})
	return metrics.Lint(c)
}

var nonErrorLabels = []string{"outcome", "reason", "state", "side", "opcode", "kind"}

// The six names are accepted with ordinary values.
func TestLintAcceptsNewLabelNames_AC41(t *testing.T) {
	for _, l := range nonErrorLabels {
		t.Run(l, func(t *testing.T) {
			if ps := lintLabel(t, l, "ok", "proxy_error", "server_error"); len(ps) != 0 {
				t.Fatalf("unexpected problems:\n%s", problemText(ps))
			}
		})
	}
}

// Coarse "an error occurred" values are allowed as long as they are not FR-29
// values.
func TestLintAcceptsCoarseErrorValues_AC41(t *testing.T) {
	for _, l := range nonErrorLabels {
		if ps := lintLabel(t, l, "error", "failed", "server_error", "proxy_error", "protocol_error"); len(ps) != 0 {
			t.Errorf("%s: %s", l, problemText(ps))
		}
	}
}

// The examples from the plan, and every class and detail on every such label.
func TestLintRejectsClassAndDetailValues_AC41(t *testing.T) {
	examples := []struct{ label, value string }{
		{"outcome", "timeout"}, {"reason", "pool_queue_full"}, {"state", "eof"},
	}
	for _, e := range examples {
		t.Run(e.label+"="+e.value, func(t *testing.T) {
			txt := problemText(lintLabel(t, e.label, "ok", e.value))
			if !strings.Contains(txt, e.value) {
				t.Fatalf("not rejected, or value not named:\n%s", txt)
			}
		})
	}
	var forbidden []string
	forbidden = append(forbidden, classValues()...)
	forbidden = append(forbidden, detailValues()...)
	for _, l := range nonErrorLabels {
		for _, v := range forbidden {
			if v != strings.ToLower(v) {
				continue // uppercase errno details cannot be label values here anyway
			}
			if ps := lintLabel(t, l, "fine", v); len(ps) == 0 {
				t.Errorf("label %s accepted the FR-29 value %q", l, v)
			}
		}
	}
}

// The rule does not apply to error_class and error_detail themselves: they
// hold exactly those values. error_class includes rejected; error_detail
// includes every Amendment 3 detail.
func TestLintAcceptsErrorLabelsWithAmendment3Values_AC41(t *testing.T) {
	if ps := lintLabel(t, "error_class", classValues()...); len(ps) != 0 {
		t.Errorf("error_class: %s", problemText(ps))
	}
	if !contains(classValues(), "rejected") {
		t.Fatal("classes lack rejected")
	}
	if ps := lintLabel(t, "error_detail", detailValues()...); len(ps) != 0 {
		t.Errorf("error_detail: %s", problemText(ps))
	}
	for _, d := range []string{
		"pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed",
		"upstream_incompatible", "shutting_down", "client_auth_unsupported", "command_blocked",
		"handshake_rejected", "unsupported_opcode", "message_too_large", "malformed_message",
		"checksum_mismatch", "unexpected_response",
	} {
		if !contains(detailValues(), d) {
			t.Errorf("details lack %s", d)
		}
	}
}

// error_class still rejects values outside the enum.
func TestLintStillRejectsUnknownClass_AC41(t *testing.T) {
	if ps := lintLabel(t, "error_class", "timeout", "rejected_typo"); len(ps) == 0 {
		t.Fatal("unknown error_class value accepted")
	}
}

// Collisions are case-exact: label values must be lowercase snake anyway, so
// the only way to collide is an exact match; near misses are fine.
func TestLintNearMissesAreAccepted_AC41(t *testing.T) {
	if ps := lintLabel(t, "outcome", "timeouts", "timeout_exceeded", "pool_queue", "eof_seen"); len(ps) != 0 {
		t.Fatalf("near-miss values rejected:\n%s", problemText(ps))
	}
}

// Every lowercase value of the live enums is covered (the 14 original details
// with errno names are uppercase and so never valid on these labels).
func TestLintRuleCoversLiveEnums_AC41(t *testing.T) {
	n := 0
	for _, c := range errclass.Classes() {
		if ps := lintLabel(t, "kind", string(c)); len(ps) == 0 {
			t.Errorf("class %q accepted on kind", c)
		}
		n++
	}
	for _, d := range errclass.Details() {
		if string(d) != strings.ToLower(string(d)) {
			continue
		}
		if ps := lintLabel(t, "opcode", string(d)); len(ps) == 0 {
			t.Errorf("detail %q accepted on opcode", d)
		}
		n++
	}
	if n < 7+14 {
		t.Fatalf("checked only %d enum values", n)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
