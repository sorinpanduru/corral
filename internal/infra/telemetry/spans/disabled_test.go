package spans_test

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
)

// countingTracer counts Start calls.
type countingTracer struct {
	trace.Tracer
	starts atomic.Int64
}

func (c *countingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	c.starts.Add(1)
	return c.Tracer.Start(ctx, name, opts...)
}

// SPEC-001 FR-19, AC-22 (Part B): with tracing off, Tracer.Start is never
// called, and Start, Checkout, RoundTrip and every End allocate nothing.
func TestDisabledNoStartNoAllocs_FR19(t *testing.T) {
	tr := &countingTracer{Tracer: noop.NewTracerProvider().Tracer("x")}
	f := spans.New(tr, false)
	i := &spans.RootInfo{Command: "find", DB: "shop", Collection: "orders", HasCollection: true,
		Client: netip.MustParseAddrPort("192.0.2.7:51234"), ConnID: 1, RequestID: 2, HasSession: true, HasCursor: true,
		AppName: "app", DriverName: "drv", DriverVersion: "1"}
	srv := netip.MustParseAddrPort("127.0.0.1:27017")
	reply := &wire.ReplyFields{HasOK: true, OK: 0, HasCode: true, Code: 11000, CodeName: "DuplicateKey"}
	err := errPoolWait
	ctx := context.Background()

	run := func() {
		c2, ex := f.Start(ctx, i)
		co := ex.Checkout(c2)
		co.End(nil, nil)
		rt := ex.RoundTrip(c2, srv, 7)
		rt.End(nil, reply)
		ex.End(nil, reply)

		_, ex = f.Start(ctx, i)
		co = ex.Checkout(ctx)
		co.End(err, nil)
		ex.End(err, nil)
	}
	run()
	if n := tr.starts.Load(); n != 0 {
		t.Fatalf("Tracer.Start called %d times with tracing disabled", n)
	}
	if got := testing.AllocsPerRun(1000, run); got != 0 {
		t.Fatalf("allocations with tracing off = %v, want 0", got)
	}
	if n := tr.starts.Load(); n != 0 {
		t.Fatalf("Tracer.Start called %d times during the allocation run", n)
	}
}

// Disabled: the context comes back unchanged.
func TestDisabledContextUnchanged_FR19(t *testing.T) {
	f := spans.New(noop.NewTracerProvider().Tracer("x"), false)
	type k struct{}
	ctx := context.WithValue(context.Background(), k{}, 1)
	c2, ex := f.Start(ctx, &spans.RootInfo{Command: "find"})
	if c2 != ctx {
		t.Fatal("Start returned a different context with tracing off")
	}
	ex.End(nil, nil)
}

// Enabled: Start does call the tracer (the counting wrapper proves the switch
// is the only difference).
func TestEnabledCallsTracer_FR19(t *testing.T) {
	tr := &countingTracer{Tracer: noop.NewTracerProvider().Tracer("x")}
	f := spans.New(tr, true)
	_, ex := f.Start(context.Background(), &spans.RootInfo{Command: "find"})
	ex.End(nil, nil)
	if tr.starts.Load() != 1 {
		t.Fatalf("starts = %d", tr.starts.Load())
	}
}

// errPoolWait is a package-level error value: passing it as an error costs no
// allocation, so the zero-allocation check holds without any unsafe trick in
// the code under test.
var errPoolWait error = &detailed{"pool_wait_timeout"}
