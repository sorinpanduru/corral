package exchange_test

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

type spansRec struct {
	rec *tracetest.SpanRecorder
	tp  *sdktrace.TracerProvider
}

func newSpansRec(t *testing.T) *spansRec {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return &spansRec{rec: rec, tp: tp}
}

func (s *spansRec) factory() *spans.Factory { return spans.New(s.tp.Tracer("exchange-test"), true) }

type shape struct{ root, checkout, roundtrip sdktrace.ReadOnlySpan }

func (s *spansRec) shape(t *testing.T, want int) shape {
	t.Helper()
	var all []sdktrace.ReadOnlySpan
	eventually(t, "spans", func() bool { all = s.rec.Ended(); return len(all) >= want })
	time.Sleep(50 * time.Millisecond)
	all = s.rec.Ended()
	if len(all) != want {
		t.Fatalf("%d spans ended, want %d", len(all), want)
	}
	var sh shape
	for _, sp := range all {
		switch sp.SpanKind() {
		case trace.SpanKindServer:
			sh.root = sp
		case trace.SpanKindInternal:
			sh.checkout = sp
		case trace.SpanKindClient:
			sh.roundtrip = sp
		}
	}
	return sh
}

func failedSpan(sp sdktrace.ReadOnlySpan) bool { return sp != nil && sp.Status().Code == codes.Error }

func attrOf(sp sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// FR-50: an ordinary leased exchange: root, pool.checkout, round-trip.
func TestSpanShapeOrdinary_FR50(t *testing.T) {
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	roundTrip(t, r.dial(), rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("orders")}))
	sh := sr.shape(t, 3)
	if sh.root == nil || sh.checkout == nil || sh.roundtrip == nil {
		t.Fatalf("shape %+v", sh)
	}
	if sh.root.Name() != "find orders" || failedSpan(sh.root) || failedSpan(sh.checkout) || failedSpan(sh.roundtrip) {
		t.Fatalf("root %q failed=%v", sh.root.Name(), failedSpan(sh.root))
	}
	if sh.checkout.Parent().SpanID() != sh.root.SpanContext().SpanID() || sh.roundtrip.Parent().SpanID() != sh.root.SpanContext().SpanID() {
		t.Fatal("children are not under the root")
	}
	if attrOf(sh.roundtrip, "server.address") == "" || attrOf(sh.roundtrip, "corral.upstream.connection.id") == "" {
		t.Fatal("round-trip lacks the upstream attributes")
	}
}

