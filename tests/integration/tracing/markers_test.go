package tracing_test

import (
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// scanAll feeds stdout, stderr, the metrics text and the collector payloads to
// the scanner and fails on any marker finding. /readyz belongs to the admin
// listener (T17) and is not part of the tracing bootstrap.
func scanAll(t *testing.T, h *harness, col *otlpcollector.Collector, markers ...string) {
	t.Helper()
	stdout, stderr := h.finish()
	sc := markerscan.New()
	for _, m := range markers {
		sc.AddMarker(t, m)
	}
	sc.Add(markerscan.SourceStdout, stdout)
	sc.Add(markerscan.SourceStderr, stderr)
	sc.Add(markerscan.SourceMetrics, h.metricsText())
	if col != nil {
		sc.AddCollector(col)
	}
	sc.AssertClean(t)
}

// AC-38 (FR-24, FR-28, FR-33): an OTLP header value (authorization=<marker>)
// appears nowhere scanned, with debug logging, both against a collector that
// rejects requests and against an unreachable one, over both protocols. The
// header must really have been sent to the rejecting collector, so the scan is
// not vacuous.
func TestAC38_HeaderMarkerDoesNotLeak(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto+"/rejecting", func(t *testing.T) {
			m := newMarker(t)
			var col *otlpcollector.Collector
			var ep string
			if proto == "grpc" {
				col = otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithGRPCRejectCode(codes.PermissionDenied))
				ep = grpcURL(col)
			} else {
				col = otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeReject), otlpcollector.WithHTTPRejectStatus(401))
				ep = httpURL(col)
			}
			env := failureEnv(proto, ep)
			env["OTEL_EXPORTER_OTLP_HEADERS"] = "authorization=" + m
			h := boot(t, opts{on: true, level: slog.LevelDebug, env: env})
			failFlushes(h, 2)

			sent := false
			for _, r := range col.Requests() {
				for _, v := range headerValues(r, "authorization") {
					if v == m {
						sent = true
					}
				}
			}
			if !sent {
				t.Fatal("precondition: the configured header never reached the collector")
			}
			scanAll(t, h, col, m)
		})
		t.Run(proto+"/unreachable", func(t *testing.T) {
			m := newMarker(t)
			addr := otlpcollector.UnreachableEndpoint(t)
			env := failureEnv(proto, "http://"+addr)
			env["OTEL_EXPORTER_OTLP_HEADERS"] = "authorization=" + m
			h := boot(t, opts{on: true, level: slog.LevelDebug, env: env})
			failFlushes(h, 2)
			scanAll(t, h, nil, m)
		})
	}
}

// AC-38 (FR-24, FR-28, FR-33): a userinfo marker inside the OTLP endpoint URL
// appears nowhere scanned, over http/protobuf and grpc, for a working and an
// unreachable collector. Debug logging is on.
func TestAC38_EndpointUserinfoMarkerDoesNotLeak(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto+"/accepting", func(t *testing.T) {
			m := newMarker(t)
			col := otlpcollector.Start(t)
			hostport := col.HTTPEndpoint()
			if proto == "grpc" {
				hostport = col.GRPCEndpoint()
			}
			env := failureEnv(proto, "http://synthuser:"+m+"@"+hostport)
			h := boot(t, opts{on: true, level: slog.LevelDebug, env: env})
			h.emit("userinfo-span")
			_ = h.flush()
			waitFor(t, "span delivery", func() bool { return hasSpan(col, "userinfo-span") })
			scanAll(t, h, col, m)
		})
		t.Run(proto+"/unreachable", func(t *testing.T) {
			m := newMarker(t)
			addr := otlpcollector.UnreachableEndpoint(t)
			env := failureEnv(proto, "http://synthuser:"+m+"@"+addr)
			h := boot(t, opts{on: true, level: slog.LevelDebug, env: env})
			failFlushes(h, 2)
			scanAll(t, h, nil, m)
		})
	}
}
