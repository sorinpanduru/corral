package otlpcollector_test

// Self-tests for recording of span links and instrumentation scope (T24.5)
// over OTLP/HTTP and OTLP/gRPC. All data is synthetic.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

func linkStubs() tracetest.SpanStubs {
	now := time.Now()
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("a1a2a3a4a5a6a7a8")
	return tracetest.SpanStubs{{
		Name:      "linked.span",
		StartTime: now.Add(-time.Second),
		EndTime:   now,
		Links: []sdktrace.Link{{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}),
			Attributes: []attribute.KeyValue{
				attribute.String("link.key", "link-value"),
				attribute.StringSlice("link.arr", []string{"a", "b"}),
			},
		}},
		InstrumentationScope: instScope(),
	}, {
		Name:                 "plain.span",
		StartTime:            now.Add(-time.Second),
		EndTime:              now,
		InstrumentationScope: instScope(),
	}}
}

func checkLinksScope(t *testing.T, c *otlpcollector.Collector) {
	t.Helper()
	spans := c.Spans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	var linked, plain otlpcollector.RecordedSpan
	for _, s := range spans {
		if s.Name == "linked.span" {
			linked = s
		} else {
			plain = s
		}
	}
	want := []otlpcollector.RecordedLink{{
		TraceID:    "0102030405060708090a0b0c0d0e0f10",
		SpanID:     "a1a2a3a4a5a6a7a8",
		Attributes: map[string]string{"link.key": "link-value", "link.arr": `["a","b"]`},
	}}
	if !reflect.DeepEqual(linked.Links, want) {
		t.Errorf("links = %+v, want %+v", linked.Links, want)
	}
	if len(plain.Links) != 0 {
		t.Errorf("plain span has links: %+v", plain.Links)
	}
	wantScope := otlpcollector.RecordedScope{
		Name: "synthetic/scope", Version: "1.2.3",
		Attributes: map[string]string{"scope.key": "scope-value"},
	}
	for _, s := range spans {
		if !reflect.DeepEqual(s.Scope, wantScope) {
			t.Errorf("span %q scope = %+v, want %+v", s.Name, s.Scope, wantScope)
		}
	}

	// Deep copy of links and scope attributes.
	linked.Links[0].Attributes["link.key"] = "mutated"
	for _, s := range spans {
		s.Scope.Attributes["scope.key"] = "mutated"
	}
	again := c.Spans()
	for _, s := range again {
		if s.Scope.Attributes["scope.key"] != "scope-value" {
			t.Errorf("Scope not deep copied: %+v", s.Scope)
		}
		if s.Name == "linked.span" && s.Links[0].Attributes["link.key"] != "link-value" {
			t.Errorf("Links not deep copied: %+v", s.Links)
		}
	}
}

func exportLinks(t *testing.T, e exporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.ExportSpans(ctx, linkStubs().Snapshots()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPRecordsLinksAndScope(t *testing.T) {
	c := otlpcollector.Start(t)
	exportLinks(t, newHTTPExp(t, c))
	checkLinksScope(t, c)
}

func TestGRPCRecordsLinksAndScope(t *testing.T) {
	c := otlpcollector.Start(t)
	exportLinks(t, newGRPCExp(t, c))
	checkLinksScope(t, c)
}

func instScope() instrumentation.Scope {
	return instrumentation.Scope{
		Name: "synthetic/scope", Version: "1.2.3",
		Attributes: attribute.NewSet(attribute.String("scope.key", "scope-value")),
	}
}