// FR-50: a moreToCome request: root, checkout and round-trip; the barrier ping
// adds no span.
func TestSpanShapeMoreToCome_FR50(t *testing.T) {
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	eventually(t, "barrier done", func() bool { return len(r.twin.ReceivedCommand("ping")) == 1 && r.p.Stats().InUse == 0 })
	sh := sr.shape(t, 3)
	if sh.root == nil || sh.checkout == nil || sh.roundtrip == nil || failedSpan(sh.root) || failedSpan(sh.roundtrip) {
		t.Fatalf("shape %+v", sh)
	}
	// The root and round-trip end when the message is written: before a delayed
	// ping would complete.
	sr2 := newSpansRec(t)
	r2 := newRig(t, opts{tracer: sr2})
	h := r2.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	c2 := r2.dial()
	c2.Send(insertMTC(1, "y"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("ping not held")
	}
	sr2.shape(t, 3) // all three spans already ended while the ping is held
	h.Release()
}

// FR-50: a synthesized error after a checkout attempt: root and checkout end in
// error, with no round-trip.
func TestSpanShapeSynthesizedAfterCheckout_FR50(t *testing.T) {
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	r.gate.set(pool.Admission{Kind: pool.FailFast, Detail: "upstream_unavailable"})
	m := roundTrip(t, r.dial(), rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c")}))
	if code(m) != 6 {
		t.Fatalf("code %d", code(m))
	}
	sh := sr.shape(t, 2)
	if sh.root == nil || sh.checkout == nil || sh.roundtrip != nil {
		t.Fatalf("shape %+v", sh)
	}
	for _, sp := range []sdktrace.ReadOnlySpan{sh.root, sh.checkout} {
		if !failedSpan(sp) || attrOf(sp, "corral.error.detail") != "upstream_unavailable" || attrOf(sp, "error.type") != "network" {
			t.Fatalf("%q: failed=%v detail=%q type=%q", sp.Name(), failedSpan(sp), attrOf(sp, "corral.error.detail"), attrOf(sp, "error.type"))
		}
		if attrOf(sp, "db.response.status_code") != "" {
			t.Fatal("a synthesized error carries a status code")
		}
	}
}

// FR-50: a synthesized error with no checkout attempt: root only; and the
// no-span cases: handshakes, auth commands, protocol errors.
func TestSpanShapeNoCheckoutAndNoSpans_FR50(t *testing.T) {
	for name, msg := range map[string][]byte{
		"blocked command": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "currentOp", Value: 1}, {Key: "$db", Value: "admin"}}}),
	} {
		sr := newSpansRec(t)
		r := newRig(t, opts{tracer: sr})
		roundTrip(t, r.dial(), msg)
		sh := sr.shape(t, 1)
		if sh.root == nil || !failedSpan(sh.root) || attrOf(sh.root, "corral.error.detail") != "command_blocked" {
			t.Fatalf("%s: root %+v", name, sh.root)
		}
	}
	// OP_QUERY refusal: root only.
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	roundTrip(t, r.dial(), queryCmd("find"))
	sh := sr.shape(t, 1)
	if sh.root == nil || !failedSpan(sh.root) || attrOf(sh.root, "corral.error.detail") != "unsupported_opcode" {
		t.Fatalf("OP_QUERY refusal root: %+v", sh.root)
	}
	// No spans at all: handshake, auth command, protocol error.
	sr = newSpansRec(t)
	r = newRig(t, opts{tracer: sr})
	c := r.dial()
	roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}}))
	roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Body: bson.D{{Key: "saslStart", Value: 1}, {Key: "$db", Value: "admin"}}}))
	bad := r.dial()
	bad.Send(rawwire.HTTPGet())
	bad.ExpectClosedNoBytes(5 * time.Second)
	time.Sleep(100 * time.Millisecond)
	if n := len(sr.rec.Ended()); n != 0 {
		t.Fatalf("%d spans for handshakes, auth commands and protocol errors, want 0", n)
	}
}

// FR-50: outcome unknown (FR-40): root and round-trip end in error; the
// checkout, which succeeded, does not.
func TestSpanShapeOutcomeUnknown_FR50(t *testing.T) {
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	r.twin.AddRule(mongosim.Match{Command: "find"}, mongosim.ResetAfterReceive())
	c := r.dial()
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c")}))
	c.ExpectClosedNoBytes(10 * time.Second)
	sh := sr.shape(t, 3)
	if !failedSpan(sh.root) || !failedSpan(sh.roundtrip) || failedSpan(sh.checkout) {
		t.Fatalf("failed: root=%v roundtrip=%v checkout=%v", failedSpan(sh.root), failedSpan(sh.roundtrip), failedSpan(sh.checkout))
	}
	if attrOf(sh.root, "error.type") != "network" {
		t.Fatalf("error.type = %q", attrOf(sh.root, "error.type"))
	}
}

// FR-23: an upstream ok:0 reply: status code and codeName on root and
// round-trip.
func TestSpanShapeUpstreamError_FR23(t *testing.T) {
	sr := newSpansRec(t)
	r := newRig(t, opts{tracer: sr})
	r.twin.AddRule(mongosim.Match{Command: "find"}, mongosim.ReplyError(13, "Unauthorized"))
	roundTrip(t, r.dial(), rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c")}))
	sh := sr.shape(t, 3)
	for _, sp := range []sdktrace.ReadOnlySpan{sh.root, sh.roundtrip} {
		if !failedSpan(sp) || attrOf(sp, "db.response.status_code") != "13" || attrOf(sp, "corral.db.response.code_name") != "Unauthorized" {
			t.Fatalf("%q: code %q name %q", sp.Name(), attrOf(sp, "db.response.status_code"), attrOf(sp, "corral.db.response.code_name"))
		}
	}
	if failedSpan(sh.checkout) {
		t.Fatal("checkout failed")
	}
}
