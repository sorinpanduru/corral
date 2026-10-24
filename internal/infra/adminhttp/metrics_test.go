package adminhttp_test

import (
	"context"
	"errors"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
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

// FR-9: exemplars must stay off whatever the client asks for.
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
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Accept %q: Content-Type = %q, want text/plain", accept, ct)
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

// (D) Only the Prometheus text format is served: protobuf and OpenMetrics
// Accept headers still get text/plain; version=0.0.4.
func TestMetricsOnlyTextFormat(t *testing.T) {
	r := start(t, nil, nil)
	for _, accept := range []string{
		"application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited",
		"application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=text",
		openMetricsAccept, "application/openmetrics-text", "*/*", "",
	} {
		code, hdr, body := r.do(t, http.MethodGet, "/metrics", map[string]string{"Accept": accept})
		ct := hdr.Get("Content-Type")
		if code != 200 || !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
			t.Errorf("Accept %q: %d %q, want 200 text/plain; version=0.0.4", accept, code, ct)
		}
		if !strings.HasPrefix(body, "# HELP ") && !strings.Contains(body, "\n# HELP ") {
			t.Errorf("Accept %q: body is not text exposition", accept)
		}
		if strings.Contains(body, "# EOF") {
			t.Errorf("Accept %q: OpenMetrics terminator in body", accept)
		}
	}
}

// A Gatherer error never reaches the response: 500 with a fixed body, with an
// ErrorLog and with a nil one (no panic).
func TestMetricsGathererError(t *testing.T) {
	m := marker(t)
	g := failGatherer{err: errors.New(m + "\x1b[31m gather failed")}
	check := func(t *testing.T, r *running) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			code, hdr, body := r.do(t, method, "/metrics", nil)
			want := "error\n"
			if method == http.MethodHead {
				want = ""
			}
			if code != 500 || body != want || hdr.Get("Content-Type") != textPlainUTF {
				t.Errorf("%s /metrics = %d %q %q, want 500 %q", method, code, body, hdr.Get("Content-Type"), want)
			}
			if strings.Contains(body+strings.Join(flat(hdr), "|"), m) {
				t.Errorf("%s: gatherer error text leaked", method)
			}
		}
		// the server survives
		if code, _, _ := r.get(t, "/healthz"); code != 200 {
			t.Errorf("healthz after gather error = %d", code)
		}
	}
	t.Run("with ErrorLog", func(t *testing.T) {
		r := startOpts(t, adminhttp.Options{Gatherer: g})
		check(t, r)
		if strings.Contains(r.logs.buf.String(), m) {
			t.Error("gatherer error text leaked into logs")
		}
	})
	t.Run("nil ErrorLog", func(t *testing.T) {
		srv := adminhttp.New(adminhttp.Options{Addr: loopback, Gatherer: g})
		if err := srv.Listen(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- srv.Serve() }()
		check(t, &running{srv: srv, base: "http://" + srv.Addr().String()})
		_ = srv.Shutdown(context.Background())
		<-done
	})
}
