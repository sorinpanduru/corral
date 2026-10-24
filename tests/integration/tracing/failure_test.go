package tracing_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const rateWindow = 30 * time.Second

// failureEnv bounds every export attempt so unreachable collectors fail fast.
func failureEnv(proto, endpoint string) map[string]string {
	return map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
		"OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
		"OTEL_EXPORTER_OTLP_TIMEOUT":  "300",
		"OTEL_BSP_EXPORT_TIMEOUT":     "2000",
		"OTEL_BSP_SCHEDULE_DELAY":     "60000", // only explicit flushes export
	}
}

// failFlushes ends one span and flushes, n times. Flush errors are expected.
func failFlushes(h *harness, n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.emit("failing-export")
		_ = h.flush()
	}
}

func assertOtelWarnings(t *testing.T, h *harness, wantCount int) []record {
	t.Helper()
	rs := h.otelRecords()
	if len(rs) != wantCount {
		t.Fatalf("component=otel records = %d, want %d", len(rs), wantCount)
	}
	for _, r := range rs {
		if r.Level.String() != "WARN" {
			t.Errorf("otel record level = %v, want WARN", r.Level)
		}
		if _, ok := r.Attrs["suppressed_count"]; !ok {
			t.Error("otel warning lacks suppressed_count")
		}
	}
	return rs
}

// AC-3 integration (FR-28, FR-33, tracing bootstrap): with an unreachable
// collector, test spans are created and flushed; repeated export failures
// give one fixed-message component=otel warning per 30 s window with a
// suppressed_count; every stdout line is JSON; nothing goes to stderr; the
// dial address is never echoed.
func TestAC3_UnreachableCollectorLogsOnlyRateLimitedFixedWarnings(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			addr := otlpcollector.UnreachableEndpoint(t)
			h := boot(t, opts{on: true, env: failureEnv(proto, "http://"+addr)})

			failFlushes(h, 4)
			first := assertOtelWarnings(t, h, 1)
			if got := first[0].Attrs["suppressed_count"].Int64(); got != 0 {
				t.Errorf("first warning suppressed_count = %d, want 0", got)
			}

			h.clk.Advance(rateWindow)
			failFlushes(h, 1)
			rs := assertOtelWarnings(t, h, 2)
			if got := rs[1].Attrs["suppressed_count"].Int64(); got < 1 {
				t.Errorf("second warning suppressed_count = %d, want >= 1 (failures in the first window)", got)
			}
			if rs[0].Msg != rs[1].Msg || rs[0].Msg == "" {
				t.Errorf("otel warnings do not share one fixed message: %q vs %q", rs[0].Msg, rs[1].Msg)
			}

			stdout, stderr := h.finish()
			if len(stderr) != 0 {
				t.Errorf("stderr has %d bytes", len(stderr))
			}
			if strings.Contains(string(stdout), addr) {
				t.Error("the unreachable endpoint address was echoed on stdout")
			}
			for _, r := range h.records() {
				if !allowedFailureComponent(r.str("component")) {
					t.Errorf("unexpected component %q during export failure", r.str("component"))
				}
			}
		})
	}
}

func allowedFailureComponent(c string) bool {
	switch c {
	case "otel", "grpc", "tracing", "thirdparty":
		return true
	}
	return false
}

// AC-3 integration and D3: a collector that rejects requests yields only
// component=otel warnings, with a fixed message, no stderr, and the error
// classified as protocol/other (ADR-0007 e).
func TestAC3_RejectingCollectorIsClassifiedProtocol(t *testing.T) {
	cases := []struct {
		name  string
		proto string
		opts  []otlpcollector.Option
		url   func(*otlpcollector.Collector) string
	}{
		{"http 400", "http/protobuf", []otlpcollector.Option{otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(400)}, httpURL},
		{"http 401", "http/protobuf", []otlpcollector.Option{otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(401)}, httpURL},
		{"grpc InvalidArgument", "grpc", []otlpcollector.Option{otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithGRPCRejectCode(codes.InvalidArgument)}, grpcURL},
		{"grpc PermissionDenied", "grpc", []otlpcollector.Option{otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithGRPCRejectCode(codes.PermissionDenied)}, grpcURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := otlpcollector.Start(t, c.opts...)
			h := boot(t, opts{on: true, env: failureEnv(c.proto, c.url(col))})
			failFlushes(h, 3)
			rs := assertOtelWarnings(t, h, 1)
			r := rs[0]
			if got := r.str("error"); got != string(errclass.ClassProtocol) {
				t.Errorf("error class = %q, want %q", got, errclass.ClassProtocol)
			}
			if got := r.str("corral.error.detail"); got != string(errclass.DetailOther) {
				t.Errorf("error detail = %q, want %q", got, errclass.DetailOther)
			}
			if n := len(col.Spans()); n != 0 {
				t.Errorf("rejecting collector recorded %d spans", n)
			}
			h.finish()
		})
	}
}
