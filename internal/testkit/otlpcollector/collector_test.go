package otlpcollector_test

// Self-tests for the in-process fake OTLP collector (plan task T13, ADR-0005),
// driven by the real OpenTelemetry OTLP exporters. All data is synthetic.
//
// Mapping to spec ACs the collector supports:
//   AC-14  ConnCount is zero when nothing connects; UnreachableEndpoint refuses.
//   AC-15  HTTP/protobuf and gRPC delivery; gzip request bodies flagged.
//   AC-24  Transport headers / metadata and resource attributes recorded.
//   AC-27  Resources() exposes exported resource attributes.
//   AC-28  Unresponsive mode holds requests until Close.
//   AC-33/38  Spans() exposes names, attributes and events for scanning.
//
// Assumed (not fixed by the plan) identifiers, to be matched by T13:
//   Mode constants ModeAccept, ModeReject, ModeUnresponsive;
//   options WithMode(Mode), WithHTTPRejectStatus(int), WithGRPCRejectCode(codes.Code);
//   RecordedSpan{Name string; Attributes map[string]string; Events []RecordedEvent};
//   RecordedEvent{Name string; Attributes map[string]string};
//   RequestInfo.Protocol is a string ("http" / "grpc"), ContentEncoding a string.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

func hostPort(endpoint string) string {
	if i := strings.Index(endpoint, "://"); i >= 0 {
		endpoint = endpoint[i+3:]
	}
	return strings.TrimSuffix(endpoint, "/")
}

func stubs(res *resource.Resource) tracetest.SpanStubs {
	now := time.Now()
	return tracetest.SpanStubs{{
		Name:      "synthetic.span",
		StartTime: now.Add(-time.Second),
		EndTime:   now,
		Attributes: []attribute.KeyValue{
			attribute.String("synthetic.key", "synthetic-value"),
		},
		Events: []sdktrace.Event{{
			Name:       "synthetic.event",
			Time:       now,
			Attributes: []attribute.KeyValue{attribute.String("event.key", "event-value")},
		}},
		Resource: res,
	}}
}

func testResource(t *testing.T) *resource.Resource {
	t.Helper()
	return resource.NewSchemaless(
		attribute.String("service.name", "corral-selftest"),
		attribute.String("deployment.environment", "synthetic"),
	)
}

type exporter interface {
	ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error
	Shutdown(context.Context) error
}

func newHTTPExp(t *testing.T, c *otlpcollector.Collector, opts ...otlptracehttp.Option) exporter {
	t.Helper()
	opts = append([]otlptracehttp.Option{
		otlptracehttp.WithEndpoint(hostPort(c.HTTPEndpoint())),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
		otlptracehttp.WithTimeout(2 * time.Second),
	}, opts...)
	e, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("http exporter: %v", err)
	}
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	return e
}

func newGRPCExp(t *testing.T, c *otlpcollector.Collector, opts ...otlptracegrpc.Option) exporter {
	t.Helper()
	opts = append([]otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(hostPort(c.GRPCEndpoint())),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}),
		otlptracegrpc.WithTimeout(2 * time.Second),
	}, opts...)
	e, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("grpc exporter: %v", err)
	}
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	return e
}

func export(e exporter, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return e.ExportSpans(ctx, stubs(nil).Snapshots())
}

func exportWithRes(t *testing.T, e exporter) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.ExportSpans(ctx, stubs(testResource(t)).Snapshots())
}

func TestStartEndpointsAndInitialState(t *testing.T) {
	c := otlpcollector.Start(t)
	h, g := hostPort(c.HTTPEndpoint()), hostPort(c.GRPCEndpoint())
	for name, ep := range map[string]string{"http": h, "grpc": g} {
		host, port, err := net.SplitHostPort(ep)
		if err != nil {
			t.Fatalf("%s endpoint %q: %v", name, ep, err)
		}
		if host != "127.0.0.1" {
			t.Errorf("%s endpoint host = %q, want 127.0.0.1", name, host)
		}
		if port == "" || port == "0" {
			t.Errorf("%s endpoint has no ephemeral port: %q", name, ep)
		}
	}
	if h == g {
		t.Errorf("HTTP and gRPC must use distinct listeners, both %q", h)
	}
	// AC-14: a collector nobody talks to has accepted zero connections.
	if n := c.ConnCount(); n != 0 {
		t.Errorf("ConnCount = %d, want 0", n)
	}
	if len(c.Spans()) != 0 || len(c.Requests()) != 0 || len(c.Resources()) != 0 {
		t.Errorf("fresh collector has recorded data")
	}
}

func TestHTTPAcceptRecordsSpans(t *testing.T) { // AC-15, AC-33
	c := otlpcollector.Start(t)
	if err := exportWithRes(t, newHTTPExp(t, c)); err != nil {
		t.Fatalf("export: %v", err)
	}
	checkRecorded(t, c, "http")
}

