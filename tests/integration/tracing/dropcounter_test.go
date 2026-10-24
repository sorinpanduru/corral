package tracing_test

import (
	"testing"
	"time"
)

// FR-37 (AC-7 precondition): corral_tracing_spans_dropped_total is registered
// through the catalog-backed factory and is zero before any drop.
func TestFR37_DroppedCounterRegisteredAtZero(t *testing.T) {
	h := boot(t, opts{on: true, exporter: newBlockingExporter()})
	v, ok := h.droppedValue()
	if !ok {
		t.Fatalf("%s is not registered", droppedMetric)
	}
	if v != 0 {
		t.Errorf("%s = %v before any span, want 0", droppedMetric, v)
	}
	h.finish()
}

// FR-37: with a tiny queue and an exporter that blocks, ending many sampled
// spans never blocks the caller, and the counter increments for the spans
// dropped because the queue was full.
func TestFR37_DroppedCounterIncrementsOnQueueFull(t *testing.T) {
	blk := newBlockingExporter()
	h := boot(t, opts{on: true, exporter: blk, env: map[string]string{
		"OTEL_BSP_MAX_QUEUE_SIZE":        "2",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "1",
		"OTEL_BSP_SCHEDULE_DELAY":        "50",
	}})
	t.Cleanup(blk.Release)

	const total = 50
	start := time.Now()
	for i := 0; i < total; i++ {
		h.emit("fills-queue")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("ending %d spans took %v with a blocked exporter; Start/End must not block", total, d)
	}
	v, ok := h.droppedValue()
	if !ok {
		t.Fatalf("%s is not registered", droppedMetric)
	}
	// At most one batch (1 span) is held by the blocked export call and 2 sit
	// in the queue, so at least total-3 spans are dropped.
	if v < total-3 || v > total {
		t.Errorf("%s = %v after %d spans into a full queue, want in [%d, %d]", droppedMetric, v, total, total-3, total)
	}
	blk.Release()
	h.finish()
}
