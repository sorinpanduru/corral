package metrics_test

// SPEC-002 FR-48 (catalog part of AC-27): the 23 metrics of "Metrics added by
// this spec", copied from the spec table. The table below is the oracle: type,
// label names, label value sets and maximum series per metric. Metric name
// constants are not used here (the plan names them without spelling them), so
// the test depends only on the catalog content.

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

type wantLabel struct {
	name   string
	values []string
}

type wantMetric struct {
	name   string
	typ    string
	labels []wantLabel
	series int
}

func classes() []string { return classValues() }

func commandValues() []string {
	return append(sanitize.CommandAllowlist(), sanitize.OtherCommand)
}

var spec002 = []wantMetric{
	{"corral_client_connections", "gauge", nil, 1},
	{"corral_client_connections_accepted_total", "counter", nil, 1},
	{"corral_client_connections_closed_total", "counter", []wantLabel{{"reason", []string{"client_closed", "idle_timeout", "io_timeout", "protocol_error", "upstream_failure", "shutdown", "tls_failure", "limit_exceeded", "internal_error"}}}, 9},
	{"corral_client_auth_rejected_total", "counter", nil, 1},
	{"corral_commands_blocked_total", "counter", nil, 1},
	{"corral_handshakes_total", "counter", []wantLabel{{"opcode", []string{"op_msg", "op_query"}}, {"outcome", []string{"ok", "error"}}}, 4},
	{"corral_protocol_errors_total", "counter", []wantLabel{{"side", []string{"client", "upstream"}}, {"error_detail", []string{"unsupported_opcode", "message_too_large", "malformed_message", "checksum_mismatch", "unexpected_response"}}}, 10},
	{"corral_requests_total", "counter", []wantLabel{{"command", nil}, {"outcome", []string{"ok", "server_error", "proxy_error"}}}, 84},
	{"corral_request_duration_seconds", "histogram", []wantLabel{{"command", nil}}, 28},
	{"corral_request_errors_total", "counter", []wantLabel{{"error_class", nil}}, 7},
	{"corral_requests_dropped_total", "counter", nil, 1},
	{"corral_pool_connections", "gauge", []wantLabel{{"state", []string{"idle", "in_use", "connecting"}}}, 3},
	{"corral_pool_max_connections", "gauge", nil, 1},
	{"corral_pool_waiters", "gauge", nil, 1},
	{"corral_pool_checkouts_total", "counter", []wantLabel{{"outcome", []string{"ok", "failed"}}}, 2},
	{"corral_pool_checkout_failures_total", "counter", []wantLabel{{"error_detail", []string{"pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down"}}}, 6},
	{"corral_pool_checkout_wait_seconds", "histogram", nil, 1},
	{"corral_upstream_connections_opened_total", "counter", []wantLabel{{"kind", []string{"pool", "monitor"}}}, 2},
	{"corral_upstream_connections_closed_total", "counter", []wantLabel{{"kind", []string{"pool", "monitor"}}, {"reason", []string{"idle", "error", "not_primary", "client_abandoned", "shutdown", "incompatible"}}}, 12},
	{"corral_upstream_connect_errors_total", "counter", []wantLabel{{"kind", []string{"pool", "monitor"}}, {"error_class", nil}}, 14},
	{"corral_upstream_auth_failures_total", "counter", []wantLabel{{"kind", []string{"pool", "monitor"}}}, 2},
	{"corral_upstream_available", "gauge", nil, 1},
	{"corral_upstream_primary_changes_total", "counter", nil, 1},
}

