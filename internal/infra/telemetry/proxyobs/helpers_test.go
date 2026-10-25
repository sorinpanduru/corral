package proxyobs_test

// Tests of the observer adapters (T18): domain observer calls in, SPEC-002
// metrics and FR-49 log records out. Logs go through logcheck (closed key set,
// components), metrics through a private registry built from the shipped
// catalog, time through the fake clock.
//
// Interface assumptions (stated in the report):
//   - AuthFailed(kind, host, err) on the upstream port counts both
//     connect_errors_total{kind, class} and auth_failures_total{kind}; a
//     failed pool open (pool port ConnOpenFailed) counts connect_errors only.
//     Tests that mix the two assert the subset invariant, not exact counts.
//   - AvailabilityChanged(false, reason) logs "upstream unavailable";
//     Available() logs "upstream available"; AvailabilityChanged sets the gauge.
//   - A pool CheckoutFailed with a detail other than pool_wait_timeout and
//     pool_queue_full produces no log record.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/proxyobs"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

var t0 = time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)

type rig struct {
	t   *testing.T
	ad  *proxyobs.Adapters
	lh  *logcheck.Handler
	reg *prometheus.Registry
	clk *fakeclock.Clock
	ctx context.Context
}

func newRig(t *testing.T) *rig {
	t.Helper()
	lh := logcheck.New(t)
	reg := metrics.NewRegistry()
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	mf, err := metrics.NewFactory(reg, cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(mf); err != nil {
		t.Fatal(err)
	}
	clk := fakeclock.New(t0)
	ad, err := proxyobs.New(logging.NewFactory(lh), mf, clk.Now)
	if err != nil {
		t.Fatalf("proxyobs.New: %v", err)
	}
	return &rig{t: t, ad: ad, lh: lh, reg: reg, clk: clk, ctx: context.Background()}
}

// records returns the captured records with the given msg.
func (r *rig) records(msg string) []logcheck.Record {
	var out []logcheck.Record
	for _, rec := range r.lh.Records() {
		if rec.Msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

func attrs(rec logcheck.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	for _, a := range rec.Attrs {
		m[a.Key] = a.Value.Resolve()
	}
	return m
}

func keysOf(m map[string]slog.Value) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func sameKeys(got map[string]slog.Value, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			return false
		}
	}
	return true
}

// one asserts exactly one record with msg, at level, from component, with
// exactly the given keys beyond component.
func (r *rig) one(msg string, level slog.Level, component string, keys ...string) map[string]slog.Value {
	r.t.Helper()
	recs := r.records(msg)
	if len(recs) != 1 {
		r.t.Fatalf("%q: %d records, want 1", msg, len(recs))
	}
	if recs[0].Level != level {
		r.t.Fatalf("%q: level %v, want %v", msg, recs[0].Level, level)
	}
	a := attrs(recs[0])
	if a["component"].String() != component {
		r.t.Fatalf("%q: component %q, want %q", msg, a["component"].String(), component)
	}
	want := append([]string{"component"}, keys...)
	if !sameKeys(a, want...) {
		r.t.Fatalf("%q: keys %v, want %v", msg, keysOf(a), want)
	}
	return a
}

func (r *rig) none(msg string) {
	r.t.Helper()
	if n := len(r.records(msg)); n != 0 {
		r.t.Fatalf("%q: %d records, want none", msg, n)
	}
}

// ---- metrics ----

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func (r *rig) family(name string) *dto.MetricFamily {
	r.t.Helper()
	fams, err := r.reg.Gather()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// val is the value of a counter or gauge series (0 when absent).
func (r *rig) val(name string, labels ...string) float64 {
	r.t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	f := r.family(name)
	if f == nil {
		return 0
	}
	for _, m := range f.GetMetric() {
		if labelsMatch(m, want) {
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// sum adds every series of a counter or gauge family.
func (r *rig) sum(name string, filter ...string) float64 {
	r.t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(filter); i += 2 {
		want[filter[i]] = filter[i+1]
	}
	f := r.family(name)
	if f == nil {
		return 0
	}
	total := 0.0
	for _, m := range f.GetMetric() {
		ok := true
		for k, v := range want {
			found := false
			for _, l := range m.GetLabel() {
				if l.GetName() == k && l.GetValue() == v {
					found = true
				}
			}
			ok = ok && found
		}
		if !ok {
			continue
		}
		switch {
		case m.GetCounter() != nil:
			total += m.GetCounter().GetValue()
		case m.GetGauge() != nil:
			total += m.GetGauge().GetValue()
		}
	}
	return total
}

// hist returns the observation count and sum of one histogram series.
func (r *rig) hist(name string, labels ...string) (uint64, float64) {
	r.t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	f := r.family(name)
	if f == nil {
		return 0, 0
	}
	for _, m := range f.GetMetric() {
		if labelsMatch(m, want) && m.GetHistogram() != nil {
			return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
		}
	}
	return 0, 0
}

func (r *rig) histCountAll(name string) uint64 {
	r.t.Helper()
	f := r.family(name)
	if f == nil {
		return 0
	}
	var n uint64
	for _, m := range f.GetMetric() {
		if m.GetHistogram() != nil {
			n += m.GetHistogram().GetSampleCount()
		}
	}
	return n
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func noMarker(t *testing.T, r *rig, m string) {
	t.Helper()
	for _, rec := range r.lh.Records() {
		if strings.Contains(rec.Msg, m) {
			t.Fatal("marker in msg")
		}
		for _, a := range rec.Attrs {
			if strings.Contains(a.Key, m) || strings.Contains(a.Value.String(), m) {
				t.Fatalf("marker in attribute %q", a.Key)
			}
		}
	}
}
