package runtime

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// White-box (package runtime): a runtime goroutine that defers recoverPanic and
// panics is logged via errclass.FromPanic: error class internal, a constant
// message, a component, and the panic value never appears (FR-24, FR-29).
func TestRecoverPanicLogsInternalWithoutValue(t *testing.T) {
	const mk = "panicmark-4f9a2c71e0b3d856"
	var buf bytes.Buffer
	l, err := logging.NewFactory(logging.NewHandler(&buf, logging.FormatJSON, slog.LevelDebug)).Logger(logging.Lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer recoverPanic(l)
		panic(mk)
	}()
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("records = %d, want 1", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["error"] != "internal" {
		t.Errorf("error class = %v, want internal", rec["error"])
	}
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if c, _ := rec["component"].(string); !logging.ValidComponent(logging.Component(c)) {
		t.Errorf("component = %v, want one from the fixed set", rec["component"])
	}
	if m, _ := rec["msg"].(string); m == "" {
		t.Errorf("empty message")
	}
	if strings.Contains(buf.String(), mk) {
		t.Errorf("panic value appears in the log output")
	}
}
