package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// Expected vector wrapper API (catalog-checked, ADR-0004):
//   (*Factory).CounterVec(name)   -> wrapper with WithLabelValues(...string) (prometheus.Counter, error)
//   (*Factory).GaugeVec(name)     -> wrapper with WithLabelValues(...string) (prometheus.Gauge, error)
//   (*Factory).HistogramVec(name) -> wrapper with WithLabelValues(...string) (prometheus.Observer, error)
// Each constructor returns (value, error).

func fullCatalog(t *testing.T) *metrics.Catalog {
	return parse(t,
		metricSpec{Name: "corral_events_total", Type: "counter", Help: "Events."},
		metricSpec{Name: "corral_depth", Type: "gauge", Help: "Depth."},
		metricSpec{Name: "corral_wait_seconds", Type: "histogram", Help: "Wait.", Buckets: []float64{0.5, 5}},
		metricSpec{Name: "corral_ops_total", Type: "counter", Help: "Ops.",
			Labels: []labelSpec{{Name: "command", Values: []string{"find", "hello", "other"}}}},
		metricSpec{Name: "corral_slots", Type: "gauge", Help: "Slots.",
			Labels: []labelSpec{{Name: "state", Values: []string{"idle", "busy"}}}},
		metricSpec{Name: "corral_rtt_seconds", Type: "histogram", Help: "Round trip.",
			Labels: []labelSpec{{Name: "command", Values: []string{"find", "hello", "other"}}}},
		metricSpec{Name: "corral_failures_total", Type: "counter", Help: "Failures.",
			Labels: []labelSpec{{Name: "error_class", Values: classValues()}}},
	)
}

func newFactory(t *testing.T) (*metrics.Factory, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	f, err := metrics.NewFactory(reg, fullCatalog(t))
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	return f, reg
}

