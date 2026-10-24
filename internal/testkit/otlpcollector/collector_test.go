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
//   RecordedSpan also has StatusCode (int32, the OTLP status code number, 2 = Error) and StatusMessage string;
//   const MaxBodyBytes (4 MiB, untyped/int constant) caps HTTP bodies, also after gunzip;
//   RequestInfo.Protocol is a string ("http" / "grpc"), ContentEncoding a string.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

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
		Status:   sdktrace.Status{Code: otelcodes.Error, Description: "synthetic status text"},
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
	// Status is recorded: StatusCode is the OTLP status code number (2 = Error),
	// StatusMessage the description.
	if s.StatusCode != int32(tracev1.Status_STATUS_CODE_ERROR) || s.StatusMessage != "synthetic status text" {
		t.Errorf("status = %q / %q", s.StatusCode, s.StatusMessage)
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
	for _, st := range []int{503, 400} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(st))
			err := export(newHTTPExp(t, c), 5*time.Second)
			if err == nil {
				t.Fatalf("export succeeded against rejecting collector")
			}
			// The configured status must reach the client. The exporter's
			// error text does not reliably include it (retryable statuses
			// are wrapped), so observe the raw response status.
			if got := postRaw(t, c, bytes.NewReader(nil), ""); got != st {
				t.Errorf("raw response status = %d, want configured %d", got, st)
			}
			if n := len(c.Spans()); n != 0 {
				t.Errorf("rejected request recorded %d spans", n)
			}
		})
	}
}

func TestRejectGRPC(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithGRPCRejectCode(code))
			err := export(newGRPCExp(t, c), 5*time.Second)
			if err == nil {
				t.Fatalf("export succeeded against rejecting collector")
			}
			got := codes.Unknown
			if st, ok := status.FromError(err); ok {
				got = st.Code()
			}
			if got != code && !strings.Contains(err.Error(), code.String()) {
				t.Errorf("error %v does not carry configured code %v", err, code)
			}
			if n := len(c.Spans()); n != 0 {
				t.Errorf("rejected request recorded %d spans", n)
			}
		})
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

