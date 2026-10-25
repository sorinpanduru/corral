// Package spans builds the spans of a proxied exchange (SPEC-002 FR-50;
// SPEC-001 FR-19, FR-22, FR-23, FR-32): a SERVER root, a pool.checkout
// INTERNAL child and a round-trip CLIENT child.
//
// Compliance (SPEC-001 FR-23, FR-24, FR-25): only allowlisted attributes are
// set, every client-supplied string goes through sanitize, and errors reach
// spans only through errclass.RecordSpan (class, detail and, for an upstream
// ok:0 reply, the server code and code name). Message bytes never appear.
//
// With tracing disabled the Factory never calls Tracer.Start and the
// Start, Checkout, RoundTrip and End calls allocate nothing.
package spans

import (
	"context"
	"net/netip"
	"strconv"
	"unsafe"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// RootInfo is what the root span records about a request. Collection and
// session and cursor values are used only when their Has flag is set.
type RootInfo struct {
	Command, DB, Collection string
	HasCollection           bool
	Client                  netip.AddrPort
	ConnID, RequestID       int32
	SessionID               [16]byte
	HasSession              bool
	CursorID                int64
	HasCursor               bool
	AppName                 string
	DriverName              string
	DriverVersion           string
}

// Factory creates exchanges.
type Factory struct {
	tr      trace.Tracer
	enabled bool
}

// New returns a Factory. With enabled false it never touches the tracer.
func New(tr trace.Tracer, enabled bool) *Factory { return &Factory{tr: tr, enabled: enabled} }

// Exchange is the span set of one request. The zero value (tracing off) does
// nothing.
type Exchange struct {
	f     *Factory
	root  trace.Span
	name  string
	op    string
	db    string
	coll  string
	hasC  bool
	reqID int32
}

// Step is one child span.
type Step struct{ span trace.Span }

func spanName(op, coll string, has bool) string {
	if has && coll != "" {
		return op + " " + coll
	}
	return op
}

// Start creates the root span. With tracing off it returns ctx unchanged and
// the zero Exchange.
func (f *Factory) Start(ctx context.Context, info *RootInfo) (context.Context, Exchange) {
	if f == nil || !f.enabled {
		return ctx, Exchange{}
	}
	op := sanitize.CommandForTelemetry(info.Command)
	coll := ""
	if info.HasCollection {
		coll = sanitize.CollectionForTelemetry(info.Collection)
	}
	db := ""
	if info.DB != "" {
		db = sanitize.CollectionForTelemetry(info.DB)
	}
	attrs := make([]attribute.KeyValue, 0, 14)
	attrs = append(attrs,
		attribute.String(attr.DBSystemName, "mongodb"),
		attribute.String(attr.DBOperationName, op))
	if db != "" {
		attrs = append(attrs, attribute.String(attr.DBNamespace, db))
	}
	if coll != "" {
		attrs = append(attrs, attribute.String(attr.DBCollectionName, coll))
	}
	if info.Client.IsValid() {
		attrs = append(attrs,
			attribute.String(attr.ClientAddress, info.Client.Addr().WithZone("").String()),
			attribute.Int(attr.ClientPort, int(info.Client.Port())))
	}
	attrs = append(attrs,
		attribute.Int64(attr.ConnectionID, int64(info.ConnID)),
		attribute.Int64(attr.RequestID, int64(info.RequestID)))
	if info.HasSession {
		attrs = append(attrs, attribute.String(attr.SessionID, uuidString(info.SessionID)))
	}
	if info.HasCursor {
		attrs = append(attrs, attribute.Int64(attr.CursorID, info.CursorID))
	}
	if info.AppName != "" {
		attrs = append(attrs, attribute.String(attr.ClientAppName, sanitize.AppName(info.AppName)))
	}
	if info.DriverName != "" {
		attrs = append(attrs, attribute.String(attr.ClientDriverName, sanitize.DriverName(info.DriverName)))
	}
	if info.DriverVersion != "" {
		attrs = append(attrs, attribute.String(attr.ClientDriverVersion, sanitize.DriverVersion(info.DriverVersion)))
	}
	name := spanName(op, coll, info.HasCollection)
	ctx, span := f.tr.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
	return ctx, Exchange{f: f, root: span, name: name, op: op, db: db, coll: coll, hasC: coll != "", reqID: info.RequestID}
}

const hexdigits = "0123456789abcdef"

func uuidString(u [16]byte) string {
	b := make([]byte, 0, 36)
	for i, x := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hexdigits[x>>4], hexdigits[x&15])
	}
	return string(b)
}

