package logging_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// SPEC-003 FR-29 / T12: the bootstrap logger filters nothing by level, so
// every configuration record (FR-8 warn, FR-11 errors) reaches stdout.
func TestBootstrapLoggerFiltersNothing(t *testing.T) {
	var buf bytes.Buffer
	l := logging.BootstrapLogger(&buf)
	l.Debug("d-record")
	l.Info("i-record")
	l.Warn("w-record")
	l.Error("e-record")
	out := buf.String()
	for _, m := range []string{"d-record", "i-record", "w-record", "e-record"} {
		if !strings.Contains(out, m) {
			t.Errorf("record %q was filtered by the bootstrap logger", m)
		}
	}
	if n := strings.Count(out, `"component":"config"`); n != 4 {
		t.Errorf("component=config appears %d times, want 4", n)
	}
}
