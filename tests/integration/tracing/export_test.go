package tracing_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// AC-15 (FR-20): spans arrive over the chosen protocol at the chosen endpoint,
// with gzip honoured. Each case ends spans, forces a flush, and checks the
// protocol and content encoding the collector saw.
func TestAC15_ProtocolEndpointAndCompression(t *testing.T) {
	type tc struct {
		name     string
		env      func(c, decoy *otlpcollector.Collector) map[string]string
		wantGRPC bool
		wantGzip bool
	}
	cases := []tc{
		{"default is http/protobuf", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c)}
		}, false, false},
		{"explicit http/protobuf", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c), "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}
		}, false, false},
		{"OTEL_TRACES_EXPORTER=otlp behaves like unset", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c), "OTEL_TRACES_EXPORTER": "otlp"}
		}, false, false},
		{"grpc via OTEL_EXPORTER_OTLP_PROTOCOL", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": grpcURL(c), "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}
		}, true, false},
		{"grpc with OTEL_TRACES_EXPORTER=otlp", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": grpcURL(c), "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_TRACES_EXPORTER": "otlp"}
		}, true, false},
		{"TRACES_PROTOCOL=grpc beats PROTOCOL=http/protobuf", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        grpcURL(c),
				"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
				"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc",
			}
		}, true, false},
		{"TRACES_PROTOCOL=http/protobuf beats PROTOCOL=grpc", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        httpURL(c),
				"OTEL_EXPORTER_OTLP_PROTOCOL":        "grpc",
				"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf",
			}
		}, false, false},
		{"TRACES_ENDPOINT beats ENDPOINT (http)", func(c, decoy *otlpcollector.Collector) map[string]string {
			return map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        httpURL(decoy),
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": httpURL(c) + "/v1/traces",
			}
		}, false, false},
		{"TRACES_ENDPOINT beats ENDPOINT (grpc)", func(c, decoy *otlpcollector.Collector) map[string]string {
			return map[string]string{
				"OTEL_EXPORTER_OTLP_PROTOCOL":        "grpc",
				"OTEL_EXPORTER_OTLP_ENDPOINT":        grpcURL(decoy),
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": grpcURL(c),
			}
		}, true, false},
		{"gzip over http", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c), "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip"}
		}, false, true},
		{"gzip over grpc", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": grpcURL(c), "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip"}
		}, true, true},
		{"gzip via TRACES_COMPRESSION", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c), "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION": "gzip"}
		}, false, true},
		{"compression none", func(c, _ *otlpcollector.Collector) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(c), "OTEL_EXPORTER_OTLP_COMPRESSION": "none"}
		}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := otlpcollector.Start(t)
			decoy := otlpcollector.Start(t)
			env := c.env(col, decoy)
			env["OTEL_BSP_SCHEDULE_DELAY"] = "50"
			h := boot(t, opts{on: true, env: env})
			h.emit("ac15-a", "ac15-b")
			if err := h.flush(); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			waitFor(t, "both spans", func() bool { return hasSpan(col, "ac15-a") && hasSpan(col, "ac15-b") })
			reqs := col.Requests()
			if len(reqs) == 0 {
				t.Fatal("collector saw no request")
			}
			wantProto := otlpcollector.ProtocolHTTP
			if c.wantGRPC {
				wantProto = otlpcollector.ProtocolGRPC
			}
			for _, r := range reqs {
				if r.Protocol != wantProto {
					t.Errorf("request protocol = %q, want %q", r.Protocol, wantProto)
				}
				wantEnc := ""
				if c.wantGzip {
					wantEnc = "gzip"
				}
				if r.ContentEncoding != wantEnc {
					t.Errorf("content encoding = %q, want %q", r.ContentEncoding, wantEnc)
				}
			}
			if n := len(decoy.Requests()); n != 0 {
				t.Errorf("the overridden endpoint received %d requests, want 0", n)
			}
			h.finish()
		})
	}
}

// AC-15 (FR-20): without a forced flush, spans still arrive after the short
// OTEL_BSP_SCHEDULE_DELAY.
func TestAC15_ScheduleDelayExportsWithoutFlush(t *testing.T) {
	col := otlpcollector.Start(t)
	h := boot(t, opts{on: true, env: map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col),
		"OTEL_BSP_SCHEDULE_DELAY":     "50",
	}})
	h.emit("scheduled")
	waitFor(t, "scheduled export", func() bool { return hasSpan(col, "scheduled") })
	h.finish()
}

// AC-24 (FR-20): configured OTLP headers are received by the collector, over
// both protocols, from both the generic and the traces-specific variable.
func TestAC24_HeadersAreSent(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, varName := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
			t.Run(proto+"/"+varName, func(t *testing.T) {
				col := otlpcollector.Start(t)
				val := "v-" + newMarker(t)
				env := map[string]string{
					"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
					varName:                       "x-corral-synthetic=" + val + ",x-corral-pct=a%20b",
				}
				if proto == "grpc" {
					env["OTEL_EXPORTER_OTLP_ENDPOINT"] = grpcURL(col)
				} else {
					env["OTEL_EXPORTER_OTLP_ENDPOINT"] = httpURL(col)
				}
				h := boot(t, opts{on: true, env: env})
				h.emit("ac24-headers")
				if err := h.flush(); err != nil {
					t.Fatalf("ForceFlush: %v", err)
				}
				waitFor(t, "request", func() bool { return len(col.Requests()) > 0 })
				for _, r := range col.Requests() {
					if got := headerValues(r, "x-corral-synthetic"); len(got) != 1 || got[0] != val {
						t.Errorf("configured header not received intact (got %d values)", len(got))
					}
					if got := headerValues(r, "x-corral-pct"); len(got) != 1 || got[0] != "a b" {
						t.Errorf("percent-decoded header value not received (got %d values)", len(got))
					}
				}
				h.finish()
			})
		}
	}
}

