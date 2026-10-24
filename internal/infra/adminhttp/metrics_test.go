package adminhttp_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http/httptest"
)

const openMetricsAccept = "application/openmetrics-text; version=1.0.0; charset=utf-8,text/plain;version=0.0.4;q=0.5"

// FR-9, AC-7: text format by default, with the catalog counter and the go_ and
// process_ families.
func TestMetricsDefaultTextFormat(t *testing.T) {
	r := start(t, nil, nil)
	code, hdr, body := r.get(t, "/metrics")
	if code != 200 {
		t.Fatalf("/metrics = %d", code)
	}
	ct := hdr.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus text format", ct)
	}
	for _, want := range []string{"corral_tracing_spans_dropped_total", "\ngo_", "\nprocess_"} {
		if !strings.Contains("\n"+body, want) {
			t.Errorf("/metrics lacks %q", strings.TrimSpace(want))
		}
	}
	if !regexp.MustCompile(`(?m)^# TYPE go_\w+ `).MatchString(body) ||
		!regexp.MustCompile(`(?m)^# TYPE process_\w+ `).MatchString(body) {
		t.Error("go_ or process_ family missing")
	}
	if !regexp.MustCompile(`(?m)^corral_tracing_spans_dropped_total (\d+|0)`).MatchString(body) {
		t.Error("corral_tracing_spans_dropped_total has no zero-valued sample")
	}
}

// FR-9 / FR-11: served from the private registry only, never the default one.
func TestMetricsPrivateRegistryOnly(t *testing.T) {
	m := strings.ToLower(marker(t))
	def := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_default_registry_" + m, Help: "synthetic"})
	prometheus.MustRegister(def)
	t.Cleanup(func() { prometheus.Unregister(def) })

	reg := newRegistry(t)
	priv := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_private_registry_" + m, Help: "synthetic"})
	reg.MustRegister(priv)

	r := start(t, nil, reg)
	_, _, body := r.get(t, "/metrics")
	if !strings.Contains(body, "test_private_registry_"+m) {
		t.Error("metric from the supplied Gatherer missing")
	}
	if strings.Contains(body, "test_default_registry_"+m) {
		t.Error("metric from the default registry leaked into /metrics")
	}
}

// FR-9: OpenMetrics negotiation is allowed but exemplars must stay off.
func TestMetricsNoExemplars(t *testing.T) {
	m := strings.ToLower(marker(t))
	reg := newRegistry(t)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_exemplar_" + m + "_total", Help: "synthetic"})
	reg.MustRegister(c)
	// Bypass the catalog factory on purpose: this counter does carry an exemplar.
	ex := strings.ToLower(marker(t))
	c.(prometheus.ExemplarAdder).AddWithExemplar(1, prometheus.Labels{"trace_id": ex})

	// Control: a handler with OpenMetrics enabled over the same registry does
	// emit the exemplar, so the assertion below can fail.
	ctl := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", openMetricsAccept)
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}).ServeHTTP(ctl, req)
	if !strings.Contains(ctl.Body.String(), ex) {
		t.Fatalf("control failed: exemplar not emitted by an OpenMetrics-enabled handler")
	}

	r := start(t, nil, reg)
	for _, accept := range []string{
		openMetricsAccept,
		"application/openmetrics-text",
		"application/openmetrics-text; version=0.0.1",
		"text/plain",
		"",
	} {
		code, hdr, body := r.do(t, http.MethodGet, "/metrics", map[string]string{"Accept": accept})
		if code != 200 {
			t.Fatalf("Accept %q: /metrics = %d", accept, code)
		}
		ct := hdr.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/openmetrics-text") {
			t.Errorf("Accept %q: Content-Type = %q", accept, ct)
		}
		if strings.Contains(body, ex) {
			t.Errorf("Accept %q: exemplar present in /metrics", accept)
		}
		if regexp.MustCompile(`(?m)\} [0-9.e+-]+ # \{`).MatchString(body) || strings.Contains(body, " # {") {
			t.Errorf("Accept %q: exemplar syntax present", accept)
		}
		if !strings.Contains(body, "test_exemplar_"+m+"_total") {
			t.Errorf("Accept %q: counter itself missing", accept)
		}
	}
}
