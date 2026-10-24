package spanqueue_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing/spanqueue"
)

// Expected API (from plans/SPEC-001.md T12):
//
//	type DropCounter interface{ Add(float64) }
//	type Config struct{ MaxQueueSize, MaxExportBatchSize int; ScheduleDelay, ExportTimeout time.Duration }
//	func DefaultConfig() Config
//	func New(exp trace.SpanExporter, cfg Config, dropped DropCounter, opts ...Option) *Processor
//	*Processor implements trace.SpanProcessor.
//	Export errors go to otel.Handle (global error handler), never logged.
//
// The fakeclock has Now/Advance only (no timers), so it cannot drive a worker
// timer. Timing tests therefore use real, generous bounds: "must not happen
// yet" checks use a very long ScheduleDelay, "must happen" checks poll with
// a multi-second deadline.

// The T7 counter must satisfy the processor's own minimal interface.
var _ spanqueue.DropCounter = (*metrics.Counter)(nil)

var _ trace.SpanProcessor = (*spanqueue.Processor)(nil)

const (
	longDelay = time.Hour
	waitMax   = 5 * time.Second
)

// ---- fakes ----

type fakeCounter struct {
	mu    sync.Mutex
	calls []float64
}

func (c *fakeCounter) Add(v float64) {
	c.mu.Lock()
	c.calls = append(c.calls, v)
	c.mu.Unlock()
}

func (c *fakeCounter) Total() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var s float64
	for _, v := range c.calls {
		s += v
	}
	return s
}

func (c *fakeCounter) Calls() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float64(nil), c.calls...)
}

type fakeExporter struct {
	mu            sync.Mutex
	batches       [][]string
	deadlines     []time.Duration // remaining time at call start; -1 if none
	shutdownCalls int
	err           error
	gate          chan struct{} // if non-nil, ExportSpans blocks until closed or ctx done
	entered       chan struct{} // signalled (non-blocking) on each ExportSpans entry
}

func newExporter() *fakeExporter {
	return &fakeExporter{entered: make(chan struct{}, 1024)}
}

func newBlockedExporter() *fakeExporter {
	e := newExporter()
	e.gate = make(chan struct{})
	return e
}

func (e *fakeExporter) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gate != nil {
		select {
		case <-e.gate:
		default:
			close(e.gate)
		}
	}
}

func (e *fakeExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	remaining := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		remaining = time.Until(dl)
	}
	select {
	case e.entered <- struct{}{}:
	default:
	}
	e.mu.Lock()
	gate, err := e.gate, e.err
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	e.mu.Lock()
	e.batches = append(e.batches, names)
	e.deadlines = append(e.deadlines, remaining)
	e.mu.Unlock()
	return err
}

func (e *fakeExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.shutdownCalls++
	e.mu.Unlock()
	return nil
}

func (e *fakeExporter) Received() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, b := range e.batches {
		n += len(b)
	}
	return n
}

func (e *fakeExporter) Batches() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.batches...)
}

func (e *fakeExporter) Deadlines() []time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Duration(nil), e.deadlines...)
}

func (e *fakeExporter) ShutdownCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCalls
}

// ---- helpers ----

func span(name string, sampled bool) trace.ReadOnlySpan {
	var flags oteltrace.TraceFlags
	if sampled {
		flags = oteltrace.FlagsSampled
	}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     oteltrace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: flags,
	})
	return tracetest.SpanStub{Name: name, SpanContext: sc}.Snapshot()
}

func sampled(name string) trace.ReadOnlySpan { return span(name, true) }

// captureErrors installs a global otel error handler for the test. Tests using
// it must not run in parallel.
func captureErrors(t *testing.T) func() []error {
	t.Helper()
	var mu sync.Mutex
	var errs []error
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}))
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })
	return func() []error {
		mu.Lock()
		defer mu.Unlock()
		return append([]error(nil), errs...)
	}
}

func cfg(queue, batch int, delay time.Duration) spanqueue.Config {
	return spanqueue.Config{
		MaxQueueSize:       queue,
		MaxExportBatchSize: batch,
		ScheduleDelay:      delay,
		ExportTimeout:      30 * time.Second,
	}
}

// start builds a processor and guarantees cleanup (exporter released first so
// Shutdown cannot hang).
func start(t *testing.T, exp *fakeExporter, c spanqueue.Config, dc spanqueue.DropCounter) *spanqueue.Processor {
	t.Helper()
	p := spanqueue.New(exp, c, dc)
	t.Cleanup(func() {
		exp.release()
		ctx, cancel := context.WithTimeout(context.Background(), waitMax)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitMax)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func flushCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitMax)
	t.Cleanup(cancel)
	return ctx
}

