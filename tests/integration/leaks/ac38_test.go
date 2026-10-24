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
// markerscan's own failure messages do print the synthetic marker they found
// (that is what makes a leak diagnosable); this package's final sweep prints
// only a setup label, the source and the encoding name, never data. Captured
// stderr content is printed only after the marker scan has passed.
package leaks_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const (
	markerLen   = 40 // >= 32 so that line-wrap splits keep a 12-byte window
	windowLen   = 12
	waitTimeout = 60 * time.Second
	alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
)

var (
	windowsMu sync.Mutex
	windows   = map[string]bool{} // every 12-byte window of every marker issued so far

	// registry of every marker issued (marker -> label) and every run's
	// buffers, for TestMain's final sweep over late writes.
	registryMu sync.Mutex
	markers    = map[string]string{}
	runs       []*runRecord
)

// runRecord keeps one run's buffers alive for the final sweep.
type runRecord struct {
	label          string
	out            *syncBuf
	stdout, stderr []byte
	coll           *otlpcollector.Collector
}

func registerRun(r *runRecord) {
	registryMu.Lock()
	runs = append(runs, r)
	registryMu.Unlock()
}

// newMarker returns a fresh synthetic marker: markerLen bytes of
// [A-Za-z0-9_-], sharing no 12-byte substring with any marker issued before.
func newMarker(tb testing.TB, label string) string {
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
		registryMu.Lock()
		markers[m] = label
		registryMu.Unlock()
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

// find returns the first JSON stdout record for which match is true.
func (s *syncBuf) find(match func(map[string]any) bool) (map[string]any, bool) {
	for _, l := range strings.Split(string(s.bytes()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if match(m) {
			return m, true
		}
	}
	return nil, false
}

func isLevel(m map[string]any, want string) bool {
	l, _ := m["level"].(string)
	return strings.EqualFold(l, want)
}

// exportFailure matches the FR-33 export-failure record: component otel, the
// logbridge constant message for the otel error handler, at warn.
func exportFailure(m map[string]any) bool {
	return m["component"] == "otel" && m["msg"] == logbridge.MsgOTelError && isLevel(m, "warn")
}

// shutdownStepFailed matches the runtime's shutdown failure record.
func shutdownStepFailed(m map[string]any) bool {
	return m["component"] == "lifecycle" && m["msg"] == "corral shutdown step failed" && isLevel(m, "warn")
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
	name       string
	proto      string // "http/protobuf" or "grpc"
	coll       collectorKind
	header     bool // authorization=<marker> header variable
	userinfo   bool // endpoint URL carries user:password markers
	shutdownTO bool // setup (d): shut down while the collector holds the export
	echo       bool // setup (e): the rejecting collector echoes the credentials back
	traces     bool // setup (f): only the OTEL_EXPORTER_OTLP_TRACES_* variables are set
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

// retargetBridges points the process-wide third-party log bridges at a
// throwaway factory, so stray collector or grpc logs after a test do not land
// in a finished test's buffer. Anything that still arrives is covered by the
// final sweep in TestMain.
func retargetBridges() {
	h := logging.NewHandler(io.Discard, "json", slog.LevelError)
	logbridge.Install(logging.NewFactory(h), slog.LevelError, time.Now)
}

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
	t.Cleanup(retargetBridges) // registered first, so it runs after the collector's Close

	label := s.name + "/" + strings.ReplaceAll(s.proto, "/", "_")
	// Markers (distinct, scanned in every source).
	sc := markerscan.New()
	var headerMarker, user, pass string
	if s.userinfo {
		user, pass = newMarker(t, label+" user"), newMarker(t, label+" password")
		sc.AddMarker(t, user)
		sc.AddMarker(t, pass)
	}
	if s.header {
		headerMarker = newMarker(t, label+" header")
		sc.AddMarker(t, headerMarker)
	}

	// Collector.
	var coll *otlpcollector.Collector
	var hostPort string
	switch s.coll {
	case collAccept:
		coll = otlpcollector.Start(t)
	case collReject:
		opts := []otlpcollector.Option{otlpcollector.WithMode(otlpcollector.ModeReject),
			otlpcollector.WithHTTPRejectStatus(http.StatusUnauthorized), // not retried by the exporter
			otlpcollector.WithGRPCRejectCode(codes.Unauthenticated)}
		if s.echo {
			// As if an auth proxy echoed the credentials it was given.
			text := "auth proxy rejected " + headerMarker + " for " + user + ":" + pass
			opts = append(opts, otlpcollector.WithHTTPRejectBody(text), otlpcollector.WithGRPCRejectMessage(text))
		}
		coll = otlpcollector.Start(t, opts...)
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
		sc.AddCollector(coll)
	}

	endpoint := "http://" + hostPort
	if s.userinfo {
		endpoint = "http://" + user + ":" + pass + "@" + hostPort
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "100")
	if s.traces {
		// Traces-specific variables only; the generic ones stay unset. The
		// OTLP/HTTP exporter uses a traces endpoint verbatim, so it needs the path.
		if s.proto != "grpc" {
			endpoint += "/v1/traces"
		}
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", s.proto)
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", endpoint)
		if s.header {
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "authorization="+headerMarker)
		}
	} else {
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", s.proto)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
		if s.header {
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization="+headerMarker)
		}
	}

	// stdout (Run's writer) and stderr (process-level capture).
	stopCapture := markerscan.CaptureStdio(t)
	out := &syncBuf{}
	rec := &runRecord{label: label, out: out, coll: coll}
	registerRun(rec)

	// Hook errors are collected and reported after stopCapture, so -v output
	// is not swallowed by the capture.
	var hooksMu sync.Mutex
	var hookErrs []string
	hookErr := func(format string, args ...any) {
		hooksMu.Lock()
		hookErrs = append(hookErrs, fmt.Sprintf(format, args...))
		hooksMu.Unlock()
	}
	reportHookErrs := func() {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		for _, e := range hookErrs {
			t.Error(e)
		}
		hookErrs = nil
	}
	// bail stops the capture, reports collected errors and fails the test.
	bail := func(format string, args ...any) {
		_, _ = stopCapture()
		reportHookErrs()
		t.Fatalf(format, args...)
	}

	admin := otlpcollector.UnreachableEndpoint(t) // a free loopback port
	base := "http://" + admin
	scan := func(src markerscan.Source, path string) (int, []byte) {
		st, body, err := probe(base + path)
		if err != nil {
			hookErr("fetch %s: %v", path, err)
			return 0, nil
		}
		hooksMu.Lock()
		sc.Add(src, body)
		hooksMu.Unlock()
		return st, body
	}

	var beforeStatus int
	var beforeBody []byte
	var stopRan bool
	var stopStatus int
	var stopBody []byte
	started := make(chan corralrt.Handles, 1)
	hooks := corralrt.Hooks{
		BeforeLastStep: func(context.Context) {
			// Startup is held: /readyz must be 503 starting (AC-31) and is scanned.
			beforeStatus, beforeBody = scan(markerscan.SourceReadyz, "/readyz")
			scan(markerscan.SourceMetrics, "/metrics")
		},
		Stopping: func(context.Context) {
			stopRan = true
			stopStatus, stopBody = scan(markerscan.SourceReadyz, "/readyz")
			scan(markerscan.SourceMetrics, "/metrics")
		},
		Started: func(h corralrt.Handles) { started <- h },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	args := []string{"-tracing.enabled=true", "-log.level=debug", "-admin.listen=" + admin}
	go func() { done <- corralrt.Run(ctx, args, os.LookupEnv, out, hooks) }()

	var h corralrt.Handles
	select {
	case h = <-started:
	case code := <-done:
		bail("Run returned %d before startup completed", code)
	case <-time.After(waitTimeout):
		bail("timed out waiting for startup")
	}
	if beforeStatus != http.StatusServiceUnavailable || string(beforeBody) != "starting\n" {
		hookErr("/readyz during the hold: status %d body %q, want 503 starting", beforeStatus, beforeBody)
	}
	if st, body, err := probe(base + "/readyz"); err != nil || st != http.StatusOK {
		hookErr("/readyz after start: status %d err %v, want 200", st, err)
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
		what, reached = "export failure record", func() bool {
			_, ok := out.find(exportFailure)
			return len(coll.Requests()) > 0 && ok
		}
	case collUnreachable:
		what, reached = "export failure record", func() bool { _, ok := out.find(exportFailure); return ok }
	case collUnresponsive:
		what, reached = "export request at the collector", func() bool { return len(coll.Requests()) > 0 }
	}
	deadline := time.Now().Add(waitTimeout)
	for !reached() {
		if time.Now().After(deadline) {
			bail("timed out waiting for: %s", what)
		}
		emit()
		time.Sleep(100 * time.Millisecond)
	}
	if s.header && coll != nil && !headerReached(coll, headerMarker) {
		// The header marker must really have been sent, otherwise the scan proves nothing.
		bail("the authorization header did not reach the collector")
	}
	// A few more spans and a pause so later exports and debug records occur.
	for i := 0; i < 3; i++ {
		emit()
		time.Sleep(150 * time.Millisecond)
	}
	sc.Add(markerscan.SourceMetrics, markerscan.FetchBody(t, base+"/metrics"))

	// Graceful shutdown (setup d: bounded by FR-34's 5 s against a held export).
	cancel()
	var code int
	select {
	case code = <-done:
	case <-time.After(waitTimeout):
		bail("Run did not return after cancel")
	}
	stdout, stderr := stopCapture()
	rec.stdout, rec.stderr = stdout, stderr
	reportHookErrs()
	if code != 0 {
		t.Errorf("Run exit code %d, want 0", code)
	}
	if !stopRan {
		t.Errorf("the Stopping hook did not run")
	} else if stopStatus != http.StatusServiceUnavailable || string(stopBody) != "shutting_down\n" {
		t.Errorf("/readyz in the Stopping hook: status %d body %q, want 503 shutting_down", stopStatus, stopBody)
	}

	if s.shutdownTO {
		if r, ok := out.find(shutdownStepFailed); !ok {
			t.Errorf("expected the shutdown-step-failed record after the timed-out flush")
		} else if r["error"] != "timeout" {
			t.Errorf("shutdown-step-failed error class %v, want timeout", r["error"])
		}
	}
	if (s.coll == collReject || s.coll == collUnreachable) && !func() bool { _, ok := out.find(exportFailure); return ok }() {
		t.Errorf("expected an FR-33 export-failure record (otel, warn)")
	}

	// Late output: close the collector, let stragglers arrive, then scan.
	if coll != nil {
		coll.Close()
	}
	time.Sleep(500 * time.Millisecond)
	sc.Add(markerscan.SourceStdout, out.bytes())
	sc.Add(markerscan.SourceStdout, stdout) // process-level stdout, e.g. stray writes
	sc.Add(markerscan.SourceStderr, stderr)
	sc.AssertClean(t)

	// FR-28: corral writes nothing to stderr. Content is printed only when the
	// marker scan found nothing, so a leak is never echoed into the report.
	if len(stderr) > 0 {
		if len(sc.Findings()) == 0 {
			t.Errorf("stderr must be empty (FR-28), got %d bytes: %q", len(stderr), stderr)
		} else {
			t.Errorf("stderr must be empty (FR-28), got %d bytes (content withheld: marker findings)", len(stderr))
		}
	}
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
		runSetup(t, setup{name: "a", proto: p, coll: collReject, header: true})
	})
}

