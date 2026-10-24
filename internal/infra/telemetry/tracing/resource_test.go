package tracing_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

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
