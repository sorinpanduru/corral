// Package leaks_test is the AC-38 marker leak verification (T19).
//
// FR-24 (no credentials in any signal), FR-28 (third-party log output is
// routed through slog with constant messages) and FR-33 (rate-limited OTLP
// failure records) are checked end to end: corral runs in process through
// runtime.Run with tracing on and log.level=debug, test spans are created
// through the exposed tracer provider, and markerscan scans stdout, stderr,
// /metrics, /readyz and the collector payloads for every marker in every
// encoding it supports.
//
// Compliance (GDPR data minimisation, FR-24): all data is synthetic. Markers
// are random per run and stand in for credentials; nothing real is used.
// Failure messages never print a marker.
package leaks_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const (
	markerLen   = 40 // >= 32 so that line-wrap splits keep a 12-byte window
	windowLen   = 12
	waitTimeout = 20 * time.Second
	alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
)

var (
	windowsMu sync.Mutex
	windows   = map[string]bool{} // every 12-byte window of every marker issued so far
)

// newMarker returns a fresh synthetic marker: markerLen bytes of
// [A-Za-z0-9_-], sharing no 12-byte substring with any marker issued before.
func newMarker(tb testing.TB) string {
	tb.Helper()
	windowsMu.Lock()
	defer windowsMu.Unlock()
	for attempt := 0; attempt < 100; attempt++ {
		raw := make([]byte, markerLen)
		if _, err := rand.Read(raw); err != nil {
			tb.Fatalf("rand: %v", err)
		}
		b := make([]byte, markerLen)
		for i, r := range raw {
			b[i] = alphabet[int(r)%len(alphabet)]
		}
		m := string(b)
		ok := true
		for i := 0; i+windowLen <= len(m); i++ {
			if windows[m[i:i+windowLen]] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for i := 0; i+windowLen <= len(m); i++ {
			windows[m[i:i+windowLen]] = true
		}
		return m
	}
	tb.Fatalf("could not generate a distinct marker")
	return ""
}

// syncBuf is a goroutine-safe io.Writer, the stdout passed to Run.
type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuf) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b...)
}

// hasRecord reports whether a JSON stdout record with the given component and
// (when non-empty) message exists.
func (s *syncBuf) hasRecord(component, msg string) bool {
	for _, l := range strings.Split(string(s.bytes()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["component"] == component && (msg == "" || m["msg"] == msg) {
			return true
		}
	}
	return false
}

var probeClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

// probe GETs a URL without failing the test (it is used from Run's goroutine
// inside hooks, where tb.Fatalf is not allowed).
func probe(url string) (status int, body []byte, err error) {
	resp, err := probeClient.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, err
}

type collectorKind int

const (
	collAccept collectorKind = iota
	collReject
	collUnreachable
	collUnresponsive
)

func (k collectorKind) String() string {
	return [...]string{"accepting", "rejecting", "unreachable", "unresponsive"}[k]
}

type setup struct {
	proto      string // "http/protobuf" or "grpc"
	coll       collectorKind
	header     bool // OTEL_EXPORTER_OTLP_HEADERS=authorization=<marker>
	userinfo   bool // endpoint URL carries user:password markers
	shutdownTO bool // setup (d): shut down while the collector holds the export
}

// clearOTelEnv removes every host OTEL_* variable for the test (restored on
// cleanup) because the exporters read the real process environment.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "") // registers restoration of the original value
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unsetenv: %v", err)
			}
		}
	}
}

func freeAddr(t *testing.T) string { return otlpcollector.UnreachableEndpoint(t) }

func headerReached(c *otlpcollector.Collector, marker string) bool {
	for _, r := range c.Requests() {
		for k, vs := range r.Headers {
			if !strings.EqualFold(k, "authorization") {
				continue
			}
			for _, v := range vs {
				if v == marker {
					return true
				}
			}
		}
	}
	return false
}

