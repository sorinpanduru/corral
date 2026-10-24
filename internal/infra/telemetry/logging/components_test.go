package logging_test

import (
	"sort"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// FR-7 (as amended, D5): the fixed component set is exactly these seven.
func TestComponentsFixedSet(t *testing.T) {
	want := []string{"admin_http", "config", "grpc", "lifecycle", "otel", "thirdparty", "tracing"}
	var got []string
	for _, c := range logging.Components() {
		got = append(got, string(c))
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("Components() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Components() = %v, want %v", got, want)
		}
	}
}

// FR-7: constant values.
func TestComponentConstantValues(t *testing.T) {
	cases := map[logging.Component]string{
		logging.Config:     "config",
		logging.Lifecycle:  "lifecycle",
		logging.AdminHTTP:  "admin_http",
		logging.Tracing:    "tracing",
		logging.OTel:       "otel",
		logging.GRPC:       "grpc",
		logging.ThirdParty: "thirdparty",
	}
	for c, v := range cases {
		if string(c) != v {
			t.Errorf("component constant = %q, want %q", c, v)
		}
	}
}

// FR-7, AC-5: validation accepts the set and rejects everything else.
func TestValidComponent(t *testing.T) {
	for _, c := range logging.Components() {
		if !logging.ValidComponent(c) {
			t.Errorf("ValidComponent(%q) = false", c)
		}
	}
	for _, bad := range []string{"", "CONFIG", "Config", "unknown", "proxy", "admin-http", " config", "config ", "third_party"} {
		if logging.ValidComponent(logging.Component(bad)) {
			t.Errorf("ValidComponent(%q) = true, want false", bad)
		}
	}
}
