package metrics_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// FR-14, AC-7/AC-8 support: the registry carries the Go runtime and process
// collectors.
func TestNewRegistryHasGoAndProcessCollectors(t *testing.T) {
	reg := metrics.NewRegistry()
	if reg == nil {
		t.Fatal("NewRegistry returned nil")
	}
	fams := familyNames(t, reg)
	var goFam, procFam bool
	for n := range fams {
		goFam = goFam || strings.HasPrefix(n, "go_")
		procFam = procFam || strings.HasPrefix(n, "process_")
	}
	if !goFam {
		t.Error("no go_ metric family")
	}
	if fams["go_goroutines"] == nil {
		t.Error("go_goroutines missing")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if !procFam {
			t.Error("no process_ metric family")
		}
		if fams["process_cpu_seconds_total"] == nil {
			t.Error("process_cpu_seconds_total missing")
		}
	}
}

// FR-9: the registry is private, not the global default, and not shared.
func TestNewRegistryIsPrivate(t *testing.T) {
	a, b := metrics.NewRegistry(), metrics.NewRegistry()
	if a == b {
		t.Fatal("NewRegistry returned the same registry twice")
	}
	if a == prometheus.DefaultRegisterer || prometheus.Gatherer(a) == prometheus.DefaultGatherer {
		t.Fatal("NewRegistry returned the global default registry")
	}
	c, _ := metrics.LoadCatalog()
	fa, err := metrics.NewFactory(a, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(fa); err != nil {
		t.Fatal(err)
	}
	if familyNames(t, b)[metrics.NameTracingSpansDropped] != nil {
		t.Error("registration in one registry leaked into another")
	}
	if fams, _ := prometheus.DefaultGatherer.Gather(); func() bool {
		for _, f := range fams {
			if strings.HasPrefix(f.GetName(), "corral_") {
				return true
			}
		}
		return false
	}() {
		t.Error("corral_ metric leaked into the default registry")
	}
}

// Two registries can each be populated (no global state), proving the
// factory registers on the given registerer only.
func TestRegisterAllOnTwoRegistries(t *testing.T) {
	c, _ := metrics.LoadCatalog()
	for i := 0; i < 2; i++ {
		reg := metrics.NewRegistry()
		f, err := metrics.NewFactory(reg, c)
		if err != nil {
			t.Fatal(err)
		}
		if err := metrics.RegisterAll(f); err != nil {
			t.Fatalf("registry %d: %v", i, err)
		}
	}
}

func corralFamilies(t *testing.T, g prometheus.Gatherer) []*dto.MetricFamily {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []*dto.MetricFamily
	for _, f := range fams {
		if strings.HasPrefix(f.GetName(), "corral_") {
			out = append(out, f)
		}
	}
	return out
}

// AC-8: the live registry contains no corral_ family missing from the
// catalog; every catalog metric appears. Populated through the factory with
// a catalog covering every type.
func TestLiveRegistryCorralFamiliesAreInCatalog(t *testing.T) {
	cat := fullCatalog(t)
	reg := metrics.NewRegistry()
	f, err := metrics.NewFactory(reg, cat)
	if err != nil {
		t.Fatal(err)
	}
	populate(t, f)

	inCatalog := map[string]bool{}
	for _, m := range cat.Metrics {
		inCatalog[m.Name] = true
	}
	seen := map[string]bool{}
	for _, fam := range corralFamilies(t, reg) {
		if !inCatalog[fam.GetName()] {
			t.Errorf("registry has corral_ family %q not in the catalog", fam.GetName())
		}
		seen[fam.GetName()] = true
	}
	for n := range inCatalog {
		if !seen[n] {
			t.Errorf("catalog metric %q not gathered after use", n)
		}
	}
}

// AC-8: same check against the embedded catalog after RegisterAll.
func TestLiveRegistryEmbeddedCatalog(t *testing.T) {
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	f, err := metrics.NewFactory(reg, cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(f); err != nil {
		t.Fatal(err)
	}
	fams := corralFamilies(t, reg)
	// Label-less metrics and histograms appear without observations: the
	// spans-dropped counter and the 10 label-less SPEC-002 metrics. Labelled
	// families appear once a series exists (catalog_spec002_test.go).
	want := map[string]bool{
		metrics.NameTracingSpansDropped: true, "corral_client_connections": true, "corral_client_connections_accepted_total": true,
		"corral_client_auth_rejected_total": true, "corral_commands_blocked_total": true, "corral_requests_dropped_total": true,
		"corral_pool_max_connections": true, "corral_pool_waiters": true, "corral_pool_checkout_wait_seconds": true,
		"corral_upstream_available": true, "corral_upstream_primary_changes_total": true,
	}
	got := map[string]bool{}
	for _, f := range fams {
		got[f.GetName()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("corral_ families = %v, want %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Fatalf("family %s missing: %v", n, got)
		}
	}
}

// AC-8: promlint over the corral_ families only (go_ and process_ families
// are not ours to lint); the test fails on any reported problem.
func TestPromlintCorralFamilies(t *testing.T) {
	reg := metrics.NewRegistry()
	f, err := metrics.NewFactory(reg, fullCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	populate(t, f)
	lintFamilies(t, corralFamilies(t, reg), 7)

	// And the shipped catalog.
	cat, _ := metrics.LoadCatalog()
	reg2 := metrics.NewRegistry()
	f2, err := metrics.NewFactory(reg2, cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(f2); err != nil {
		t.Fatal(err)
	}
	// Label-less metrics (and histograms) appear without observations: the
	// spans-dropped counter plus 10 label-less SPEC-002 metrics. Labelled
	// families are linted in catalog_spec002_test.go.
	lintFamilies(t, corralFamilies(t, reg2), 11)
}

func lintFamilies(t *testing.T, fams []*dto.MetricFamily, want int) {
	t.Helper()
	if len(fams) != want {
		t.Fatalf("got %d corral_ families, want %d (the lint must not run on an empty set)", len(fams), want)
	}
	probs, err := promlint.NewWithMetricFamilies(fams).Lint()
	if err != nil {
		t.Fatalf("promlint: %v", err)
	}
	for _, p := range probs {
		t.Errorf("promlint: %s: %s", p.Metric, p.Text)
	}
}

// populate creates one series for every metric in fullCatalog.
func populate(t *testing.T, f *metrics.Factory) {
	t.Helper()
	c, err := f.Counter("corral_events_total")
	must(t, err)
	c.Inc()
	g, err := f.Gauge("corral_depth")
	must(t, err)
	g.Set(1)
	h, err := f.Histogram("corral_wait_seconds")
	must(t, err)
	h.Observe(1)
	cv, err := f.CounterVec("corral_ops_total")
	must(t, err)
	cc, err := cv.WithLabelValues("find")
	must(t, err)
	cc.Inc()
	gv, err := f.GaugeVec("corral_slots")
	must(t, err)
	gg, err := gv.WithLabelValues("find")
	must(t, err)
	gg.Set(2)
	hv, err := f.HistogramVec("corral_rtt_seconds")
	must(t, err)
	ho, err := hv.WithLabelValues("other")
	must(t, err)
	ho.Observe(0.1)
	ev, err := f.CounterVec("corral_failures_total")
	must(t, err)
	ec, err := ev.WithLabelValues("timeout")
	must(t, err)
	ec.Inc()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