// runBounded fails if f does not return within d.
func runBounded(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked longer than %v", what, d)
	}
}

// ---- tests ----

// FR-37: defaults are the OpenTelemetry specification defaults.
func TestDefaultConfig_FR37(t *testing.T) {
	got := spanqueue.DefaultConfig()
	if got.MaxQueueSize != 2048 || got.MaxExportBatchSize != 512 ||
		got.ScheduleDelay != 5*time.Second || got.ExportTimeout != 30*time.Second {
		t.Fatalf("DefaultConfig = %+v", got)
	}
}

// FR-37: OnStart is a no-op and never blocks.
func TestOnStartNoop_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, cfg(4, 2, longDelay), &fakeCounter{})
	runBounded(t, time.Second, "OnStart", func() { p.OnStart(context.Background(), nil) })
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 0 {
		t.Fatalf("OnStart exported something: %d", exp.Received())
	}
}

// FR-37: below capacity nothing is dropped and the counter is untouched.
func TestNoDropBelowCapacity_FR37(t *testing.T) {
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := start(t, exp, cfg(8, 8, longDelay), dc)
	for i := 0; i < 7; i++ { // fewer than batch size: no export is triggered
		p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
	}
	if len(dc.Calls()) != 0 {
		t.Fatalf("counter touched below capacity: %v", dc.Calls())
	}
	exp.release()
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 7 || dc.Total() != 0 {
		t.Fatalf("received=%d drops=%v", exp.Received(), dc.Total())
	}
}

// FR-37, AC-36 (unit): with a stalled exporter OnEnd never blocks, every
// queue-full drop adds exactly 1, and the drop count is bounded by what the
// queue (plus one in-flight batch) can hold.
func TestQueueFullDropsNonBlocking_FR37_AC36(t *testing.T) {
	const queue, batch, total = 4, 2, 200
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := start(t, exp, cfg(queue, batch, longDelay), dc)

	runBounded(t, 2*time.Second, "OnEnd with stalled exporter", func() {
		for i := 0; i < total; i++ {
			p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
		}
	})

	for _, v := range dc.Calls() {
		if v != 1 {
			t.Fatalf("each drop must Add(1), got Add(%v)", v)
		}
	}
	drops := int(dc.Total())
	if drops < total-queue-batch || drops > total-queue {
		t.Fatalf("drops=%d, want within [%d,%d]", drops, total-queue-batch, total-queue)
	}
}

// FR-37, AC-36 (unit form): counter == spans ended - spans received by the
// exporter, after releasing the exporter and flushing; counter > 0.
func TestAC36_CounterEqualsEndedMinusReceived(t *testing.T) {
	const total = 100
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := start(t, exp, cfg(8, 4, longDelay), dc)

	runBounded(t, 2*time.Second, "OnEnd", func() {
		for i := 0; i < total; i++ {
			p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
		}
	})
	exp.release()
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	drops := int(dc.Total())
	if drops <= 0 {
		t.Fatalf("expected drops > 0, got %d", drops)
	}
	if got := total - exp.Received(); drops != got {
		t.Fatalf("counter=%d, ended-received=%d", drops, got)
	}
}

// FR-37: exports contain at most MaxExportBatchSize spans, and an export
// starts as soon as that many are queued (no schedule tick needed).
func TestBatchSizeTriggersExport_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, cfg(10, 3, longDelay), &fakeCounter{})
	for i := 0; i < 7; i++ {
		p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
	}
	eventually(t, "two full batches exported without flush or tick", func() bool { return exp.Received() >= 6 })

	time.Sleep(100 * time.Millisecond)
	if got := exp.Received(); got != 6 {
		t.Fatalf("partial batch exported early: received=%d, want 6", got)
	}
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 7 {
		t.Fatalf("received=%d after flush, want 7", exp.Received())
	}
	for _, b := range exp.Batches() {
		if len(b) > 3 || len(b) == 0 {
			t.Fatalf("batch size %d outside (0,3]", len(b))
		}
	}
}

// FR-37: spans queued but below batch size are exported once ScheduleDelay
// elapses (real time; bounds are generous).
func TestScheduleDelayExports_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, cfg(10, 10, 600*time.Millisecond), &fakeCounter{})
	p.OnEnd(sampled("only"))
	if exp.Received() != 0 {
		t.Fatal("exported before the schedule delay")
	}
	eventually(t, "export after schedule delay", func() bool { return exp.Received() == 1 })
}