func waitRequests(t *testing.T, c *otlpcollector.Collector) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.Requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("request never reached the collector")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A request held in unresponsive mode must be failed (not implicitly
// acknowledged) when the collector is closed, and Close must return promptly.
func TestUnresponsiveInFlightFailsOnClose(t *testing.T) {
	for name, mk := range map[string]func(*testing.T, *otlpcollector.Collector) exporter{
		"http": func(t *testing.T, c *otlpcollector.Collector) exporter {
			return newHTTPExp(t, c, otlptracehttp.WithTimeout(30*time.Second))
		},
		"grpc": func(t *testing.T, c *otlpcollector.Collector) exporter {
			return newGRPCExp(t, c, otlptracegrpc.WithTimeout(30*time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			e := mk(t, c)
			errc := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				errc <- e.ExportSpans(ctx, stubs(nil).Snapshots())
			}()
			waitRequests(t, c)
			select {
			case err := <-errc:
				t.Fatalf("export returned before Close: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			done := make(chan struct{})
			go func() { c.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Close did not return promptly with a held request")
			}
			select {
			case err := <-errc:
				if err == nil {
					t.Errorf("held request reported success after Close")
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("held request not released by Close")
			}
			if n := len(c.Spans()); n != 0 {
				t.Errorf("held request recorded %d spans", n)
			}
		})
	}
}

func postRaw(t *testing.T, c *otlpcollector.Collector, body io.Reader, encoding string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+hostPort(c.HTTPEndpoint())+"/v1/traces", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		// The server may close the connection after answering 413 while the
		// client is still writing; treat as a distinct, reported failure.
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestHTTPBodyOverLimitRejected(t *testing.T) { // security: bounded request size
	c := otlpcollector.Start(t)
	body := bytes.Repeat([]byte{0}, otlpcollector.MaxBodyBytes+1)
	if got := postRaw(t, c, bytes.NewReader(body), ""); got != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", got)
	}
	if n := len(c.Spans()); n != 0 {
		t.Errorf("oversize body recorded %d spans", n)
	}
}

func TestHTTPGzipBombRejectedWithBoundedMemory(t *testing.T) { // security
	c := otlpcollector.Start(t)
	var comp bytes.Buffer
	zw := gzip.NewWriter(&comp)
	chunk := make([]byte, 1<<20)
	const expanded = 64 << 20 // far beyond MaxBodyBytes
	for w := 0; w < expanded; w += len(chunk) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if comp.Len() > 1<<20 {
		t.Fatalf("test bomb not small enough: %d bytes", comp.Len())
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := postRaw(t, c, bytes.NewReader(comp.Bytes()), "gzip")
	runtime.ReadMemStats(&after)
	if got != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", got)
	}
	if n := len(c.Spans()); n != 0 {
		t.Errorf("gzip bomb recorded %d spans", n)
	}
	// Decompression must be capped near MaxBodyBytes, not run to 64 MiB.
	if d := after.TotalAlloc - before.TotalAlloc; d > 4*otlpcollector.MaxBodyBytes {
		t.Errorf("allocated %d bytes handling the bomb, want <= %d", d, 4*otlpcollector.MaxBodyBytes)
	}
}

func TestCloseWaitsForHandlersAndLeaksNoGoroutines(t *testing.T) {
	settle := func(max int) int {
		deadline := time.Now().Add(5 * time.Second)
		n := runtime.NumGoroutine()
		for n > max && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}
	runtime.GC()
	baseline := runtime.NumGoroutine()

	c := otlpcollector.Start(t)
	he, ge := newHTTPExp(t, c), newGRPCExp(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = export(he, 5*time.Second) }()
		go func() { defer wg.Done(); _ = export(ge, 5*time.Second) }()
	}
	c.Close()
	afterClose := len(c.Spans())
	wg.Wait()
	_ = he.Shutdown(context.Background())
	_ = ge.Shutdown(context.Background())
	time.Sleep(200 * time.Millisecond)
	if n := len(c.Spans()); n != afterClose {
		t.Errorf("spans recorded after Close returned: %d -> %d", afterClose, n)
	}
	// Small slack for runtime/grpc internals that exit asynchronously.
	if n := settle(baseline + 2); n > baseline+2 {
		t.Errorf("goroutines = %d after Close, baseline %d", n, baseline)
	}
}

func mutateMap(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Map {
		return
	}
	for _, k := range rv.MapKeys() {
		rv.SetMapIndex(k, reflect.Value{})
	}
}

func TestGettersReturnDeepCopies(t *testing.T) {
	c := otlpcollector.Start(t)
	if err := exportWithRes(t, newHTTPExp(t, c, otlptracehttp.WithHeaders(map[string]string{"x-synthetic": "v"}))); err != nil {
		t.Fatal(err)
	}
	s1 := c.Spans()
	s1[0].Name = "mutated"
	s1[0].Attributes["synthetic.key"] = "mutated"
	s1[0].Attributes["extra"] = "x"
	s1[0].Events[0].Name = "mutated"
	s1[0].Events[0].Attributes["event.key"] = "mutated"
	s2 := c.Spans()
	if len(s2) != 1 || s2[0].Name != "synthetic.span" || s2[0].Attributes["synthetic.key"] != "synthetic-value" ||
		s2[0].Attributes["extra"] != "" || s2[0].Events[0].Name != "synthetic.event" ||
		s2[0].Events[0].Attributes["event.key"] != "event-value" {
		t.Errorf("Spans() not a deep copy: %+v", s2)
	}

	r1 := c.Resources()
	r1[0]["service.name"] = "mutated"
	delete(r1[0], "deployment.environment")
	if r2 := c.Resources(); r2[0]["service.name"] != "corral-selftest" || r2[0]["deployment.environment"] != "synthetic" {
		t.Errorf("Resources() not a deep copy: %v", r2)
	}

	q1 := c.Requests()
	mutateMap(q1[0].Headers)
	q1[0].ContentEncoding = "mutated"
	q2 := c.Requests()
	if q2[0].ContentEncoding == "mutated" || !strings.Contains(strings.ToLower(fmt.Sprint(q2[0].Headers)), "x-synthetic") {
		t.Errorf("Requests() not a deep copy: %+v", q2[0])
	}
}

// Attribute value forms (the contract for RecordedSpan.Attributes and
// Resources): string as is; int "42"; bool "true"/"false"; double via
// strconv.FormatFloat(v, 'g', -1, 64) ("1.5"); bytes standard base64 with
// padding; array and kvlist as compact JSON of the natural types
// (["a","b"], [1,2], {"k1":"v1","k2":2} with keys sorted).
func TestNonStringAttributeForms(t *testing.T) {
	str := func(v string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}
	}
	num := func(v int64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}
	}
	kv := func(k string, v *commonpb.AnyValue) *commonpb.KeyValue { return &commonpb.KeyValue{Key: k, Value: v} }
	attrs := []*commonpb.KeyValue{
		kv("a.int", num(42)),
		kv("a.bool", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}),
		kv("a.double", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}),
		kv("a.bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("synth")}}),
		kv("a.array", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{str("a"), str("b")}}}}),
		kv("a.intarray", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{num(1), num(2)}}}}),
		kv("a.kvlist", &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kv("k2", num(2)), kv("k1", str("v1"))}}}}),
		kv("a.str", str("plain")),
	}
	req := &collectorpb.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("r.int", num(7)), kv("service.name", str("corral-selftest"))}},
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{
			Name:              "typed.span",
			TraceId:           bytes.Repeat([]byte{1}, 16),
			SpanId:            bytes.Repeat([]byte{2}, 8),
			StartTimeUnixNano: 1,
			EndTimeUnixNano:   2,
			Attributes:        attrs,
			Status:            &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: "synthetic status text"},
		}}}},
	}}}
	raw, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	c := otlpcollector.Start(t)
	if got := postRaw(t, c, bytes.NewReader(raw), ""); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	spans := c.Spans()
	if len(spans) != 1 {
		t.Fatalf("Spans() len = %d", len(spans))
	}
	want := map[string]string{
		"a.int":      "42",
		"a.bool":     "true",
		"a.double":   "1.5",
		"a.bytes":    base64.StdEncoding.EncodeToString([]byte("synth")),
		"a.array":    `["a","b"]`,
		"a.intarray": `[1,2]`,
		"a.kvlist":   `{"k1":"v1","k2":2}`,
		"a.str":      "plain",
	}
	for k, v := range want {
		if got := spans[0].Attributes[k]; got != v {
			t.Errorf("attribute %s = %q, want %q", k, got, v)
		}
	}
	if spans[0].StatusCode != int32(tracev1.Status_STATUS_CODE_ERROR) || spans[0].StatusMessage != "synthetic status text" {
		t.Errorf("status = %q / %q", spans[0].StatusCode, spans[0].StatusMessage)
	}
	if res := c.Resources(); len(res) == 0 || res[0]["r.int"] != "7" {
		t.Errorf("resource attributes = %v", res)
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

// Reject body and message options: the configured synthetic text is echoed.
func TestRejectEchoesConfiguredText(t *testing.T) {
	const text = "echo-synthetic-text-0123456789"
	c := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject),
		otlpcollector.WithHTTPRejectStatus(401), otlpcollector.WithHTTPRejectBody(text),
		otlpcollector.WithGRPCRejectCode(codes.Unauthenticated), otlpcollector.WithGRPCRejectMessage(text))

	resp, err := http.Post("http://"+c.HTTPEndpoint()+"/v1/traces", "application/x-protobuf", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(string(body), text) {
		t.Errorf("HTTP reject: status %d body %q, want 401 echoing the configured body", resp.StatusCode, body)
	}

	err = export(newGRPCExp(t, c), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Errorf("gRPC reject error %v does not carry the configured message", err)
	}
}
