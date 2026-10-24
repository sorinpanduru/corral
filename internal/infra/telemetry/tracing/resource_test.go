package tracing_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// Expected API (plan T11):
//   tracing.NewResource(ctx context.Context, env tracing.LookupEnv) (*resource.Resource, error)
// Environment is read only through env, never from the process environment.

func mapEnv(m map[string]string) tracing.LookupEnv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func marker(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "synthetic-marker-" + hex.EncodeToString(b)
}

func attrs(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	res, err := tracing.NewResource(context.Background(), mapEnv(env))
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	if res == nil {
		t.Fatal("NewResource returned nil resource")
	}
	out := map[string]string{}
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func allowedByDefault(k string) bool {
	return strings.HasPrefix(k, "service.") || strings.HasPrefix(k, "telemetry.sdk.") || k == "host.name"
}

var bannedKeys = []string{"process.command_args", "process.command_line", "process.owner"}

// FR-30, AC-24: without OTEL_SERVICE_NAME, service.name is corral.
func TestResourceDefaultServiceName(t *testing.T) {
	got := attrs(t, nil)
	if got["service.name"] != "corral" {
		t.Errorf("service.name = %q, want corral", got["service.name"])
	}
}

// FR-30, AC-24: OTEL_SERVICE_NAME overrides the default, and wins over a
// service.name supplied in OTEL_RESOURCE_ATTRIBUTES.
func TestResourceServiceNameOverride(t *testing.T) {
	got := attrs(t, map[string]string{"OTEL_SERVICE_NAME": "synthetic-svc"})
	if got["service.name"] != "synthetic-svc" {
		t.Errorf("service.name = %q, want synthetic-svc", got["service.name"])
	}
	got = attrs(t, map[string]string{
		"OTEL_SERVICE_NAME":        "from-service-name",
		"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs",
	})
	if got["service.name"] != "from-service-name" {
		t.Errorf("service.name = %q, want OTEL_SERVICE_NAME to win", got["service.name"])
	}
}

// FR-30, AC-24: service.name in OTEL_RESOURCE_ATTRIBUTES alone is honoured.
func TestResourceServiceNameFromResourceAttributes(t *testing.T) {
	got := attrs(t, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs"})
	if got["service.name"] != "from-attrs" {
		t.Errorf("service.name = %q, want from-attrs", got["service.name"])
	}
}

// FR-30, AC-27: with no operator input, every attribute is inside the
// allowlist (service.*, telemetry.sdk.*, host.name), and the SDK attributes
// and host.name are present.
func TestResourceDefaultAttributesWithinAllowlist(t *testing.T) {
	got := attrs(t, nil)
	for k := range got {
		if !allowedByDefault(k) {
			t.Errorf("attribute %q is outside the FR-30 allowlist", k)
		}
	}
	for _, k := range []string{"service.name", "telemetry.sdk.name", "telemetry.sdk.language", "telemetry.sdk.version", "host.name"} {
		if got[k] == "" {
			t.Errorf("expected attribute %q to be present and non-empty", k)
		}
	}
}

// FR-30, AC-27: detector output outside the allowlist is not emitted.
func TestResourceNoOtherDetectorOutput(t *testing.T) {
	got := attrs(t, nil)
	for _, k := range []string{
		"process.pid", "process.executable.name", "process.executable.path",
		"process.runtime.name", "process.runtime.version", "process.runtime.description",
		"process.parent_pid", "os.type", "os.description", "host.id", "host.arch",
		"container.id",
	} {
		if _, ok := got[k]; ok {
			t.Errorf("detector attribute %q must not be emitted", k)
		}
	}
	for _, k := range bannedKeys {
		if _, ok := got[k]; ok {
			t.Errorf("%q must never be emitted", k)
		}
	}
}

// FR-30, AC-27: banned process.* keys are removed even when supplied via
// OTEL_RESOURCE_ATTRIBUTES, and their marker values never appear anywhere in
// the final attributes. Operator-supplied keys survive.
func TestResourceBannedKeysRemovedWhenSupplied(t *testing.T) {
	m1, m2, m3, m4 := marker(t), marker(t), marker(t), marker(t)
	got := attrs(t, map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "process.command_args=" + m1 +
			",process.command_line=" + m2 +
			",process.owner=" + m3 +
			",deployment.environment.name=" + m4 +
			",service.version=1.2.3",
	})
	for _, k := range bannedKeys {
		if v, ok := got[k]; ok {
			t.Errorf("%q survived with value %q", k, v)
		}
	}
	for k, v := range got {
		for _, m := range []string{m1, m2, m3} {
			if strings.Contains(v, m) || strings.Contains(k, m) {
				t.Errorf("banned marker leaked in attribute %q=%q", k, v)
			}
		}
	}
	if got["deployment.environment.name"] != m4 {
		t.Errorf("operator-supplied key lost: got %q", got["deployment.environment.name"])
	}
	if got["service.version"] != "1.2.3" {
		t.Errorf("service.version = %q, want 1.2.3", got["service.version"])
	}
}

// FR-30: removal holds when the banned keys are the only supplied keys and
// when combined with OTEL_SERVICE_NAME.
func TestResourceBannedKeysRemovedWithServiceName(t *testing.T) {
	m := marker(t)
	got := attrs(t, map[string]string{
		"OTEL_SERVICE_NAME":        "synthetic-svc",
		"OTEL_RESOURCE_ATTRIBUTES": "process.owner=" + m,
	})
	if _, ok := got["process.owner"]; ok {
		t.Error("process.owner survived")
	}
	if got["service.name"] != "synthetic-svc" {
		t.Errorf("service.name = %q", got["service.name"])
	}
}

// FR-30, AC-24: operator-supplied keys from OTEL_RESOURCE_ATTRIBUTES appear
// in the resource, with their values.
func TestResourceOperatorSuppliedKeysSurvive(t *testing.T) {
	got := attrs(t, map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "team=synthetic-team,deployment.environment.name=test",
	})
	if got["team"] != "synthetic-team" || got["deployment.environment.name"] != "test" {
		t.Errorf("operator keys missing: %v", got)
	}
}

