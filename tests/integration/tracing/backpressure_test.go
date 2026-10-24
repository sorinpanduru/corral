package tracing_test

// T16 (plans/SPEC-001.md): FR-37 and AC-36 (integration) through the real
// bootstrap. Expected API: none new. Uses Options.Exporter (T15) and the
// metrics registry Gather. Synthetic data only. Not parallel.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// countingBlocker blocks every export until released, then counts the spans of
// each export it completes.
type countingBlocker struct {
	release  chan struct{}
	received atomic.Int64
	entered  atomic.Bool
}

func newCountingBlocker() *countingBlocker {
	return &countingBlocker{release: make(chan struct{})}
}

func (c *countingBlocker) Release() {
	select {
	case <-c.release:
	default:
		close(c.release)
	}
}

func (c *countingBlocker) ExportSpans(ctx context.Context, s []sdktrace.ReadOnlySpan) error {
	c.entered.Store(true)
	select {
	case <-c.release:
		c.received.Add(int64(len(s)))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *countingBlocker) Shutdown(context.Context) error { return nil }

// AC-36 / FR-37: more spans than the queue holds; ending them never blocks; after
// release and shutdown, dropped > 0 and dropped == ended - received.
func TestAC36_DropsEqualEndedMinusReceived(t *testing.T) {
	exp := newCountingBlocker()
	h := boot(t, opts{on: true, exporter: exp, env: map[string]string{
		"OTEL_BSP_MAX_QUEUE_SIZE":        "4",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "2",
		"OTEL_BSP_SCHEDULE_DELAY":        "20",
		"OTEL_BSP_EXPORT_TIMEOUT":        "30000",
	}})
	t.Cleanup(exp.Release)

	const total = 200
	tr := h.tr.TracerProvider().Tracer("corral-test")
	var worst time.Duration
	start := time.Now()
	for i := 0; i < total; i++ {
		t0 := time.Now()
		_, sp := tr.Start(context.Background(), "bp")
		sp.End()
		if d := time.Since(t0); d > worst {
			worst = d
		}
		if i == 0 {
			// let the export goroutine pick up a batch and block in the exporter
			time.Sleep(100 * time.Millisecond)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("AC-36: starting and ending %d spans took %v with a blocked exporter, want < 3s", total, d)
	}
	if worst > time.Second {
		t.Errorf("AC-36: a single span Start/End call blocked for %v", worst)
	}

	exp.Release()
	if err := h.shutdown(); err != nil {
		t.Errorf("Shutdown after release returned %v", err)
	}

	dropped, ok := h.droppedValue()
	if !ok {
		t.Fatalf("%s is not registered", droppedMetric)
	}
	received := float64(exp.received.Load())
	if dropped <= 0 {
		t.Errorf("AC-36: %s = %v, want > 0", droppedMetric, dropped)
	}
	if received <= 0 {
		t.Errorf("exporter received %v spans after release, want > 0", received)
	}
	if dropped != float64(total)-received {
		t.Errorf("AC-36: %s = %v, want ended (%d) - received (%v) = %v", droppedMetric, dropped, total, received, float64(total)-received)
	}
	h.finish()
}

// AC-36 / FR-37: the same through an explicit ForceFlush instead of Shutdown;
// the counter is stable afterwards and equals ended - received.
func TestAC36_ForceFlushAfterReleaseAccountsForEverySpan(t *testing.T) {
	exp := newCountingBlocker()
	h := boot(t, opts{on: true, exporter: exp, env: map[string]string{
		"OTEL_BSP_MAX_QUEUE_SIZE":        "3",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "3",
		"OTEL_BSP_SCHEDULE_DELAY":        "20",
		"OTEL_BSP_EXPORT_TIMEOUT":        "30000",
	}})
	t.Cleanup(exp.Release)

	const total = 60
	h.emit("first")
	time.Sleep(100 * time.Millisecond)
	names := make([]string, total-1)
	for i := range names {
		names[i] = "bp"
	}
	start := time.Now()
	h.emit(names...)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("AC-36: ending %d spans took %v with a blocked exporter", total-1, d)
	}
	exp.Release()
	if err := h.flush(); err != nil {
		t.Errorf("ForceFlush after release returned %v", err)
	}
	dropped, _ := h.droppedValue()
	received := float64(exp.received.Load())
	if dropped <= 0 || dropped != float64(total)-received {
		t.Errorf("AC-36: dropped = %v, received = %v, ended = %d; want dropped > 0 and dropped == ended - received", dropped, received, total)
	}
	h.finish()
}