// AC-24 (FR-20, FR-30): exported resource attributes include the operator's
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME values.
func TestAC24_ResourceAttributesFromEnvironment(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			col := otlpcollector.Start(t)
			ep := httpURL(col)
			if proto == "grpc" {
				ep = grpcURL(col)
			}
			h := boot(t, opts{on: true, env: map[string]string{
				"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
				"OTEL_EXPORTER_OTLP_ENDPOINT": ep,
				"OTEL_SERVICE_NAME":           "synthetic-service",
				"OTEL_RESOURCE_ATTRIBUTES":    "service.version=9.9.9,deployment.environment=synthetic-env",
			}})
			h.emit("ac24-resource")
			if err := h.flush(); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			waitFor(t, "resources", func() bool { return len(col.Resources()) > 0 })
			for _, res := range col.Resources() {
				for k, want := range map[string]string{
					"service.name":           "synthetic-service",
					"service.version":        "9.9.9",
					"deployment.environment": "synthetic-env",
				} {
					if res[k] != want {
						t.Errorf("resource %s = %q, want %q", k, res[k], want)
					}
				}
			}
			h.finish()
		})
	}
}

// AC-24 (FR-30): without OTEL_SERVICE_NAME, service.name is "corral".
func TestAC24_DefaultServiceNameIsCorral(t *testing.T) {
	col := otlpcollector.Start(t)
	h := boot(t, opts{on: true, env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col)}})
	h.emit("ac24-default-name")
	if err := h.flush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	waitFor(t, "resources", func() bool { return len(col.Resources()) > 0 })
	for _, res := range col.Resources() {
		if res["service.name"] != "corral" {
			t.Errorf("service.name = %q, want corral", res["service.name"])
		}
	}
	h.finish()
}

var bannedProcessKeys = []string{"process.command_args", "process.command_line", "process.owner"}

func allowedResourceKey(k string, operator map[string]bool) bool {
	switch {
	case strings.HasPrefix(k, "service."), strings.HasPrefix(k, "telemetry.sdk."), k == "host.name":
		return true
	}
	return operator[k]
}

// AC-27 (FR-30), through the real bootstrap: banned process.* keys supplied
// in the real OTEL_RESOURCE_ATTRIBUTES never reach the collector, so
// FilterResource is wired into the OTLP path (ADR-0007 a). Every exported
// resource key is within the allowlist.
func TestAC27_BannedResourceKeysNeverReachCollector(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			col := otlpcollector.Start(t)
			ep := httpURL(col)
			if proto == "grpc" {
				ep = grpcURL(col)
			}
			bannedVals := make([]string, len(bannedProcessKeys))
			attrs := []string{"service.version=1.2.3"}
			for i, k := range bannedProcessKeys {
				bannedVals[i] = "bv-" + newMarker(t)
				attrs = append(attrs, k+"="+bannedVals[i])
			}
			h := boot(t, opts{on: true, env: map[string]string{
				"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
				"OTEL_EXPORTER_OTLP_ENDPOINT": ep,
				"OTEL_RESOURCE_ATTRIBUTES":    strings.Join(attrs, ","),
			}})
			h.emit("ac27-span")
			if err := h.flush(); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			waitFor(t, "resources", func() bool { return len(col.Resources()) > 0 })
			operator := map[string]bool{"service.version": true}
			for _, res := range col.Resources() {
				if res["service.version"] != "1.2.3" {
					t.Errorf("operator attribute service.version = %q, want 1.2.3", res["service.version"])
				}
				for k, v := range res {
					if !allowedResourceKey(k, operator) {
						t.Errorf("exported resource key %q is outside the FR-30 allowlist", k)
					}
					for _, bv := range bannedVals {
						if strings.Contains(v, bv) || strings.Contains(k, bv) {
							t.Errorf("banned attribute value leaked under key %q", k)
						}
					}
				}
				for _, k := range bannedProcessKeys {
					if _, ok := res[k]; ok {
						t.Errorf("banned resource key %s was exported", k)
					}
				}
			}
			h.finish()
		})
	}
}

// AC-27 / ADR-0007 (f): the test-only exporter override is also behind
// FilterResource, so spans delivered to it carry no banned resource key.
func TestAC27_ExporterOverrideDoesNotBypassFilterResource(t *testing.T) {
	rec := tracetest.NewInMemoryExporter()
	h := boot(t, opts{on: true, exporter: rec, env: map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "service.version=1.2.3,process.command_args=a,process.command_line=b,process.owner=c",
	}})
	h.emit("ac27-override")
	if err := h.flush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	spans := rec.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("override exporter got %d spans, want 1", len(spans))
	}
	seenVersion := false
	for _, kv := range spans[0].Resource.Attributes() {
		k := string(kv.Key)
		for _, b := range bannedProcessKeys {
			if k == b {
				t.Errorf("banned resource key %s reached the override exporter", b)
			}
		}
		if k == "service.version" {
			seenVersion = true
		}
	}
	if !seenVersion {
		t.Error("operator attribute service.version missing from the override exporter's resource")
	}
	h.finish()
}
