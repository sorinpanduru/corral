package logging_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// FR-39: the key allowlist (fixed keys, FR-23 keys, numeric suffix rule).
func TestAllowedKeyAccepts(t *testing.T) {
	keys := []string{
		"time", "level", "msg", "component", "error", "trace_id", "span_id",
		"stack", "setting", "corral.error.detail", "suppressed_count",
		"db.system.name", "db.operation.name", "db.namespace", "db.collection.name",
		"client.address", "client.port", "error.type", "db.response.status_code",
		"corral.db.response.code_name", "server.address", "server.port",
		"corral.connection.id", "corral.upstream.connection.id",
		"corral.request.id", "corral.session.id", "corral.cursor.id",
		"corral.client.app_name", "corral.client.driver.name",
		"corral.client.driver.version",
		// numeric-suffix rule
		"retry_count", "wait_seconds", "buffer_bytes", "queue_depth_count",
	}
	for _, k := range keys {
		if !logging.AllowedKey(k) {
			t.Errorf("AllowedKey(%q) = false, want true", k)
		}
	}
}

// FR-39: keys outside the allowlist are rejected. error_class and
// error_detail are metric label names only (amendment, D4).
func TestAllowedKeyRejects(t *testing.T) {
	keys := []string{
		"", "foo", "password", "user", "username", "Component", "TIME",
		"error_class", "error_detail", "db.query.text", "db.query.summary",
		"client_address", "request", "duration_ms", "retry_count_total",
		"Retry_Count", "retryCount", "wait-seconds", "waitSeconds",
		"seconds", "count", "bytes", "1_count",
	}
	for _, k := range keys {
		if logging.AllowedKey(k) {
			t.Errorf("AllowedKey(%q) = true, want false", k)
		}
	}
}

// FR-39: numeric keys are snake_case names ending in _seconds, _bytes, _count.
func TestNumericKey(t *testing.T) {
	yes := []string{"timeout_seconds", "buffer_bytes", "retry_count", "suppressed_count", "a_b_count"}
	no := []string{
		"", "time", "msg", "component", "seconds", "count",
		"Timeout_seconds", "timeoutSeconds", "timeout-seconds", "timeout_ms",
		"retry_count_total", "retry count", "retry_COUNT", "corral.error.detail", "1_count",
	}
	for _, k := range yes {
		if !logging.NumericKey(k) {
			t.Errorf("NumericKey(%q) = false, want true", k)
		}
	}
	for _, k := range no {
		if logging.NumericKey(k) {
			t.Errorf("NumericKey(%q) = true, want false", k)
		}
	}
}