// FR-30: the resource reads environment only through the injected lookup.
// Values in the real process environment must not leak in, so detectors that
// read os.Getenv (resource.WithFromEnv) are not used.
func TestResourceIgnoresProcessEnvironment(t *testing.T) {
	m := marker(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "process.owner="+m+",leak.key="+m)
	t.Setenv("OTEL_SERVICE_NAME", "process-env-svc")
	got := attrs(t, nil)
	if got["service.name"] != "corral" {
		t.Errorf("service.name = %q, process env must be ignored", got["service.name"])
	}
	for k, v := range got {
		if strings.Contains(v, m) {
			t.Errorf("process-env marker leaked into %q", k)
		}
	}
}

// FR-30: NewResource does not merge with resource.Default(), so no
// unexpected keys arrive from OTEL_RESOURCE_ATTRIBUTES-independent defaults.
func TestResourceAttributeKeysAreOnlyAllowedOrOperatorSupplied(t *testing.T) {
	supplied := map[string]bool{"team": true}
	got := attrs(t, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "team=x"})
	for k := range got {
		if !allowedByDefault(k) && !supplied[k] {
			t.Errorf("unexpected attribute %q", k)
		}
	}
}

// FR-30: percent-encoded banned keys are dropped too (raw and decoded key
// are both compared).
func TestResourcePercentEncodedBannedKeysRemoved(t *testing.T) {
	m1, m2 := marker(t), marker(t)
	got := attrs(t, map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "process%2Eowner=" + m1 + ",process%2Ecommand_line=" + m2 + ",team=x",
	})
	for k, v := range got {
		if strings.Contains(v, m1) || strings.Contains(v, m2) {
			t.Errorf("banned marker leaked in %q=%q", k, v)
		}
		if k == "process.owner" || k == "process.command_line" || k == "process.command_args" ||
			strings.HasPrefix(k, "process%2E") {
			t.Errorf("banned key %q survived", k)
		}
	}
	if got["team"] != "x" {
		t.Errorf("team = %q, want x", got["team"])
	}
}

// FR-30: percent-decoded values and whitespace trimming in
// OTEL_RESOURCE_ATTRIBUTES; whitespace-only OTEL_SERVICE_NAME falls back to
// the default.
func TestResourceValueDecodingAndTrimming(t *testing.T) {
	got := attrs(t, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "k=a%20b"})
	if got["k"] != "a b" {
		t.Errorf("k = %q, want %q", got["k"], "a b")
	}
	got = attrs(t, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": " team = x , other=y "})
	if got["team"] != "x" || got["other"] != "y" {
		t.Errorf("whitespace not trimmed: team=%q other=%q", got["team"], got["other"])
	}
	got = attrs(t, map[string]string{"OTEL_SERVICE_NAME": "   "})
	if got["service.name"] != "corral" {
		t.Errorf("whitespace-only OTEL_SERVICE_NAME: service.name = %q, want corral", got["service.name"])
	}
}

// Expected API (orchestrator decision):
//   tracing.FilterResource(exp sdktrace.SpanExporter, res *resource.Resource) sdktrace.SpanExporter
// exports each span with Resource() replaced by res.

