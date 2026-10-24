package metrics

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry returns a private registry (never the global default, FR-9)
// with the Go runtime and process collectors registered (FR-14).
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Counter is a catalog-created counter. It deliberately exposes only Inc and
// Add (plus Collector, so tests can read it), so it cannot attach exemplars
// (FR-11): it does not implement prometheus.ExemplarAdder.
type Counter struct{ c prometheus.Counter }

// Inc adds 1.
func (c *Counter) Inc() { c.c.Inc() }

// Add adds v (must be >= 0).
func (c *Counter) Add(v float64) { c.c.Add(v) }

// Describe implements prometheus.Collector.
func (c *Counter) Describe(ch chan<- *prometheus.Desc) { c.c.Describe(ch) }

// Collect implements prometheus.Collector.
func (c *Counter) Collect(ch chan<- prometheus.Metric) { c.c.Collect(ch) }

// Gauge is a catalog-created gauge exposing only Set, Inc, Dec, Add and Sub.
type Gauge struct{ g prometheus.Gauge }

// Set sets the value.
func (g *Gauge) Set(v float64) { g.g.Set(v) }

// Inc adds 1.
func (g *Gauge) Inc() { g.g.Inc() }

// Dec subtracts 1.
func (g *Gauge) Dec() { g.g.Dec() }

// Add adds v.
func (g *Gauge) Add(v float64) { g.g.Add(v) }

// Sub subtracts v.
func (g *Gauge) Sub(v float64) { g.g.Sub(v) }

// Describe implements prometheus.Collector.
func (g *Gauge) Describe(ch chan<- *prometheus.Desc) { g.g.Describe(ch) }

// Collect implements prometheus.Collector.
func (g *Gauge) Collect(ch chan<- prometheus.Metric) { g.g.Collect(ch) }

// Histogram is a catalog-created histogram exposing only Observe. It does not
// implement prometheus.ExemplarObserver.
type Histogram struct{ o prometheus.Observer }

// Observe records v.
func (h *Histogram) Observe(v float64) { h.o.Observe(v) }

// Factory creates metrics from catalog entries only. It works on a private
// deep copy of the catalog. Exemplars are unreachable (FR-11): handed-out
// values are thin wrappers that do not expose the exemplar APIs.
type Factory struct {
	reg  prometheus.Registerer
	defs map[string]MetricDef
	list []MetricDef

	mu    sync.Mutex
	cache map[string]prometheus.Collector // collectors this factory created, by name
}

func copyCatalog(c *Catalog) *Catalog {
	out := &Catalog{Metrics: make([]MetricDef, len(c.Metrics))}
	for i, m := range c.Metrics {
		m.Buckets = slices.Clone(m.Buckets)
		if m.Labels != nil {
			labels := make([]LabelDef, len(m.Labels))
			for j, l := range m.Labels {
				l.Values = slices.Clone(l.Values)
				labels[j] = l
			}
			m.Labels = labels
		}
		out.Metrics[i] = m
	}
	return out
}

// NewFactory binds a catalog to a registerer. It refuses a nil catalog or
// registerer and a catalog that fails Lint, and keeps a deep copy of the
// catalog so later caller mutation has no effect.
func NewFactory(reg prometheus.Registerer, c *Catalog) (*Factory, error) {
	if c == nil {
		return nil, errors.New("metrics: nil catalog")
	}
	if reg == nil {
		return nil, errors.New("metrics: nil registerer")
	}
	cc := copyCatalog(c)
	if ps := Lint(cc); len(ps) > 0 {
		return nil, fmt.Errorf("metrics: catalog fails lint with %d problem(s), first: %s", len(ps), ps[0])
	}
	f := &Factory{reg: reg, defs: make(map[string]MetricDef, len(cc.Metrics)), cache: map[string]prometheus.Collector{}}
	for _, m := range cc.Metrics {
		f.defs[m.Name] = m
		f.list = append(f.list, m)
	}
	return f, nil
}

func (f *Factory) lookup(name string, t Type, vec bool) (MetricDef, error) {
	m, ok := f.defs[name]
	if !ok {
		return MetricDef{}, fmt.Errorf("metrics: %q is not in the catalog", name)
	}
	if m.Type != t {
		return MetricDef{}, fmt.Errorf("metrics: %q is a %s, not a %s", name, m.Type, t)
	}
	if vec && len(m.Labels) == 0 {
		return MetricDef{}, fmt.Errorf("metrics: %q declares no labels, use the non-vector constructor", name)
	}
	if !vec && len(m.Labels) > 0 {
		return MetricDef{}, fmt.Errorf("metrics: %q declares labels, use the vector constructor", name)
	}
	return m, nil
}

func labelNames(m MetricDef) []string {
	out := make([]string, len(m.Labels))
	for i, l := range m.Labels {
		out[i] = l.Name
	}
	return out
}

func buckets(m MetricDef) []float64 {
	if len(m.Buckets) == 0 {
		return prometheus.DefBuckets
	}
	return m.Buckets
}

// obtain returns the collector the factory already created for name, or
// builds, registers and caches a new one. A registration clash with a
// collector the factory did not create is an error.
func obtain[T prometheus.Collector](f *Factory, name string, build func() T) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ex, ok := f.cache[name]; ok {
		if t, ok := ex.(T); ok {
			return t, nil
		}
	}
	c := build()
	if err := f.reg.Register(c); err != nil {
		var zero T
		return zero, fmt.Errorf("metrics: register %q: %w", name, err)
	}
	f.cache[name] = c
	return c, nil
}

