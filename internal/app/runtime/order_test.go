package runtime_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// FR-28 / ADR-0006 step 2, AC-3: logbridge is installed before any grpc, otel,
// tracing or admin-listener code, so nothing reaches stderr even with tracing
// on and an unreachable collector, for both OTLP protocols. Every line written
// to the supplied stdout writer is JSON. Not parallel: CaptureStdio swaps
// process-wide state, and Run installs process-wide bridges.
func TestNoStderrWithTracingAndUnreachableCollector(t *testing.T) { // AC-3, FR-28
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			stop := markerscan.CaptureStdio(t)
			ep := otlpcollector.UnreachableEndpoint(t)
			h := run(t, runOpts{
				args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
				env: map[string]string{
					"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + ep,
					"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
				},
			})
			hd := h.waitStarted()
			_, span := hd.TracerProvider.Tracer("t18-test").Start(context.Background(), "t18-span",
				trace.WithSpanKind(trace.SpanKindInternal))
			span.End()
			time.Sleep(300 * time.Millisecond)
			if code := h.stop(); code != 0 {
				t.Errorf("exit code = %d, want 0 (graceful shutdown with an unreachable collector)", code)
			}
			_, stderr := stop()
			if len(stderr) != 0 {
				t.Errorf("process wrote %d bytes to stderr, want 0", len(stderr))
			}
			h.out.records(t) // every stdout line must be JSON
			if len(h.out.lines()) == 0 {
				t.Errorf("no log output at all on the supplied writer")
			}
		})
	}
}