func exportedResourceAttrs(t *testing.T, mkProvider func(exp *tracetest.InMemoryExporter) *sdktrace.TracerProvider) map[string]string {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := mkProvider(mem)
	_, sp := tp.Tracer("synthetic").Start(context.Background(), "op")
	sp.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	out := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// FR-30, AC-27 (end to end, unit level): even with banned keys in the REAL
// process environment (which sdktrace.WithResource merges via
// resource.Environment()), FilterResource keeps them out of exported spans.
func TestFilterResourceEndToEndNoBannedKeys(t *testing.T) {
	m1, m2, m3 := marker(t), marker(t), marker(t)
	raw := "process.owner=" + m1 + ",process.command_line=" + m2 + ",process.command_args=" + m3 + ",team=x"
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", raw)
	res, err := tracing.NewResource(context.Background(), mapEnv(map[string]string{"OTEL_RESOURCE_ATTRIBUTES": raw}))
	if err != nil {
		t.Fatal(err)
	}

	// Control: without FilterResource the banned keys DO appear, so the
	// assertions below can fail.
	control := exportedResourceAttrs(t, func(mem *tracetest.InMemoryExporter) *sdktrace.TracerProvider {
		return sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSyncer(mem))
	})
	if control["process.owner"] != m1 {
		t.Fatalf("control: expected process.owner to leak without FilterResource, got %v", control)
	}

	got := exportedResourceAttrs(t, func(mem *tracetest.InMemoryExporter) *sdktrace.TracerProvider {
		return sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSyncer(tracing.FilterResource(mem, res)))
	})
	for k, v := range got {
		for _, m := range []string{m1, m2, m3} {
			if strings.Contains(v, m) {
				t.Errorf("marker leaked in exported %q=%q", k, v)
			}
		}
		if k == "process.owner" || k == "process.command_line" || k == "process.command_args" {
			t.Errorf("banned key %q exported", k)
		}
		if !allowedByDefault(k) && k != "team" {
			t.Errorf("exported attribute %q outside the allowlist", k)
		}
	}
	if got["service.name"] != "corral" || got["team"] != "x" {
		t.Errorf("service.name=%q team=%q, want corral and x", got["service.name"], got["team"])
	}
}

type spyExporter struct {
	exported, shutdowns, flushes int
	shutdownErr, flushErr        error
	resources                    []*resource.Resource
}

func (s *spyExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	s.exported += len(spans)
	for _, sp := range spans {
		s.resources = append(s.resources, sp.Resource())
	}
	return nil
}
func (s *spyExporter) Shutdown(context.Context) error   { s.shutdowns++; return s.shutdownErr }
func (s *spyExporter) ForceFlush(context.Context) error { s.flushes++; return s.flushErr }

// FR-30: Shutdown and ForceFlush pass through to the wrapped exporter, with
// their errors.
func TestFilterResourcePassesThroughShutdownAndFlush(t *testing.T) {
	res, err := tracing.NewResource(context.Background(), mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	sErr, fErr := errors.New("synthetic shutdown"), errors.New("synthetic flush")
	spy := &spyExporter{shutdownErr: sErr, flushErr: fErr}
	exp := tracing.FilterResource(spy, res)

	if err := exp.Shutdown(context.Background()); !errors.Is(err, sErr) || spy.shutdowns != 1 {
		t.Errorf("Shutdown: err=%v calls=%d, want passthrough", err, spy.shutdowns)
	}
	f, ok := exp.(interface{ ForceFlush(context.Context) error })
	if !ok {
		t.Fatal("wrapper must expose ForceFlush when the wrapped exporter does")
	}
	if err := f.ForceFlush(context.Background()); !errors.Is(err, fErr) || spy.flushes != 1 {
		t.Errorf("ForceFlush: err=%v calls=%d, want passthrough", err, spy.flushes)
	}
}

// FR-30: every exported span carries exactly the given resource, and the
// span count is preserved.
func TestFilterResourceReplacesResource(t *testing.T) {
	res, err := tracing.NewResource(context.Background(), mapEnv(map[string]string{"OTEL_SERVICE_NAME": "synthetic-svc"}))
	if err != nil {
		t.Fatal(err)
	}
	spy := &spyExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracing.FilterResource(spy, res)))
	tr := tp.Tracer("synthetic")
	for i := 0; i < 3; i++ {
		_, sp := tr.Start(context.Background(), "op")
		sp.End()
	}
	if spy.exported != 3 {
		t.Fatalf("exported %d spans, want 3", spy.exported)
	}
	for _, r := range spy.resources {
		if !r.Equal(res) {
			t.Errorf("span resource %v != supplied resource %v", r.Attributes(), res.Attributes())
		}
	}
}
