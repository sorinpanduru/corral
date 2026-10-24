package metrics

import (
	"errors"
	"fmt"
	"slices"

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

// Factory creates metrics from catalog entries only. Exemplars are never
// attached (FR-11): the factory never calls the exemplar APIs.
type Factory struct {
	reg  prometheus.Registerer
	defs map[string]MetricDef
	list []MetricDef
}

// NewFactory binds a catalog to a registerer. It refuses a nil catalog or
// registerer and a catalog that fails Lint.
func NewFactory(reg prometheus.Registerer, c *Catalog) (*Factory, error) {
	if c == nil {
		return nil, errors.New("metrics: nil catalog")
	}
	if reg == nil {
		return nil, errors.New("metrics: nil registerer")
	}
	if ps := Lint(c); len(ps) > 0 {
		return nil, fmt.Errorf("metrics: catalog fails lint with %d problem(s), first: %s", len(ps), ps[0])
	}
	f := &Factory{reg: reg, defs: make(map[string]MetricDef, len(c.Metrics))}
	for _, m := range c.Metrics {
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

// register registers c, or returns the already registered equal collector.
func register[T prometheus.Collector](f *Factory, c T) (T, error) {
	if err := f.reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if ex, ok := are.ExistingCollector.(T); ok {
				return ex, nil
			}
		}
		var zero T
		return zero, fmt.Errorf("metrics: register: %w", err)
	}
	return c, nil
}

// Counter creates a label-less counter from the catalog.
func (f *Factory) Counter(name string) (prometheus.Counter, error) {
	m, err := f.lookup(name, TypeCounter, false)
	if err != nil {
		return nil, err
	}
	return register(f, prometheus.NewCounter(prometheus.CounterOpts{Name: m.Name, Help: m.Help}))
}

// Gauge creates a label-less gauge from the catalog.
func (f *Factory) Gauge(name string) (prometheus.Gauge, error) {
	m, err := f.lookup(name, TypeGauge, false)
	if err != nil {
		return nil, err
	}
	return register(f, prometheus.NewGauge(prometheus.GaugeOpts{Name: m.Name, Help: m.Help}))
}

// Histogram creates a label-less histogram from the catalog.
func (f *Factory) Histogram(name string) (prometheus.Histogram, error) {
	m, err := f.lookup(name, TypeHistogram, false)
	if err != nil {
		return nil, err
	}
	return register(f, prometheus.NewHistogram(prometheus.HistogramOpts{Name: m.Name, Help: m.Help, Buckets: buckets(m)}))
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
func (c *CounterVec) WithLabelValues(values ...string) (prometheus.Counter, error) {
	if err := c.k.check(values); err != nil {
		return nil, err
	}
	return c.v.GetMetricWithLabelValues(values...)
}

// GaugeVec is a catalog-checked gauge vector.
type GaugeVec struct {
	v *prometheus.GaugeVec
	k checker
}

// WithLabelValues returns the gauge for declared label values only.
func (g *GaugeVec) WithLabelValues(values ...string) (prometheus.Gauge, error) {
	if err := g.k.check(values); err != nil {
		return nil, err
	}
	return g.v.GetMetricWithLabelValues(values...)
}

// HistogramVec is a catalog-checked histogram vector.
type HistogramVec struct {
	v *prometheus.HistogramVec
	k checker
}

// WithLabelValues returns the observer for declared label values only.
func (h *HistogramVec) WithLabelValues(values ...string) (prometheus.Observer, error) {
	if err := h.k.check(values); err != nil {
		return nil, err
	}
	return h.v.GetMetricWithLabelValues(values...)
}

// CounterVec creates a counter vector from the catalog.
func (f *Factory) CounterVec(name string) (*CounterVec, error) {
	m, err := f.lookup(name, TypeCounter, true)
	if err != nil {
		return nil, err
	}
	v, err := register(f, prometheus.NewCounterVec(prometheus.CounterOpts{Name: m.Name, Help: m.Help}, labelNames(m)))
	if err != nil {
		return nil, err
	}
	return &CounterVec{v: v, k: checker{m.Name, m.Labels}}, nil
}

// GaugeVec creates a gauge vector from the catalog.
func (f *Factory) GaugeVec(name string) (*GaugeVec, error) {
	m, err := f.lookup(name, TypeGauge, true)
	if err != nil {
		return nil, err
	}
	v, err := register(f, prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: m.Name, Help: m.Help}, labelNames(m)))
	if err != nil {
		return nil, err
	}
	return &GaugeVec{v: v, k: checker{m.Name, m.Labels}}, nil
}

// HistogramVec creates a histogram vector from the catalog.
func (f *Factory) HistogramVec(name string) (*HistogramVec, error) {
	m, err := f.lookup(name, TypeHistogram, true)
	if err != nil {
		return nil, err
	}
	v, err := register(f, prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: m.Name, Help: m.Help, Buckets: buckets(m)}, labelNames(m)))
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
