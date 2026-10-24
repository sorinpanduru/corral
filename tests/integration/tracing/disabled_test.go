package tracing_test

import (
	"context"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// AC-14 (FR-19, FR-20): in each setup, test spans are created, a flush and
// shutdown are forced, nothing reaches the collector, and the collector accepts
// zero TCP connections. ConnCount is read before Close (T13 decision).
func TestAC14_NoConnectionsWhenTracingIsOffOrNone(t *testing.T) {
	cases := []struct {
		name          string
		on            bool
		env           map[string]string
		wantEnabled   bool
		wantRecording bool // the provider creates recording spans only for "none"
	}{
		{"default settings", false, nil, false, false},
		{"OTEL_SDK_DISABLED=TRUE beats tracing.enabled", true, map[string]string{"OTEL_SDK_DISABLED": "TRUE"}, false, false},
		{"OTEL_SDK_DISABLED=true beats tracing.enabled", true, map[string]string{"OTEL_SDK_DISABLED": "true"}, false, false},
		{"OTEL_TRACES_EXPORTER=none", true, map[string]string{"OTEL_TRACES_EXPORTER": "none"}, true, true},
	}
	for _, protocol := range []string{"http/protobuf", "grpc"} {
		for _, tc := range cases {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				c := otlpcollector.Start(t)
				env := map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": protocol}
				if protocol == "grpc" {
					env["OTEL_EXPORTER_OTLP_ENDPOINT"] = grpcURL(c)
				} else {
					env["OTEL_EXPORTER_OTLP_ENDPOINT"] = httpURL(c)
				}
				for k, v := range tc.env {
					env[k] = v
				}
				h := boot(t, opts{on: tc.on, env: env})
				if h.tr.Enabled() != tc.wantEnabled {
					t.Fatalf("Enabled() = %v, want %v", h.tr.Enabled(), tc.wantEnabled)
				}

				tr := h.tr.TracerProvider().Tracer("corral-test")
				_, sp := tr.Start(context.Background(), "ac14-span")
				if sp.IsRecording() != tc.wantRecording {
					t.Errorf("IsRecording() = %v, want %v", sp.IsRecording(), tc.wantRecording)
				}
				sp.End()

				if err := h.flush(); err != nil {
					t.Errorf("ForceFlush returned %v, want nil with nothing to export", err)
				}
				if err := h.shutdown(); err != nil {
					t.Errorf("Shutdown returned %v, want nil", err)
				}
				if n := c.ConnCount(); n != 0 {
					t.Errorf("collector accepted %d TCP connections, want 0", n)
				}
				if got := c.Spans(); len(got) != 0 {
					t.Errorf("collector recorded %d spans, want 0", len(got))
				}
				if got := len(c.Requests()); got != 0 {
					t.Errorf("collector received %d requests, want 0", got)
				}
				h.finish()
			})
		}
	}
}

// AC-14 (unit part restated at bootstrap level): OTEL_SDK_DISABLED=yes counts
// as false, so tracing stays on and Start does not fail.
func TestAC14_SDKDisabledYesIsNotDisabled(t *testing.T) {
	c := otlpcollector.Start(t)
	h := boot(t, opts{on: true, env: map[string]string{
		"OTEL_SDK_DISABLED":           "yes",
		"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c),
	}})
	if !h.tr.Enabled() {
		t.Fatal("Enabled() = false with OTEL_SDK_DISABLED=yes, want true")
	}
	h.emit("yes-span")
	if err := h.flush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	waitFor(t, "span", func() bool { return hasSpan(c, "yes-span") })
	h.finish()
}

// FR-19 (ADR-0007): disabled tracing returns the OTel no-op provider,
// Enabled() is false, and the test-only exporter override is ignored, so no
// span is ever created or exported and nothing is installed globally.
func TestFR19_DisabledReturnsNoopProviderAndIgnoresExporterOverride(t *testing.T) {
	rec := tracetest.NewInMemoryExporter()
	h := boot(t, opts{on: false, exporter: rec})

	if h.tr.Enabled() {
		t.Fatal("Enabled() = true with tracing off")
	}
	tp := h.tr.TracerProvider()
	if reflect.TypeOf(tp) != reflect.TypeOf(noop.NewTracerProvider()) {
		t.Errorf("TracerProvider() has type %T, want the OTel no-op provider", tp)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Error("an SDK provider was installed as the global provider while tracing is off")
	}
	h.emit("never-recorded")
	if err := h.flush(); err != nil {
		t.Errorf("ForceFlush on disabled tracing: %v", err)
	}
	if err := h.shutdown(); err != nil {
		t.Errorf("Shutdown on disabled tracing: %v", err)
	}
	if n := len(rec.GetSpans()); n != 0 {
		t.Errorf("override exporter received %d spans while tracing is off", n)
	}
	h.finish()
}

// FR-19 / FR-8 (ADR-0007): with tracing on the SDK provider is returned and
// registered as the global provider, and Enabled() is true, including with
// OTEL_TRACES_EXPORTER=none.
func TestFR19_EnabledExposesSDKProviderAndSetsGlobal(t *testing.T) {
	for _, exporter := range []string{"otlp", "none"} {
		t.Run(exporter, func(t *testing.T) {
			rec := tracetest.NewInMemoryExporter()
			var ov sdktrace.SpanExporter
			if exporter == "otlp" {
				ov = rec // keeps the test off the network
			}
			h := boot(t, opts{on: true, env: map[string]string{"OTEL_TRACES_EXPORTER": exporter}, exporter: ov})
			if !h.tr.Enabled() {
				t.Fatal("Enabled() = false with tracing on")
			}
			if _, ok := h.tr.TracerProvider().(*sdktrace.TracerProvider); !ok {
				t.Errorf("TracerProvider() has type %T, want *sdktrace.TracerProvider", h.tr.TracerProvider())
			}
			if exporter == "otlp" && otel.GetTracerProvider() != h.tr.TracerProvider() {
				t.Error("the global provider is not the bootstrap provider")
			}
			h.finish()
		})
	}
}