func TestGRPCAcceptRecordsSpans(t *testing.T) { // AC-15, AC-33
	c := otlpcollector.Start(t)
	if err := exportWithRes(t, newGRPCExp(t, c)); err != nil {
		t.Fatalf("export: %v", err)
	}
	checkRecorded(t, c, "grpc")
}

func checkRecorded(t *testing.T, c *otlpcollector.Collector, proto string) {
	t.Helper()
	spans := c.Spans()
	if len(spans) != 1 {
		t.Fatalf("Spans() len = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name != "synthetic.span" {
		t.Errorf("span name = %q", s.Name)
	}
	if s.Attributes["synthetic.key"] != "synthetic-value" {
		t.Errorf("span attributes = %v", s.Attributes)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "synthetic.event" ||
		s.Events[0].Attributes["event.key"] != "event-value" {
		t.Errorf("span events = %+v", s.Events)
	}
	// AC-24, AC-27: resource attributes are exposed.
	res := c.Resources()
	if len(res) == 0 {
		t.Fatalf("Resources() empty")
	}
	if res[0]["service.name"] != "corral-selftest" || res[0]["deployment.environment"] != "synthetic" {
		t.Errorf("resource = %v", res[0])
	}
	reqs := c.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Requests() len = %d, want 1", len(reqs))
	}
	if !strings.EqualFold(reqs[0].Protocol, proto) {
		t.Errorf("Protocol = %q, want %q", reqs[0].Protocol, proto)
	}
	if reqs[0].ContentEncoding == "gzip" {
		t.Errorf("uncompressed request flagged as gzip")
	}
	if c.ConnCount() < 1 {
		t.Errorf("ConnCount = %d after a request, want >= 1", c.ConnCount())
	}
}

func TestGzipFlagged(t *testing.T) { // AC-15 compression
	c := otlpcollector.Start(t)
	if err := export(newHTTPExp(t, c, otlptracehttp.WithCompression(otlptracehttp.GzipCompression)), 5*time.Second); err != nil {
		t.Fatalf("http gzip export: %v", err)
	}
	if err := export(newGRPCExp(t, c, otlptracegrpc.WithCompressor("gzip")), 5*time.Second); err != nil {
		t.Fatalf("grpc gzip export: %v", err)
	}
	reqs := c.Requests()
	if len(reqs) != 2 {
		t.Fatalf("Requests() len = %d, want 2", len(reqs))
	}
	for _, r := range reqs {
		if r.ContentEncoding != "gzip" {
			t.Errorf("%s request ContentEncoding = %q, want gzip", r.Protocol, r.ContentEncoding)
		}
	}
	// Gzip bodies must still be decoded.
	if n := len(c.Spans()); n != 2 {
		t.Errorf("Spans() len = %d, want 2 (decoded from gzip)", n)
	}
}

func TestHeadersRecorded(t *testing.T) { // AC-24
	c := otlpcollector.Start(t)
	hdr := map[string]string{"x-synthetic-token": "hdr-value-123"}
	if err := export(newHTTPExp(t, c, otlptracehttp.WithHeaders(hdr)), 5*time.Second); err != nil {
		t.Fatalf("http export: %v", err)
	}
	if err := export(newGRPCExp(t, c, otlptracegrpc.WithHeaders(hdr)), 5*time.Second); err != nil {
		t.Fatalf("grpc export: %v", err)
	}
	reqs := c.Requests()
	if len(reqs) != 2 {
		t.Fatalf("Requests() len = %d, want 2", len(reqs))
	}
	for _, r := range reqs {
		s := strings.ToLower(fmt.Sprint(r.Headers))
		if !strings.Contains(s, "x-synthetic-token") || !strings.Contains(s, "hdr-value-123") {
			t.Errorf("%s request headers missing configured header: %v", r.Protocol, r.Headers)
		}
	}
}

func TestRejectHTTP(t *testing.T) {
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(503))
	if err := export(newHTTPExp(t, c), 5*time.Second); err == nil {
		t.Fatalf("export succeeded against rejecting collector")
	}
	if n := len(c.Spans()); n != 0 {
		t.Errorf("rejected request recorded %d spans", n)
	}
}

func TestRejectGRPC(t *testing.T) {
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithGRPCRejectCode(codes.Unavailable))
	if err := export(newGRPCExp(t, c), 5*time.Second); err == nil {
		t.Fatalf("export succeeded against rejecting collector")
	}
	if n := len(c.Spans()); n != 0 {
		t.Errorf("rejected request recorded %d spans", n)
	}
}

func TestRejectHTTP4xx(t *testing.T) {
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(400))
	if err := export(newHTTPExp(t, c), 5*time.Second); err == nil {
		t.Fatalf("export succeeded against rejecting collector")
	}
}