// FR-37: each export call is bounded by ExportTimeout.
func TestExportTimeoutBoundsCall_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, spanqueue.Config{
		MaxQueueSize: 10, MaxExportBatchSize: 10, ScheduleDelay: longDelay,
		ExportTimeout: 7 * time.Second,
	}, &fakeCounter{})
	p.OnEnd(sampled("a"))
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	ds := exp.Deadlines()
	if len(ds) != 1 {
		t.Fatalf("exports=%d", len(ds))
	}
	if ds[0] < 0 || ds[0] > 7*time.Second {
		t.Fatalf("export ctx remaining=%v, want a deadline <= ExportTimeout", ds[0])
	}
}

// FR-37: a stalled export is cancelled after ExportTimeout, the error is
// reported through the otel error handler, and the worker keeps going.
func TestExportTimeoutCancelsStalledExport_FR37(t *testing.T) {
	errs := captureErrors(t)
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := start(t, exp, spanqueue.Config{
		MaxQueueSize: 10, MaxExportBatchSize: 1, ScheduleDelay: longDelay,
		ExportTimeout: 100 * time.Millisecond,
	}, dc)
	p.OnEnd(sampled("stuck"))
	eventually(t, "timeout error reported", func() bool {
		for _, e := range errs() {
			if errors.Is(e, context.DeadlineExceeded) {
				return true
			}
		}
		return false
	})
	if dc.Total() != 0 {
		t.Fatalf("export timeout must not touch the drop counter: %v", dc.Total())
	}
	// Worker is not wedged: next span is attempted.
	exp.release()
	p.OnEnd(sampled("next"))
	eventually(t, "span exported after a timed-out export", func() bool { return exp.Received() == 1 })
}

// FR-37, FR-34: ForceFlush exports everything queued.
func TestForceFlushExportsAll_FR34_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, cfg(100, 5, longDelay), &fakeCounter{})
	for i := 0; i < 12; i++ {
		p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
	}
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 12 {
		t.Fatalf("received=%d, want 12", exp.Received())
	}
	for _, b := range exp.Batches() {
		if len(b) > 5 {
			t.Fatalf("batch of %d exceeds MaxExportBatchSize", len(b))
		}
	}
}

func TestForceFlushEmpty_FR37(t *testing.T) {
	exp := newExporter()
	p := start(t, exp, cfg(10, 5, longDelay), &fakeCounter{})
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
}