// Checkout starts the pool.checkout child.
func (e Exchange) Checkout(ctx context.Context) Step {
	if e.f == nil {
		return Step{}
	}
	_, s := e.f.tr.Start(ctx, "pool.checkout", trace.WithSpanKind(trace.SpanKindInternal))
	return Step{span: s}
}

// RoundTrip starts the round-trip CLIENT child for upstream connection
// upstreamConnID at server.
func (e Exchange) RoundTrip(ctx context.Context, server netip.AddrPort, upstreamConnID uint64) Step {
	if e.f == nil {
		return Step{}
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	attrs = append(attrs,
		attribute.String(attr.DBSystemName, "mongodb"),
		attribute.String(attr.DBOperationName, e.op))
	if e.db != "" {
		attrs = append(attrs, attribute.String(attr.DBNamespace, e.db))
	}
	if e.hasC {
		attrs = append(attrs, attribute.String(attr.DBCollectionName, e.coll))
	}
	if server.IsValid() {
		attrs = append(attrs,
			attribute.String(attr.ServerAddress, server.Addr().WithZone("").String()),
			attribute.Int(attr.ServerPort, int(server.Port())))
	}
	attrs = append(attrs,
		attribute.String(attr.UpstreamConnectionID, strconv.FormatUint(upstreamConnID, 10)),
		attribute.Int64(attr.RequestID, int64(e.reqID)))
	_, s := e.f.tr.Start(ctx, e.name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return Step{span: s}
}

// End ends the root span. A non-nil err, or an upstream ok:0 reply, marks it
// failed through errclass.
func (e Exchange) End(err error, reply *wire.ReplyFields) {
	if e.f == nil {
		return
	}
	finish(e.root, hideErr(err), reply)
}

// End ends the child span the same way.
func (s Step) End(err error, reply *wire.ReplyFields) {
	if s.span == nil {
		return
	}
	finish(s.span, hideErr(err), reply)
}

// hideErr returns a copy of err whose escape-analysis flow is cut, so that
// passing an error to End does not force the caller to heap-allocate the
// boxed value. This is what lets End allocate nothing when tracing is off
// (SPEC-001 FR-19, AC-22): the compiler cannot prove err stays unretained,
// because it is handed to errclass, which calls methods on it.
//
// It is sound because errclass only reads the error during the call and keeps
// nothing that points into the box (class and detail are constants, and the
// code name is a string value copied out). The copy is a byte copy of the two
// interface words into a local interface variable on the stack, so the GC
// and stack growth still see ordinary pointers.
func hideErr(err error) error {
	if err == nil {
		return nil
	}
	var out error
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)),
		unsafe.Slice((*byte)(unsafe.Pointer(&err)), unsafe.Sizeof(err)))
	return out
}

func finish(span trace.Span, err error, reply *wire.ReplyFields) {
	if err == nil && reply != nil {
		err = ReplyError(*reply)
	}
	if err != nil {
		errclass.RecordSpan(span, err)
	}
	span.End()
}

type replyErr struct {
	code int32
	name string
}

func (e *replyErr) Error() string          { return "upstream returned an error reply" }
func (e *replyErr) ServerCode() int32      { return e.code }
func (e *replyErr) ServerCodeName() string { return e.name }

// ReplyError returns a ServerError for an ok:0 reply, and nil otherwise.
func ReplyError(f wire.ReplyFields) error {
	if !f.HasOK || f.OK != 0 {
		return nil
	}
	e := &replyErr{name: f.CodeName}
	if f.HasCode {
		e.code = f.Code
	}
	return e
}