// runSetup drives one AC-38 setup and ends with AssertClean.
func runSetup(t *testing.T, s setup) {
	clearOTelEnv(t)

	// Collector.
	var coll *otlpcollector.Collector
	var hostPort string
	switch s.coll {
	case collAccept:
		coll = otlpcollector.Start(t)
	case collReject:
		coll = otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject),
			otlpcollector.WithHTTPRejectStatus(http.StatusUnauthorized), // not retried by the exporter
			otlpcollector.WithGRPCRejectCode(codes.Unauthenticated))
	case collUnresponsive:
		coll = otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	case collUnreachable:
		hostPort = otlpcollector.UnreachableEndpoint(t)
	}
	if coll != nil {
		if s.proto == "grpc" {
			hostPort = coll.GRPCEndpoint()
		} else {
			hostPort = coll.HTTPEndpoint()
		}
	}

	// Markers (distinct, scanned in every source).
	sc := markerscan.New()
	var headerMarker, user, pass string
	endpoint := "http://" + hostPort
	if s.userinfo {
		user, pass = newMarker(t), newMarker(t)
		endpoint = "http://" + user + ":" + pass + "@" + hostPort
		sc.AddMarker(t, user)
		sc.AddMarker(t, pass)
	}
	if s.header {
		headerMarker = newMarker(t)
		sc.AddMarker(t, headerMarker)
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization="+headerMarker)
	}
	if coll != nil {
		sc.AddCollector(coll)
	}

	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", s.proto)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "100")

	// stdout (Run's writer) and stderr (process-level capture).
	stopCapture := markerscan.CaptureStdio(t)
	out := &syncBuf{}

	admin := freeAddr(t)
	base := "http://" + admin
	var hooksMu sync.Mutex
	scan := func(src markerscan.Source, path string) {
		_, body, err := probe(base + path)
		if err != nil {
			t.Errorf("fetch %s: %v", path, err)
			return
		}
		hooksMu.Lock()
		sc.Add(src, body)
		hooksMu.Unlock()
	}

	var beforeStatus int
	var beforeBody []byte
	hooks := corralrt.Hooks{
		BeforeLastStep: func(context.Context) {
			// Startup is held: /readyz must be 503 starting (AC-31) and is scanned.
			st, body, err := probe(base + "/readyz")
			if err != nil {
				t.Errorf("fetch /readyz during hold: %v", err)
				return
			}
			beforeStatus, beforeBody = st, body
			hooksMu.Lock()
			sc.Add(markerscan.SourceReadyz, body)
			hooksMu.Unlock()
			scan(markerscan.SourceMetrics, "/metrics")
		},
		Stopping: func(context.Context) {
			scan(markerscan.SourceReadyz, "/readyz")
			scan(markerscan.SourceMetrics, "/metrics")
		},
	}
	started := make(chan corralrt.Handles, 1)
	hooks.Started = func(h corralrt.Handles) { started <- h }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	args := []string{"-tracing.enabled=true", "-log.level=debug", "-admin.listen=" + admin}
	go func() { done <- corralrt.Run(ctx, args, os.LookupEnv, out, hooks) }()

	var h corralrt.Handles
	select {
	case h = <-started:
	case code := <-done:
		t.Fatalf("Run returned %d before startup completed", code)
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for startup")
	}
	if beforeStatus != http.StatusServiceUnavailable || strings.TrimSpace(string(beforeBody)) != "starting" {
		t.Errorf("/readyz during the hold: status %d body %q, want 503 starting", beforeStatus, beforeBody)
	}
	if st, body, err := probe(base + "/readyz"); err != nil || st != http.StatusOK {
		t.Errorf("/readyz after start: status %d err %v, want 200", st, err)
	} else {
		sc.Add(markerscan.SourceReadyz, body)
	}
	sc.Add(markerscan.SourceMetrics, markerscan.FetchBody(t, base+"/metrics"))

	// Emit test spans until the setup's expected effect is observed.
	tracer := h.TracerProvider.Tracer("ac38-test")
	emit := func() {
		_, span := tracer.Start(context.Background(), "ac38.synthetic.span")
		span.End()
	}
	var reached func() bool
	var what string
	switch s.coll {
	case collAccept:
		what, reached = "spans at the collector", func() bool { return len(coll.Spans()) > 0 }
	case collReject:
		what, reached = "export failure record", func() bool { return len(coll.Requests()) > 0 && out.hasRecord("otel", "") }
	case collUnreachable:
		what, reached = "export failure record", func() bool { return out.hasRecord("otel", "") }
	case collUnresponsive:
		what, reached = "export request at the collector", func() bool { return len(coll.Requests()) > 0 }
	}
	deadline := time.Now().Add(waitTimeout)
	for !reached() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		emit()
		time.Sleep(100 * time.Millisecond)
	}
	if s.header && coll != nil && !headerReached(coll, headerMarker) {
		// The header marker must really have been sent, otherwise the scan proves nothing.
		t.Fatalf("the authorization header did not reach the collector")
	}
	// A few more spans and a pause so later exports and debug records occur.
	for i := 0; i < 3; i++ {
		emit()
		time.Sleep(150 * time.Millisecond)
	}
	sc.Add(markerscan.SourceMetrics, markerscan.FetchBody(t, base+"/metrics"))

	// Graceful shutdown (setup d: bounded by FR-34's 5 s against a held export).
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Run exit code %d, want 0", code)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("Run did not return after cancel")
	}
	stdout, stderr := stopCapture()

	if s.shutdownTO && !out.hasRecord("lifecycle", "corral shutdown step failed") {
		t.Errorf("expected the shutdown failure record after the timed-out flush")
	}
	if (s.coll == collReject || s.coll == collUnreachable) && !out.hasRecord("otel", "") {
		t.Errorf("expected an otel export failure record (FR-33)")
	}

	sc.Add(markerscan.SourceStdout, out.bytes())
	sc.Add(markerscan.SourceStdout, stdout) // process-level stdout, e.g. stray writes
	sc.Add(markerscan.SourceStderr, stderr)
	sc.AssertClean(t)
}

var protocols = []string{"http/protobuf", "grpc"}

func eachProto(t *testing.T, f func(t *testing.T, proto string)) {
	for _, p := range protocols {
		p := p
		t.Run(strings.ReplaceAll(p, "/", "_"), func(t *testing.T) { f(t, p) })
	}
}

// AC-38 (FR-24, FR-28, FR-33), setup (a): authorization header marker, rejecting collector.
func TestAC38_HeaderMarker_RejectingCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{proto: p, coll: collReject, header: true})
	})
}

// AC-38, setup (b): same header, unreachable collector.
func TestAC38_HeaderMarker_UnreachableCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{proto: p, coll: collUnreachable, header: true})
	})
}

// AC-38, setup (c): userinfo markers in the endpoint URL, accepting collector.
func TestAC38_UserinfoMarker_AcceptingCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{proto: p, coll: collAccept, userinfo: true})
	})
}

// AC-38, setup (c): userinfo markers in the endpoint URL, unreachable collector.
func TestAC38_UserinfoMarker_UnreachableCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{proto: p, coll: collUnreachable, userinfo: true})
	})
}

// AC-38, setup (d) (T16 security review; FR-24, FR-28, FR-34): timed-out
// shutdown against an unresponsive collector, endpoint carrying the userinfo
// markers. The shutdown failure log and all signals are scanned.
func TestAC38_TimedOutShutdown_UnresponsiveCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{proto: p, coll: collUnresponsive, userinfo: true, shutdownTO: true})
	})
}