// FR-34: ForceFlush honours the caller's deadline when the exporter is stalled.
func TestForceFlushHonoursContext_FR34(t *testing.T) {
	exp := newBlockedExporter()
	p := start(t, exp, cfg(10, 10, longDelay), &fakeCounter{})
	p.OnEnd(sampled("a"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var err error
	runBounded(t, 3*time.Second, "ForceFlush", func() { err = p.ForceFlush(ctx) })
	if err == nil {
		t.Fatal("ForceFlush with stalled exporter and expired context must return an error")
	}
}

// FR-34: Shutdown flushes queued spans, shuts the exporter down, then refuses
// new spans (silently: not exported, not counted as queue-full drops).
func TestShutdownFlushesThenRefuses_FR34(t *testing.T) {
	exp := newExporter()
	dc := &fakeCounter{}
	p := spanqueue.New(exp, cfg(100, 50, longDelay), dc)
	for i := 0; i < 7; i++ {
		p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
	}
	if err := p.Shutdown(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 7 {
		t.Fatalf("received=%d, want 7 flushed on shutdown", exp.Received())
	}
	if exp.ShutdownCalls() != 1 {
		t.Fatalf("exporter Shutdown calls=%d, want 1", exp.ShutdownCalls())
	}
	runBounded(t, time.Second, "OnEnd after Shutdown", func() { p.OnEnd(sampled("late")) })
	_ = p.ForceFlush(flushCtx(t)) // result unspecified; must not hang or panic
	if exp.Received() != 7 {
		t.Fatalf("span accepted after Shutdown: received=%d", exp.Received())
	}
	if len(dc.Calls()) != 0 {
		t.Fatalf("post-shutdown spans must not count as queue-full drops: %v", dc.Calls())
	}
	// Second Shutdown must not panic or hang.
	runBounded(t, 2*time.Second, "second Shutdown", func() { _ = p.Shutdown(flushCtx(t)) })
}

// FR-34: Shutdown returns within the caller's deadline with a stalled
// exporter, drops the rest, and does not count those as queue-full drops.
func TestShutdownDeadlineWithStalledExporter_FR34_FR37(t *testing.T) {
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := spanqueue.New(exp, cfg(10, 10, longDelay), dc)
	for i := 0; i < 5; i++ {
		p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	runBounded(t, 3*time.Second, "Shutdown", func() { _ = p.Shutdown(ctx) })
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Shutdown took %v with a 200ms deadline", el)
	}
	if len(dc.Calls()) != 0 {
		t.Fatalf("shutdown-deadline losses must not touch the counter: %v", dc.Calls())
	}
	exp.release()
}

// FR-37: unsampled spans are ignored (not exported, not counted, no space used).
func TestUnsampledIgnored_FR37(t *testing.T) {
	exp := newBlockedExporter()
	dc := &fakeCounter{}
	p := start(t, exp, cfg(2, 2, longDelay), dc)
	for i := 0; i < 50; i++ {
		p.OnEnd(span(fmt.Sprintf("u%d", i), false))
	}
	if len(dc.Calls()) != 0 {
		t.Fatalf("unsampled spans counted as drops: %v", dc.Calls())
	}
	p.OnEnd(sampled("kept1"))
	exp.release()
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if exp.Received() != 1 {
		t.Fatalf("received=%d, want only the sampled span", exp.Received())
	}
	if b := exp.Batches(); len(b) != 1 || b[0][0] != "kept1" {
		t.Fatalf("batches=%v", b)
	}
}

// FR-37: exporter errors do not block, reach otel.Handle, and are not counted
// as queue-full drops.
func TestExporterErrorsReported_FR37(t *testing.T) {
	errs := captureErrors(t)
	boom := errors.New("synthetic export failure")
	exp := newExporter()
	exp.err = boom
	dc := &fakeCounter{}
	p := start(t, exp, cfg(10, 2, longDelay), dc)

	runBounded(t, time.Second, "OnEnd", func() {
		for i := 0; i < 6; i++ {
			p.OnEnd(sampled(fmt.Sprintf("s%d", i)))
		}
	})
	eventually(t, "all spans attempted", func() bool { return exp.Received() == 6 })
	found := false
	for _, e := range errs() {
		if errors.Is(e, boom) {
			found = true
		}
	}
	if !found {
		t.Fatalf("export error not passed to the otel error handler: %v", errs())
	}
	if len(dc.Calls()) != 0 {
		t.Fatalf("failed exports must not touch the drop counter: %v", dc.Calls())
	}
}

// Shutdown leaves no goroutines behind.
func TestNoGoroutineLeakAfterShutdown_FR37(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	exp := newExporter()
	p := spanqueue.New(exp, cfg(16, 4, 10*time.Millisecond), &fakeCounter{})
	for i := 0; i < 40; i++ {
		p.OnEnd(sampled("s"))
	}
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= before })
}

// Also after a Shutdown that hit its deadline with a stalled exporter, once
// the exporter lets go.
func TestNoGoroutineLeakAfterDeadlineShutdown_FR34(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	exp := newBlockedExporter()
	p := spanqueue.New(exp, cfg(8, 4, longDelay), &fakeCounter{})
	for i := 0; i < 20; i++ {
		p.OnEnd(sampled("s"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = p.Shutdown(ctx)
	exp.release()
	eventually(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= before })
}

// Race safety: concurrent OnEnd (plus concurrent ForceFlush). Every ended span
// is either received by the exporter or counted as dropped.
func TestConcurrentOnEnd_Race(t *testing.T) {
	const workers, per = 8, 500
	exp := newExporter()
	dc := &fakeCounter{}
	p := start(t, exp, cfg(16, 4, time.Millisecond), dc)

	var wg sync.WaitGroup
	var ended atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				p.OnEnd(sampled(fmt.Sprintf("w%d-%d", w, i)))
				ended.Add(1)
				if i%100 == 0 {
					_ = p.ForceFlush(context.Background())
				}
			}
		}(w)
	}
	runBounded(t, 30*time.Second, "concurrent OnEnd", wg.Wait)
	if err := p.ForceFlush(flushCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got := int(dc.Total()) + exp.Received(); got != int(ended.Load()) {
		t.Fatalf("drops(%v)+received(%d)=%d, ended=%d", dc.Total(), exp.Received(), got, ended.Load())
	}
}