func TestSetModeSwitchesBehaviour(t *testing.T) {
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(500))
	e := newHTTPExp(t, c)
	if err := export(e, 5*time.Second); err == nil {
		t.Fatalf("expected rejection")
	}
	c.SetMode(otlpcollector.ModeAccept)
	if err := export(e, 5*time.Second); err != nil {
		t.Fatalf("export after switching to accept: %v", err)
	}
	if n := len(c.Spans()); n != 1 {
		t.Errorf("Spans() len = %d, want 1", n)
	}
}

func TestUnresponsiveHoldsUntilClose(t *testing.T) { // AC-28
	for name, mk := range map[string]func(*testing.T, *otlpcollector.Collector) exporter{
		"http": func(t *testing.T, c *otlpcollector.Collector) exporter { return newHTTPExp(t, c) },
		"grpc": func(t *testing.T, c *otlpcollector.Collector) exporter { return newGRPCExp(t, c) },
	} {
		t.Run(name, func(t *testing.T) {
			c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			e := mk(t, c)
			start := time.Now()
			if err := export(e, 400*time.Millisecond); err == nil {
				t.Fatalf("export succeeded against unresponsive collector")
			}
			if d := time.Since(start); d < 300*time.Millisecond {
				t.Errorf("export returned after %v; collector did not hold the request", d)
			}
			// A held request must not block Close.
			done := make(chan struct{})
			go func() { c.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Close blocked while a request was held")
			}
			if n := len(c.Spans()); n != 0 {
				t.Errorf("unresponsive collector recorded %d spans", n)
			}
		})
	}
}

func TestUnresponsiveReleasedOnClose(t *testing.T) {
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	e := newHTTPExp(t, c, otlptracehttp.WithTimeout(30*time.Second))
	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		errc <- e.ExportSpans(ctx, stubs(nil).Snapshots())
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-errc:
		t.Fatalf("export returned before Close: %v", err)
	default:
	}
	c.Close()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatalf("held request not released by Close")
	}
}

func TestConnCountCountsAcceptedConnections(t *testing.T) { // AC-14
	c := otlpcollector.Start(t)
	before := c.ConnCount()
	conn, err := net.Dial("tcp", hostPort(c.HTTPEndpoint()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for c.ConnCount() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.ConnCount() != before+1 {
		t.Errorf("ConnCount = %d, want %d", c.ConnCount(), before+1)
	}
	conn2, err := net.Dial("tcp", hostPort(c.GRPCEndpoint()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	deadline = time.Now().Add(2 * time.Second)
	for c.ConnCount() <= before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.ConnCount() != before+2 {
		t.Errorf("ConnCount = %d, want %d (gRPC listener also counted)", c.ConnCount(), before+2)
	}
}

func TestUnreachableEndpoint(t *testing.T) { // AC-14, ADR-0005
	ep := otlpcollector.UnreachableEndpoint(t)
	host, port, err := net.SplitHostPort(hostPort(ep))
	if err != nil {
		t.Fatalf("endpoint %q: %v", ep, err)
	}
	if host != "127.0.0.1" || port == "" || port == "0" {
		t.Errorf("endpoint = %q, want 127.0.0.1:<port>", ep)
	}
	conn, err := net.DialTimeout("tcp", hostPort(ep), time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("dial to unreachable endpoint %q succeeded", ep)
	}
	if ep2 := otlpcollector.UnreachableEndpoint(t); ep2 == "" {
		t.Errorf("second call returned empty endpoint")
	}
}

func TestCloseStopsListenersAndIsRepeatable(t *testing.T) {
	c := otlpcollector.Start(t)
	h, g := hostPort(c.HTTPEndpoint()), hostPort(c.GRPCEndpoint())
	c.Close()
	c.Close() // also runs again from the tb cleanup; must not panic
	for _, ep := range []string{h, g} {
		if conn, err := net.DialTimeout("tcp", ep, time.Second); err == nil {
			conn.Close()
			t.Errorf("listener %s still accepting after Close", ep)
		}
	}
}

func TestRecordedDataSurvivesClose(t *testing.T) {
	c := otlpcollector.Start(t)
	if err := export(newHTTPExp(t, c), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(c.Spans()) != 1 || len(c.Requests()) != 1 {
		t.Errorf("recorded data lost after Close")
	}
}

func TestIndependentCollectors(t *testing.T) {
	a, b := otlpcollector.Start(t), otlpcollector.Start(t)
	if hostPort(a.HTTPEndpoint()) == hostPort(b.HTTPEndpoint()) {
		t.Fatalf("two collectors share an endpoint")
	}
	if err := export(newHTTPExp(t, a), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(b.Spans()) != 0 || b.ConnCount() != 0 {
		t.Errorf("collector b saw traffic meant for a")
	}
}