func familyNames(t *testing.T, g prometheus.Gatherer) map[string]*dto.MetricFamily {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

// FR-31: a plain counter takes type, help and name from the catalog.
func TestFactoryCounterFromCatalog(t *testing.T) {
	f, reg := newFactory(t)
	c, err := f.Counter("corral_events_total")
	if err != nil {
		t.Fatal(err)
	}
	c.Inc()
	c.Add(2)
	if got := testutil.ToFloat64(c); got != 3 {
		t.Fatalf("value = %v, want 3", got)
	}
	fam := familyNames(t, reg)["corral_events_total"]
	if fam == nil {
		t.Fatal("family not registered")
	}
	if fam.GetHelp() != "Events." || fam.GetType() != dto.MetricType_COUNTER {
		t.Errorf("help/type = %q/%v", fam.GetHelp(), fam.GetType())
	}
}

// FR-31, AC-8: the constructor refuses a name that is not in the catalog and
// registers nothing, for every constructor.
func TestFactoryRefusesUnlistedName(t *testing.T) {
	f, reg := newFactory(t)
	const bad = "corral_not_in_catalog_total"
	if _, err := f.Counter(bad); err == nil {
		t.Error("Counter accepted unlisted name")
	}
	if _, err := f.CounterVec(bad); err == nil {
		t.Error("CounterVec accepted unlisted name")
	}
	if _, err := f.Gauge("corral_not_in_catalog"); err == nil {
		t.Error("Gauge accepted unlisted name")
	}
	if _, err := f.GaugeVec("corral_not_in_catalog"); err == nil {
		t.Error("GaugeVec accepted unlisted name")
	}
	if _, err := f.Histogram("corral_not_in_catalog_seconds"); err == nil {
		t.Error("Histogram accepted unlisted name")
	}
	if _, err := f.HistogramVec("corral_not_in_catalog_seconds"); err == nil {
		t.Error("HistogramVec accepted unlisted name")
	}
	if _, err := f.Counter(""); err == nil {
		t.Error("Counter accepted empty name")
	}
	if n := len(familyNames(t, reg)); n != 0 {
		t.Fatalf("registry has %d families after refusals, want 0", n)
	}
}

// FR-31, AC-8: refusal against the real embedded catalog, including a
// look-alike of the one listed metric.
func TestFactoryEmbeddedCatalogRefusesUnlisted(t *testing.T) {
	c, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	f, err := metrics.NewFactory(reg, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"corral_tracing_spans_dropped", "corral_tracing_spans_dropped_total_", "corral_unknown_total", "go_goroutines"} {
		if _, err := f.Counter(name); err == nil {
			t.Errorf("Counter(%q) accepted", name)
		}
	}
	if _, err := f.Counter(metrics.NameTracingSpansDropped); err != nil {
		t.Fatalf("listed counter refused: %v", err)
	}
}

// ADR-0004: a type mismatch returns an error and registers nothing.
func TestFactoryRefusesTypeMismatch(t *testing.T) {
	f, reg := newFactory(t)
	if _, err := f.Gauge("corral_events_total"); err == nil {
		t.Error("Gauge accepted a counter entry")
	}
	if _, err := f.Counter("corral_depth"); err == nil {
		t.Error("Counter accepted a gauge entry")
	}
	if _, err := f.Histogram("corral_depth"); err == nil {
		t.Error("Histogram accepted a gauge entry")
	}
	if _, err := f.Counter("corral_wait_seconds"); err == nil {
		t.Error("Counter accepted a histogram entry")
	}
	// Labelled entry through the label-less constructor and vice versa.
	if _, err := f.Counter("corral_ops_total"); err == nil {
		t.Error("Counter accepted an entry that declares labels")
	}
	if _, err := f.CounterVec("corral_events_total"); err == nil {
		t.Error("CounterVec accepted an entry without labels")
	}
	if n := len(familyNames(t, reg)); n != 0 {
		t.Fatalf("registry has %d families after refusals, want 0", n)
	}
}

// ADR-0004: gauge and histogram constructors; histogram honours catalog buckets.
func TestFactoryGaugeAndHistogram(t *testing.T) {
	f, reg := newFactory(t)
	g, err := f.Gauge("corral_depth")
	if err != nil {
		t.Fatal(err)
	}
	g.Set(7)
	g.Dec()
	if got := testutil.ToFloat64(g); got != 6 {
		t.Fatalf("gauge = %v", got)
	}
	h, err := f.Histogram("corral_wait_seconds")
	if err != nil {
		t.Fatal(err)
	}
	h.Observe(1)
	fam := familyNames(t, reg)["corral_wait_seconds"]
	if fam == nil {
		t.Fatal("histogram not registered")
	}
	hist := fam.GetMetric()[0].GetHistogram()
	if hist.GetSampleCount() != 1 || len(hist.GetBucket()) != 2 {
		t.Fatalf("count=%d buckets=%d, want 1 and 2 (catalog buckets 0.5, 5)",
			hist.GetSampleCount(), len(hist.GetBucket()))
	}
	if hist.GetBucket()[0].GetUpperBound() != 0.5 || hist.GetBucket()[1].GetUpperBound() != 5 {
		t.Errorf("bucket bounds %v", hist.GetBucket())
	}
}

// ADR-0004: vectors accept declared label values.
func TestFactoryVectorsAcceptDeclaredValues(t *testing.T) {
	f, reg := newFactory(t)
	cv, err := f.CounterVec("corral_ops_total")
	if err != nil {
		t.Fatal(err)
	}
	c, err := cv.WithLabelValues("find")
	if err != nil {
		t.Fatalf("declared value refused: %v", err)
	}
	c.Inc()
	gv, err := f.GaugeVec("corral_slots")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gv.WithLabelValues("idle")
	if err != nil {
		t.Fatal(err)
	}
	g.Set(4)
	hv, err := f.HistogramVec("corral_rtt_seconds")
	if err != nil {
		t.Fatal(err)
	}
	o, err := hv.WithLabelValues("hello")
	if err != nil {
		t.Fatal(err)
	}
	o.Observe(0.2)
	fams := familyNames(t, reg)
	for _, n := range []string{"corral_ops_total", "corral_slots", "corral_rtt_seconds"} {
		if fams[n] == nil || len(fams[n].GetMetric()) != 1 {
			t.Errorf("%s: want 1 series", n)
		}
	}
}

// ADR-0004, FR-11: vectors refuse label values outside the declared set and
// the wrong number of values; no series is created by a refusal.
func TestFactoryVectorsRefuseUndeclaredValues(t *testing.T) {
	f, reg := newFactory(t)
	cv, _ := f.CounterVec("corral_ops_total")
	gv, _ := f.GaugeVec("corral_slots")
	hv, _ := f.HistogramVec("corral_rtt_seconds")
	ev, err := f.CounterVec("corral_failures_total")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cv.WithLabelValues("aggregate"); err == nil {
		t.Error("CounterVec accepted undeclared value")
	}
	if _, err := cv.WithLabelValues("Find"); err == nil {
		t.Error("CounterVec matched case-insensitively")
	}
	if _, err := cv.WithLabelValues(""); err == nil {
		t.Error("CounterVec accepted empty value")
	}
	if _, err := cv.WithLabelValues("find", "extra"); err == nil {
		t.Error("CounterVec accepted too many values")
	}
	if _, err := cv.WithLabelValues(); err == nil {
		t.Error("CounterVec accepted too few values")
	}
	if _, err := gv.WithLabelValues("zombie"); err == nil {
		t.Error("GaugeVec accepted undeclared value")
	}
	if _, err := hv.WithLabelValues("insert"); err == nil {
		t.Error("HistogramVec accepted undeclared value")
	}
	if _, err := ev.WithLabelValues("not_a_class"); err == nil {
		t.Error("error-class vec accepted value outside the class enum")
	}
	for name, fam := range familyNames(t, reg) {
		if len(fam.GetMetric()) != 0 {
			t.Errorf("%s has %d series after refused values", name, len(fam.GetMetric()))
		}
	}
}

// FR-11: no exemplars are attached, whatever the caller does through the
// factory-produced metrics.
func TestFactoryNeverAttachesExemplars(t *testing.T) {
	f, reg := newFactory(t)
	c, _ := f.Counter("corral_events_total")
	c.Inc()
	h, _ := f.Histogram("corral_wait_seconds")
	h.Observe(1)
	fams := familyNames(t, reg)
	if ex := fams["corral_events_total"].GetMetric()[0].GetCounter().GetExemplar(); ex != nil {
		t.Errorf("counter has exemplar %v", ex)
	}
	for _, b := range fams["corral_wait_seconds"].GetMetric()[0].GetHistogram().GetBucket() {
		if b.GetExemplar() != nil {
			t.Errorf("histogram bucket has exemplar")
		}
	}
}

// ADR-0004: NewFactory rejects a nil catalog or registerer.
func TestNewFactoryRejectsNil(t *testing.T) {
	if _, err := metrics.NewFactory(prometheus.NewRegistry(), nil); err == nil {
		t.Error("nil catalog accepted")
	}
	if _, err := metrics.NewFactory(nil, fullCatalog(t)); err == nil {
		t.Error("nil registerer accepted")
	}
}

// ADR-0004: startup lints the catalog, so a catalog with lint problems must
// not yield a working factory.
func TestNewFactoryRejectsCatalogThatFailsLint(t *testing.T) {
	c := parseFile(t, "bad_prefix.json")
	reg := prometheus.NewRegistry()
	f, err := metrics.NewFactory(reg, c)
	if err == nil {
		if _, cerr := f.Counter("pool_requests_total"); cerr == nil {
			t.Fatal("a metric outside the corral_ namespace was created")
		}
	}
	if n := len(familyNames(t, reg)); n != 0 {
		t.Fatalf("%d families registered", n)
	}
}

// FR-31, FR-37, AC-7 support: RegisterAll exposes every catalog metric at
// zero, including the spans-dropped counter, before anything increments it.
func TestRegisterAllExposesZeroValues(t *testing.T) {
	c, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	f, err := metrics.NewFactory(reg, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(f); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	fam := familyNames(t, reg)[metrics.NameTracingSpansDropped]
	if fam == nil {
		t.Fatal("corral_tracing_spans_dropped_total missing after RegisterAll")
	}
	if len(fam.GetMetric()) != 1 || fam.GetMetric()[0].GetCounter().GetValue() != 0 {
		t.Fatalf("want one zero-valued series, got %v", fam.GetMetric())
	}
	if len(fam.GetMetric()[0].GetLabel()) != 0 {
		t.Errorf("counter has labels %v (FR-37: none)", fam.GetMetric()[0].GetLabel())
	}
	if fam.GetHelp() != droppedHelp {
		t.Errorf("help = %q", fam.GetHelp())
	}
}

// RegisterAll over a richer catalog covers vectors: each family appears.
func TestRegisterAllCoversEveryType(t *testing.T) {
	reg := prometheus.NewRegistry()
	f, err := metrics.NewFactory(reg, fullCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(f); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	// Vectors have no children until used, so gathered output holds at least
	// the label-less metrics; none of the entries may be missing from the
	// catalog (checked in registry_test.go).
	fams := familyNames(t, reg)
	for _, n := range []string{"corral_events_total", "corral_depth", "corral_wait_seconds"} {
		if fams[n] == nil {
			t.Errorf("%s missing after RegisterAll", n)
		}
	}
}
