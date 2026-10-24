package tracing_test

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/grpclog"

	"github.com/sorinpanduru/corral/internal/infra/osfile"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const (
	msgSlogDefault = "slog default probe"
	msgStdLog      = "std log probe"
	msgGRPCProbe   = "grpc probe"
)

// Harness pin (review item 6): every global that logbridge.Install sets is
// wired by boot, observed through one record each with the right component.
func TestHarnessInstallsEveryBridge(t *testing.T) {
	h := boot(t, opts{on: false})
	otel.Handle(errors.New("probe"))
	slog.Warn(msgSlogDefault)
	log.Print(msgStdLog)
	grpclog.Warning(msgGRPCProbe)

	want := map[string]int{"otel": 1, "thirdparty": 2, "grpc": 1}
	got := map[string]int{}
	for _, r := range h.records() {
		got[r.str("component")]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("component=%s records = %d, want %d", k, got[k], n)
		}
	}
	h.finish()
}

// Review item 2 (D): OTEL_TRACES_EXPORTER=none with an exporter override
// exports nothing and installs no global provider.
func TestNoneWithExporterOverrideExportsNothing(t *testing.T) {
	rec := tracetest.NewInMemoryExporter()
	h := boot(t, opts{on: true, exporter: rec, env: map[string]string{"OTEL_TRACES_EXPORTER": "none"}})
	if !h.tr.Enabled() {
		t.Fatal("Enabled() = false with none")
	}
	h.emit("none-span")
	_ = h.flush()
	_ = h.shutdown()
	if n := len(rec.GetSpans()); n != 0 {
		t.Errorf("override exporter received %d spans with exporter=none", n)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Error("a global SDK provider was set with exporter=none")
	}
	h.finish()
}

// Review item 3 (D): enabled and exporting without a metrics factory is an
// error with constant text.
func TestStartWithoutMetricsFactoryFails(t *testing.T) {
	cases := map[string]sdktrace.SpanExporter{"override": tracetest.NewInMemoryExporter(), "otlp": nil}
	for name, ov := range cases {
		t.Run(name, func(t *testing.T) {
			restoreGlobals(t)
			m := newMarker(t)
			col := otlpcollector.Start(t)
			setEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://synthuser:" + m + "@" + col.HTTPEndpoint()})
			st := tracing.ResolveInputs{Enabled: true, SampleRatio: 1.0}
			enabled, cfg, errs := tracing.Resolve(st, os.LookupEnv, osfile.Read)
			if len(errs) > 0 || !enabled {
				t.Fatalf("Resolve: enabled=%v errors=%d", enabled, len(errs))
			}
			tr, err := tracing.Start(context.Background(), tracing.Options{
				Env: os.LookupEnv, Config: cfg, Enabled: true, Exporter: ov,
			})
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = tr.Shutdown(ctx)
				t.Fatal("Start without Options.Metrics succeeded, want an error")
			}
			if strings.Contains(err.Error(), m) {
				t.Error("Start error text contains the endpoint userinfo marker")
			}
		})
	}
}

type flushSpy struct {
	*tracetest.InMemoryExporter
	flushes int
}

func (f *flushSpy) ForceFlush(context.Context) error { f.flushes++; return nil }

// Review item 4: Tracing.ForceFlush reaches an override exporter that
// implements ForceFlush.
func TestForceFlushForwardsToExporter(t *testing.T) {
	spy := &flushSpy{InMemoryExporter: tracetest.NewInMemoryExporter()}
	h := boot(t, opts{on: true, exporter: spy})
	h.emit("flush-span")
	if err := h.flush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if spy.flushes < 1 {
		t.Errorf("exporter ForceFlush calls = %d, want >= 1", spy.flushes)
	}
	h.finish()
}

// Review item 5 (D, security A1): Shutdown and ForceFlush errors carry
// constant text. An unresponsive collector at an endpoint holding a marker
// makes both fail; neither error text holds the marker or endpoint.
func TestShutdownAndFlushErrorsHaveConstantText(t *testing.T) {
	m := newMarker(t)
	col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	env := failureEnv("http/protobuf", "http://synthuser:"+m+"@"+col.HTTPEndpoint())
	env["OTEL_EXPORTER_OTLP_TIMEOUT"] = "30000"
	h := boot(t, opts{on: true, env: env})
	h.emit("stuck")

	check := func(what string, err error) {
		if err == nil {
			return // nothing to inspect
		}
		for _, bad := range []string{m, col.HTTPEndpoint(), "synthuser"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("%s error text contains endpoint data", what)
			}
		}
	}
	fctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	check("ForceFlush", h.tr.ForceFlush(fctx))
	cancel()
	h.shutDown = true
	sctx, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	check("Shutdown", h.tr.Shutdown(sctx))
	cancel2()
	h.finish()
}
