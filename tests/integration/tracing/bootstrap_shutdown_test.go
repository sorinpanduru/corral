package tracing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// FR-34 (basic check; T16 owns the full AC-28): with a responsive collector,
// spans ended just before Shutdown are received.
func TestFR34_ShutdownFlushesPendingSpans(t *testing.T) {
	col := otlpcollector.Start(t)
	h := boot(t, opts{on: true, env: map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col),
		"OTEL_BSP_SCHEDULE_DELAY":     "60000",
	}})
	h.emit("pending-1", "pending-2")
	if err := h.shutdown(); err != nil {
		t.Errorf("Shutdown with a responsive collector returned %v", err)
	}
	for _, n := range []string{"pending-1", "pending-2"} {
		if !hasSpan(col, n) {
			t.Errorf("span %s not received after Shutdown", n)
		}
	}
	h.finish()
}

// FR-34 (ADR-0007 d): Shutdown applies a 5 s deadline even when the caller
// passes a context without one, so an unresponsive collector cannot hold it
// for the 30 s default export timeout.
func TestFR34_ShutdownIsBoundedByFiveSeconds(t *testing.T) {
	col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	h := boot(t, opts{on: true, env: map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col),
		"OTEL_EXPORTER_OTLP_TIMEOUT":  "30000",
		"OTEL_BSP_EXPORT_TIMEOUT":     "30000",
		"OTEL_BSP_SCHEDULE_DELAY":     "60000",
	}})
	h.emit("stuck")
	h.shutDown = true
	start := time.Now()
	_ = h.tr.Shutdown(context.Background())
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("Shutdown took %v with an unresponsive collector, want within 5s plus tolerance", d)
	}
	h.finish()
}

// FR-34: a caller deadline shorter than 5 s is honoured.
func TestFR34_ShutdownHonoursShorterCallerContext(t *testing.T) {
	col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	h := boot(t, opts{on: true, env: map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col),
		"OTEL_EXPORTER_OTLP_TIMEOUT":  "30000",
		"OTEL_BSP_EXPORT_TIMEOUT":     "30000",
		"OTEL_BSP_SCHEDULE_DELAY":     "60000",
	}})
	h.emit("stuck")
	h.shutDown = true
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = h.tr.Shutdown(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Shutdown took %v with a 300ms caller deadline", d)
	}
	h.finish()
}