// AC-38, setup (b): same header, unreachable collector.
func TestAC38_HeaderMarker_UnreachableCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "b", proto: p, coll: collUnreachable, header: true})
	})
}

// AC-38, setup (c): userinfo markers in the endpoint URL, accepting collector.
func TestAC38_UserinfoMarker_AcceptingCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "c-accept", proto: p, coll: collAccept, userinfo: true})
	})
}

// AC-38, setup (c): userinfo markers in the endpoint URL, unreachable collector.
func TestAC38_UserinfoMarker_UnreachableCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "c-unreachable", proto: p, coll: collUnreachable, userinfo: true})
	})
}

// AC-38, setup (d) (T16 security review; FR-24, FR-28, FR-34): timed-out
// shutdown against an unresponsive collector, endpoint carrying the userinfo
// markers. The shutdown failure log and all signals are scanned.
func TestAC38_TimedOutShutdown_UnresponsiveCollector(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "d", proto: p, coll: collUnresponsive, userinfo: true, shutdownTO: true})
	})
}

// AC-38, setup (e): a rejecting collector that echoes the credentials it was
// given (header and userinfo markers) in its HTTP body / gRPC status message,
// as an authenticating proxy might.
func TestAC38_RejectingCollectorEchoesCredentials(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "e", proto: p, coll: collReject, header: true, userinfo: true, echo: true})
	})
}

// AC-38, setup (f): the traces-specific variables
// (OTEL_EXPORTER_OTLP_TRACES_HEADERS and _TRACES_ENDPOINT with userinfo
// markers), with the generic variables unset.
func TestAC38_TracesSpecificVariables(t *testing.T) {
	eachProto(t, func(t *testing.T, p string) {
		runSetup(t, setup{name: "f", proto: p, coll: collReject, header: true, userinfo: true, traces: true})
	})
}
