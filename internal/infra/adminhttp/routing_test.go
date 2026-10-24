package adminhttp_test

import (
	"context"
	"expvar"
	"net/http"
	"net/http/httptest"
	_ "net/http/pprof" // registers on DefaultServeMux: the admin mux must not expose it (FR-15)
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
)

var _ = expvar.NewInt // expvar registers /debug/vars on DefaultServeMux

// Control: the test binary really exposes pprof and expvar on the default mux,
// so the 404 assertions below are meaningful (FR-15, AC-7).
func TestControlDefaultMuxHasPprofAndExpvar(t *testing.T) {
	for _, p := range []string{"/debug/pprof/", "/debug/vars"} {
		rec := httptest.NewRecorder()
		http.DefaultServeMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("control: DefaultServeMux %s = %d, want 200", p, rec.Code)
		}
	}
}

// FR-15, AC-7: exact paths only; the dedicated mux never serves pprof/expvar.
func TestNotFoundPaths(t *testing.T) {
	r := start(t, nil, nil)
	r.srv.MarkStarted()
	m := marker(t)
	paths := []string{
		"/metrics/", "/healthz/", "/readyz/", "/", "/debug/pprof/", "/debug/pprof",
		"/debug/pprof/cmdline", "/debug/pprof/heap", "/debug/vars", "/unknown",
		"/metrics/x", "/Metrics", "/HEALTHZ", "/readyz/x", "/" + m,
	}
	for _, p := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			code, hdr, body := r.do(t, method, p, nil)
			if code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, p, code)
			}
			if strings.Contains(body, m) || strings.Contains(strings.Join(hdr.Values("Location"), ""), m) {
				t.Errorf("%s %s echoed the request path", method, p)
			}
			if strings.Contains(body, "goroutine") || strings.Contains(body, "cmdline") {
				t.Errorf("%s %s looks like pprof/expvar output", method, p)
			}
		}
	}
}

// FR-15 via Handler(): works without Listen, dedicated mux, not DefaultServeMux.
func TestHandlerIsDedicatedMux(t *testing.T) {
	srv := adminhttp.New(adminhttp.Options{Addr: loopback, Gatherer: newRegistry(t), ErrorLog: newLogs(t).l})
	h := srv.Handler()
	if h == http.DefaultServeMux {
		t.Fatal("Handler() is http.DefaultServeMux")
	}
	for _, p := range []string{"/debug/pprof/", "/debug/vars", "/metrics/", "/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("Handler GET %s = %d, want 404", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("Handler GET /healthz = %d %q", rec.Code, rec.Body.String())
	}
}

// FR-15, AC-7: only GET and HEAD; others 405 with Allow: GET, HEAD.
func TestMethodNotAllowed(t *testing.T) {
	r := start(t, nil, nil)
	r.srv.MarkStarted()
	for _, p := range []string{"/metrics", "/healthz", "/readyz"} {
		for _, method := range []string{
			http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
			http.MethodOptions, http.MethodTrace, "PROPFIND",
		} {
			code, hdr, _ := r.do(t, method, p, nil)
			if code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, p, code)
				continue
			}
			if got := hdr.Get("Allow"); got != allowAll {
				t.Errorf("%s %s Allow = %q, want %q", method, p, got, allowAll)
			}
		}
	}
}

// AC-7: HEAD succeeds on each served path, with no body.
func TestHeadOnEachPath(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: true}}
	r := start(t, ready, nil)
	r.srv.MarkStarted()
	for _, p := range []string{"/metrics", "/healthz", "/readyz"} {
		code, hdr, body := r.do(t, http.MethodHead, p, nil)
		if code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", p, code)
		}
		if body != "" {
			t.Errorf("HEAD %s returned a body", p)
		}
		if hdr.Get("Content-Type") == "" {
			t.Errorf("HEAD %s has no Content-Type", p)
		}
	}
}