// Counter creates (or returns the cached) label-less counter from the catalog.
func (f *Factory) Counter(name string) (*Counter, error) {
	m, err := f.lookup(name, TypeCounter, false)
	if err != nil {
		return nil, err
	}
	c, err := obtain(f, name, func() prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Name: m.Name, Help: m.Help})
	})
	if err != nil {
		return nil, err
	}
	return &Counter{c}, nil
}

// Gauge creates (or returns the cached) label-less gauge from the catalog.
func (f *Factory) Gauge(name string) (*Gauge, error) {
	m, err := f.lookup(name, TypeGauge, false)
	if err != nil {
		return nil, err
	}
	g, err := obtain(f, name, func() prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Name: m.Name, Help: m.Help})
	})
	if err != nil {
		return nil, err
	}
	return &Gauge{g}, nil
}

// Histogram creates (or returns the cached) label-less histogram.
func (f *Factory) Histogram(name string) (*Histogram, error) {
	m, err := f.lookup(name, TypeHistogram, false)
	if err != nil {
		return nil, err
	}
	h, err := obtain(f, name, func() prometheus.Histogram {
		return prometheus.NewHistogram(prometheus.HistogramOpts{Name: m.Name, Help: m.Help, Buckets: buckets(m)})
	})
	if err != nil {
		return nil, err
	}
	return &Histogram{h}, nil
}

// checker validates label values against the catalog declaration.
type checker struct {
	name   string
	labels []LabelDef
}

func (k checker) check(values []string) error {
	if len(values) != len(k.labels) {
		return fmt.Errorf("metrics: %q takes %d label value(s), got %d", k.name, len(k.labels), len(values))
	}
	for i, v := range values {
		if !slices.Contains(k.labels[i].Values, v) {
			return fmt.Errorf("metrics: %q label %q: value is not in the declared set", k.name, k.labels[i].Name)
		}
	}
	return nil
}

// CounterVec is a catalog-checked counter vector.
type CounterVec struct {
	v *prometheus.CounterVec
	k checker
}

// WithLabelValues returns the counter for declared label values only.
func (c *CounterVec) WithLabelValues(values ...string) (*Counter, error) {
	if err := c.k.check(values); err != nil {
		return nil, err
	}
	ch, err := c.v.GetMetricWithLabelValues(values...)
	if err != nil {
		return nil, err
	}
	return &Counter{ch}, nil
}

// GaugeVec is a catalog-checked gauge vector.
type GaugeVec struct {
	v *prometheus.GaugeVec
	k checker
}

// WithLabelValues returns the gauge for declared label values only.
func (g *GaugeVec) WithLabelValues(values ...string) (*Gauge, error) {
	if err := g.k.check(values); err != nil {
		return nil, err
	}
	ch, err := g.v.GetMetricWithLabelValues(values...)
	if err != nil {
		return nil, err
	}
	return &Gauge{ch}, nil
}

// HistogramVec is a catalog-checked histogram vector.
type HistogramVec struct {
	v *prometheus.HistogramVec
	k checker
}

// WithLabelValues returns the histogram for declared label values only.
func (h *HistogramVec) WithLabelValues(values ...string) (*Histogram, error) {
	if err := h.k.check(values); err != nil {
		return nil, err
	}
	ch, err := h.v.GetMetricWithLabelValues(values...)
	if err != nil {
		return nil, err
	}
	return &Histogram{ch}, nil
}

// CounterVec creates (or returns the cached) counter vector from the catalog.
func (f *Factory) CounterVec(name string) (*CounterVec, error) {
	m, err := f.lookup(name, TypeCounter, true)
	if err != nil {
		return nil, err
	}
	v, err := obtain(f, name, func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: m.Name, Help: m.Help}, labelNames(m))
	})
	if err != nil {
		return nil, err
	}
	return &CounterVec{v: v, k: checker{m.Name, m.Labels}}, nil
}

// GaugeVec creates (or returns the cached) gauge vector from the catalog.
func (f *Factory) GaugeVec(name string) (*GaugeVec, error) {
	m, err := f.lookup(name, TypeGauge, true)
	if err != nil {
		return nil, err
	}
	v, err := obtain(f, name, func() *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: m.Name, Help: m.Help}, labelNames(m))
	})
	if err != nil {
		return nil, err
	}
	return &GaugeVec{v: v, k: checker{m.Name, m.Labels}}, nil
}

// HistogramVec creates (or returns the cached) histogram vector.
func (f *Factory) HistogramVec(name string) (*HistogramVec, error) {
	m, err := f.lookup(name, TypeHistogram, true)
	if err != nil {
		return nil, err
	}
	v, err := obtain(f, name, func() *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: m.Name, Help: m.Help, Buckets: buckets(m)}, labelNames(m))
	})
	if err != nil {
		return nil, err
	}
	return &HistogramVec{v: v, k: checker{m.Name, m.Labels}}, nil
}

// RegisterAll registers every catalog metric so label-less metrics appear at
// /metrics with zero value (AC-7).
func RegisterAll(f *Factory) error {
	if f == nil {
		return errors.New("metrics: nil factory")
	}
	for _, m := range f.list {
		var err error
		vec := len(m.Labels) > 0
		switch {
		case m.Type == TypeCounter && vec:
			_, err = f.CounterVec(m.Name)
		case m.Type == TypeCounter:
			_, err = f.Counter(m.Name)
		case m.Type == TypeGauge && vec:
			_, err = f.GaugeVec(m.Name)
		case m.Type == TypeGauge:
			_, err = f.Gauge(m.Name)
		case m.Type == TypeHistogram && vec:
			_, err = f.HistogramVec(m.Name)
		default:
			_, err = f.Histogram(m.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
