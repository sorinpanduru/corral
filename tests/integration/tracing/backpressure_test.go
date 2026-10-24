package tracing_test

// T16 (plans/SPEC-001.md): FR-37 and AC-36 (integration) through the real
// bootstrap. Expected API: none new. Uses Options.Exporter (T15) and the
// metrics registry Gather. Synthetic data only. Not parallel.

import (
	"context"
	"sync"
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

// runWithWatchdog runs fn in a goroutine. If it exceeds bound, the exporter is
// released and the test fails, so a blocking OnEnd fails fast instead of hanging.
func runWithWatchdog(t *testing.T, exp *countingBlocker, bound time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(bound):
		exp.Release()
		t.Errorf("AC-36: %s exceeded %v; span Start/End blocked on the exporter", what, bound)
	}
	wg.Wait()
}

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
	// First span on the test goroutine; wait until the exporter is blocked
	// (waitFor may call t.Fatalf, so it must not run in another goroutine).
	_, first := tr.Start(context.Background(), "bp")
	first.End()
	waitFor(t, "exporter to be entered and blocked", exp.entered.Load)
	start := time.Now()
	runWithWatchdog(t, exp, 5*time.Second, "ending spans", func() {
		for i := 1; i < total; i++ {
			t0 := time.Now()
			_, sp := tr.Start(context.Background(), "bp")
			sp.End()
			if d := time.Since(t0); d > worst {
				worst = d
			}
		}
	})
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
	waitFor(t, "exporter to be entered and blocked", exp.entered.Load)
	names := make([]string, total-1)
	for i := range names {
		names[i] = "bp"
	}
	start := time.Now()
	runWithWatchdog(t, exp, 5*time.Second, "ending spans", func() { h.emit(names...) })
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
	// the counter must be stable once everything has been shut down
	if again, _ := h.droppedValue(); again != dropped {
		t.Errorf("%s changed from %v to %v after finish", droppedMetric, dropped, again)
	}
}