// FR-17, AC-12: /healthz 200, ok\n, text/plain; charset=utf-8.
func TestHealthz(t *testing.T) {
	r := start(t, nil, nil) // liveness does not depend on startup completing
	code, hdr, body := r.get(t, "/healthz")
	if code != 200 || body != "ok\n" || hdr.Get("Content-Type") != textPlainUTF {
		t.Fatalf("/healthz = %d %q %q", code, body, hdr.Get("Content-Type"))
	}
	r.srv.MarkStarted()
	r.srv.MarkStopping()
	code, _, body = r.get(t, "/healthz")
	if code != 200 || body != "ok\n" {
		t.Fatalf("/healthz while stopping = %d %q, want 200 ok", code, body)
	}
}

// FR-15, AC-7: query strings are ignored and never echoed.
func TestQueryStringIgnored(t *testing.T) {
	ready := &fakeReadiness{status: health.Status{Ready: false, Reason: health.Reason("zz")}}
	r := start(t, ready, nil)
	r.srv.MarkStarted()
	m := marker(t)
	wantCode, wantHdr, wantBody := r.get(t, "/healthz")
	for _, q := range []string{"?x=1", "?", "?" + m, "?a=b&a=c&ready=true", "?reason=ok",
		"?name[]=go_goroutines", "?name%5B%5D=go_goroutines"} {
		code, hdr, body := r.get(t, "/healthz"+q)
		if code != wantCode || body != wantBody || hdr.Get("Content-Type") != wantHdr.Get("Content-Type") {
			t.Errorf("/healthz%s differs: %d %q", q, code, body)
		}
		rc, _, rb := r.get(t, "/readyz"+q)
		if rc != 503 || rb != "unknown\n" {
			t.Errorf("/readyz%s = %d %q, want 503 unknown", q, rc, rb)
		}
		mc, _, mb := r.get(t, "/metrics"+q)
		if mc != 200 || strings.Contains(mb, m) {
			t.Errorf("/metrics%s = %d (echo=%v)", q, mc, strings.Contains(mb, m))
		}
		// (D) the query has no effect: no family filtering (name[]=...).
		if !strings.Contains(mb, "\ncorral_tracing_spans_dropped_total") && !strings.HasPrefix(mb, "corral_tracing_spans_dropped_total") ||
			!strings.Contains(mb, "\nprocess_") || !strings.Contains(mb, "\ngo_") {
			t.Errorf("/metrics%s lost families: query must not filter", q)
		}
	}
	// 404 for an unknown path stays 404 with a query.
	if code, _, _ := r.get(t, "/metrics/?x=1"); code != 404 {
		t.Errorf("/metrics/?x=1 = %d, want 404", code)
	}
}

// FR-15 hardening: request headers are never reflected into responses.
func TestRequestDataNotReflected(t *testing.T) {
	r := start(t, nil, nil)
	r.srv.MarkStarted()
	m := marker(t)
	hdr := map[string]string{"User-Agent": m, "X-Forwarded-For": m, "Referer": m}
	for _, p := range []string{"/healthz", "/readyz", "/nope"} {
		_, h, body := r.do(t, http.MethodGet, p, hdr)
		if strings.Contains(body, m) || strings.Contains(strings.Join(flat(h), "|"), m) {
			t.Errorf("%s reflected request data", p)
		}
	}
}

func flat(h http.Header) []string {
	var out []string
	for k, vs := range h {
		out = append(out, k)
		out = append(out, vs...)
	}
	return out
}

// Shutdown stops the listener; Serve returns nil or http.ErrServerClosed.
func TestShutdown(t *testing.T) {
	lg := newLogs(t)
	srv := adminhttp.New(adminhttp.Options{Addr: loopback, Gatherer: newRegistry(t), ErrorLog: lg.l})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	r := &running{srv: srv, base: "http://" + srv.Addr().String()}
	if code, _, _ := r.get(t, "/healthz"); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-done; !isClosed(err) {
		t.Fatalf("Serve returned %v", err)
	}
	if _, err := http.Get(r.base + "/healthz"); err == nil {
		t.Fatal("listener still accepting after Shutdown")
	}
}