func shipped(t *testing.T) *metrics.Catalog {
	t.Helper()
	c, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func find(c *metrics.Catalog, name string) *metrics.MetricDef {
	for i := range c.Metrics {
		if c.Metrics[i].Name == name {
			return &c.Metrics[i]
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return len(m) == len(a)
}

func TestSpec002MetricCount_FR48(t *testing.T) {
	if len(spec002) != 23 {
		t.Fatalf("test oracle has %d metrics, want 23", len(spec002))
	}
	c := shipped(t)
	if len(c.Metrics) != 24 {
		t.Fatalf("catalog has %d metrics, want 24", len(c.Metrics))
	}
}

// Every SPEC-002 metric: exact type, label names (in order) and value sets.
func TestSpec002Definitions_FR48(t *testing.T) {
	c := shipped(t)
	for _, w := range spec002 {
		t.Run(w.name, func(t *testing.T) {
			m := find(c, w.name)
			if m == nil {
				t.Fatal("missing from the catalog")
			}
			if string(m.Type) != w.typ {
				t.Errorf("type = %q, want %q", m.Type, w.typ)
			}
			if strings.TrimSpace(m.Help) == "" {
				t.Error("empty help")
			}
			if len(m.Labels) != len(w.labels) {
				t.Fatalf("labels = %d, want %d", len(m.Labels), len(w.labels))
			}
			series := 1
			for i, wl := range w.labels {
				got := m.Labels[i]
				if got.Name != wl.name {
					t.Fatalf("label %d = %q, want %q", i, got.Name, wl.name)
				}
				want := wl.values
				switch wl.name {
				case "command":
					want = commandValues()
				case "error_class":
					want = classes()
				}
				if !sameSet(got.Values, want) {
					t.Errorf("%s values = %v, want %v", wl.name, got.Values, want)
				}
				series *= len(got.Values)
			}
			if series != w.series {
				t.Errorf("series = %d, want %d", series, w.series)
			}
			if series > 100 {
				t.Errorf("series %d exceeds the FR-13 cap", series)
			}
		})
	}
}

// corral_requests_total is the largest family: 28 commands x 3 outcomes = 84.
func TestRequestsTotalHas84Series_FR13(t *testing.T) {
	m := find(shipped(t), "corral_requests_total")
	if m == nil {
		t.Fatal("missing")
	}
	n := 1
	for _, l := range m.Labels {
		n *= len(l.Values)
	}
	if n != 84 {
		t.Fatalf("series = %d, want 84", n)
	}
}

// ADR-0016: histogram buckets.
func TestSpec002Buckets_ADR0016(t *testing.T) {
	c := shipped(t)
	cases := map[string][]float64{
		"corral_request_duration_seconds":   {0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		"corral_pool_checkout_wait_seconds": {0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}
	for name, want := range cases {
		m := find(c, name)
		if m == nil {
			t.Fatalf("%s missing", name)
		}
		if len(m.Buckets) != len(want) {
			t.Fatalf("%s: %d buckets, want %d", name, len(m.Buckets), len(want))
		}
		for i := range want {
			if m.Buckets[i] != want[i] {
				t.Errorf("%s bucket %d = %v, want %v", name, i, m.Buckets[i], want[i])
			}
		}
	}
}

// SPEC-001 AC-8, AC-10 (Part B rerun): the shipped catalog lints clean, and
// no non-error label value equals a class or detail (FR-11).
func TestShippedCatalogLintsClean_AC8(t *testing.T) {
	c := shipped(t)
	if ps := metrics.Lint(c); len(ps) != 0 {
		t.Fatalf("Lint problems:\n%s", problemText(ps))
	}
	forbidden := map[string]bool{}
	for _, v := range classValues() {
		forbidden[v] = true
	}
	for _, v := range detailValues() {
		forbidden[v] = true
	}
	for _, m := range c.Metrics {
		for _, l := range m.Labels {
			switch l.Name {
			case "outcome", "reason", "state", "side", "opcode", "kind":
				for _, v := range l.Values {
					if forbidden[v] {
						t.Errorf("%s{%s=%q} collides with an FR-29 value", m.Name, l.Name, v)
					}
				}
			}
		}
	}
	// Names use only the nine allowed label names, none forbidden.
	allowed := map[string]bool{}
	for _, n := range metrics.AllowedLabelNames() {
		allowed[n] = true
	}
	for _, m := range c.Metrics {
		for _, l := range m.Labels {
			if !allowed[l.Name] {
				t.Errorf("%s uses the label name %q", m.Name, l.Name)
			}
		}
	}
}

// Amendment 1 definitions are stated in the help text (normative wording is
// the planner's; these are the load-bearing words).
func TestHelpStatesDefinitions_Amendment1(t *testing.T) {
	c := shipped(t)
	need := map[string][]string{
		"corral_request_errors_total":              {"proxy_error"},
		"corral_client_connections_accepted_total": {"limit"},
		"corral_client_connections":                {"limit"},
		"corral_upstream_connect_errors_total":     {"authentication"},
		"corral_upstream_auth_failures_total":      {"upstream_auth_failed"},
		"corral_protocol_errors_total":             {"clos"},
		"corral_requests_dropped_total":            {"moreToCome"},
	}
	for name, words := range need {
		m := find(c, name)
		if m == nil {
			t.Fatalf("%s missing", name)
		}
		h := strings.ToLower(m.Help)
		for _, w := range words {
			if !strings.Contains(h, strings.ToLower(w)) {
				t.Errorf("%s help lacks %q: %q", name, w, m.Help)
			}
		}
	}
	// request_errors: server_error requests are not counted.
	if m := find(c, "corral_request_errors_total"); m != nil && !strings.Contains(strings.ToLower(m.Help), "server_error") {
		t.Errorf("corral_request_errors_total help does not say server_error requests are not counted: %q", m.Help)
	}
}

// AC-8: promlint over every SPEC-002 family, with a series created for each
// label combination.
func TestSpec002PromlintAllFamilies_AC8(t *testing.T) {
	reg := metrics.NewRegistry()
	f, err := metrics.NewFactory(reg, shipped(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(f); err != nil {
		t.Fatal(err)
	}
	for _, w := range spec002 {
		combos := [][]string{nil}
		for _, l := range w.labels {
			vals := l.values
			switch l.name {
			case "command":
				vals = commandValues()
			case "error_class":
				vals = classes()
			}
			var next [][]string
			for _, c := range combos {
				for _, v := range vals {
					next = append(next, append(append([]string(nil), c...), v))
				}
			}
			combos = next
		}
		for _, vals := range combos {
			switch {
			case len(w.labels) == 0 && w.typ == "counter":
				c, err := f.Counter(w.name)
				must(t, err)
				c.Inc()
			case len(w.labels) == 0 && w.typ == "gauge":
				g, err := f.Gauge(w.name)
				must(t, err)
				g.Set(1)
			case len(w.labels) == 0:
				h, err := f.Histogram(w.name)
				must(t, err)
				h.Observe(0.5)
			case w.typ == "counter":
				v, err := f.CounterVec(w.name)
				must(t, err)
				c, err := v.WithLabelValues(vals...)
				must(t, err)
				c.Inc()
			case w.typ == "gauge":
				v, err := f.GaugeVec(w.name)
				must(t, err)
				g, err := v.WithLabelValues(vals...)
				must(t, err)
				g.Set(1)
			default:
				v, err := f.HistogramVec(w.name)
				must(t, err)
				h, err := v.WithLabelValues(vals...)
				must(t, err)
				h.Observe(0.5)
			}
		}
	}
	fams := corralFamilies(t, reg)
	if len(fams) != 24 {
		t.Fatalf("%d corral_ families, want 24", len(fams))
	}
	lintFamilies(t, fams, 24)
	// The series count per family matches the declared maximum.
	byName := map[string]int{}
	for _, fam := range fams {
		byName[fam.GetName()] = len(fam.GetMetric())
	}
	for _, w := range spec002 {
		if byName[w.name] != w.series {
			t.Errorf("%s has %d series, want %d", w.name, byName[w.name], w.series)
		}
	}
}

// A value outside a declared set is refused by the factory (closed sets).
func TestSpec002ClosedValueSets_FR11(t *testing.T) {
	f, err := metrics.NewFactory(metrics.NewRegistry(), shipped(t))
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.CounterVec("corral_requests_total")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"find", "timeout"}, {"Find", "ok"}, {"find", "error"}} {
		if _, err := v.WithLabelValues(bad...); err == nil {
			t.Errorf("value set %v accepted", bad)
		}
	}
	if _, err := v.WithLabelValues("GetMore", "ok"); err == nil {
		t.Error("an unmapped command value was accepted (must be other)")
	}
	if _, err := v.WithLabelValues("other", "proxy_error"); err != nil {
		t.Errorf("valid values refused: %v", err)
	}
	d, _ := f.CounterVec("corral_pool_checkout_failures_total")
	if _, err := d.WithLabelValues("canceled"); err == nil {
		t.Error("checkout failure detail outside the six accepted")
	}
	e, _ := f.CounterVec("corral_request_errors_total")
	for _, c := range errclass.Classes() {
		if _, err := e.WithLabelValues(string(c)); err != nil {
			t.Errorf("class %q refused: %v", c, err)
		}
	}
}

var _ = prometheus.NewRegistry
