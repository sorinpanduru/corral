package spans_test

// SPEC-002 FR-50 / SPEC-001 FR-22, FR-23, FR-32 (as amended): span shapes for
// proxied exchanges. API under test (plans/SPEC-002.md T16):
//
//	spans.New(tr, enabled) *Factory
//	(*Factory).Start(ctx, *RootInfo) (context.Context, Exchange)
//	Exchange.Checkout(ctx) Step; Exchange.RoundTrip(ctx, server, upstreamConnID) Step
//	Exchange.End(err, reply *wire.ReplyFields); Step.End(err, reply)
//	spans.ReplyError(wire.ReplyFields) error
//
// Interface assumptions (stated in the report): End(nil, &f) with an ok:0
// reply marks the span failed with the server code, exactly as
// End(spans.ReplyError(f), nil) does; an upstream reply with ok:1 never marks
// a span failed.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
)

type detailed struct{ detail string }

func (e detailed) Error() string             { return "synthetic refusal text SHOULD-NOT-APPEAR" }
func (e detailed) CorralErrorDetail() string { return e.detail }

func setup(t *testing.T) (*spans.Factory, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return spans.New(tp.Tracer("spans-test"), true), rec
}

func info() *spans.RootInfo {
	return &spans.RootInfo{
		Command: "find", DB: "shop", Collection: "orders", HasCollection: true,
		Client: netip.MustParseAddrPort("192.0.2.7:51234"), ConnID: 42, RequestID: 1001,
		SessionID: [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, HasSession: true,
		CursorID: 777, HasCursor: true, AppName: "synthetic-app", DriverName: "synthetic-driver", DriverVersion: "1.2.3",
	}
}

var server = netip.MustParseAddrPort("127.0.0.1:27017")

// allowed is the FR-23 attribute allowlist.
var allowed = map[string]bool{
	attr.DBSystemName: true, attr.DBOperationName: true, attr.DBNamespace: true, attr.DBCollectionName: true,
	attr.ClientAddress: true, attr.ClientPort: true, attr.ErrorType: true, attr.ErrorDetail: true,
	attr.DBResponseStatusCode: true, attr.DBResponseCodeName: true,
	attr.ServerAddress: true, attr.ServerPort: true, attr.ConnectionID: true, attr.UpstreamConnectionID: true,
	attr.RequestID: true, attr.SessionID: true, attr.CursorID: true, attr.ClientAppName: true,
	attr.ClientDriverName: true, attr.ClientDriverVersion: true,
}

func amap(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func byName(t *testing.T, rec *tracetest.SpanRecorder) (root, checkout, rt sdktrace.ReadOnlySpan, all []sdktrace.ReadOnlySpan) {
	t.Helper()
	all = rec.Ended()
	for _, s := range all {
		switch s.SpanKind() {
		case trace.SpanKindServer:
			if root != nil {
				t.Fatal("two root spans")
			}
			root = s
		case trace.SpanKindInternal:
			if s.Name() != "pool.checkout" {
				t.Fatalf("internal span named %q", s.Name())
			}
			checkout = s
		case trace.SpanKindClient:
			rt = s
		default:
			t.Fatalf("unexpected span kind %v", s.SpanKind())
		}
	}
	return
}

// invariants checked on every shape.
func invariants(t *testing.T, all []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, s := range all {
		for k := range amap(s) {
			if !allowed[k] {
				t.Errorf("span %q carries the attribute %q outside the FR-23 allowlist", s.Name(), k)
			}
		}
		if len(s.Events()) != 0 {
			t.Errorf("span %q has %d events (no exception events allowed)", s.Name(), len(s.Events()))
		}
		if s.Status().Description != "" {
			t.Errorf("span %q has a status description", s.Name())
		}
		if _, ok := amap(s)["db.query.text"]; ok {
			t.Error("db.query.text set")
		}
		if strings.Contains(s.Name(), "SHOULD-NOT-APPEAR") {
			t.Error("error text in the span name")
		}
	}
}

func failed(s sdktrace.ReadOnlySpan) bool { return s.Status().Code == codes.Error }

func okReply() *wire.ReplyFields { return &wire.ReplyFields{HasOK: true, OK: 1} }

// FR-50: an ordinary leased exchange: root, pool.checkout and round-trip.
func TestOrdinaryExchange_FR50(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	co := ex.Checkout(ctx)
	co.End(nil, nil)
	rt := ex.RoundTrip(ctx, server, 99)
	rt.End(nil, okReply())
	ex.End(nil, okReply())

	root, checkout, round, all := byName(t, rec)
	if len(all) != 3 || root == nil || checkout == nil || round == nil {
		t.Fatalf("spans: %d", len(all))
	}
	invariants(t, all)
	if root.Name() != "find orders" || round.Name() != "find orders" || checkout.Name() != "pool.checkout" {
		t.Fatalf("names: %q %q %q", root.Name(), round.Name(), checkout.Name())
	}
	for _, c := range []sdktrace.ReadOnlySpan{checkout, round} {
		if c.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("%q is not a child of the root", c.Name())
		}
	}
	if root.Parent().IsValid() {
		t.Fatal("the root has a parent")
	}
	for _, s := range all {
		if failed(s) {
			t.Fatalf("span %q failed on a successful exchange", s.Name())
		}
	}
	rm := amap(root)
	want := map[string]string{
		attr.DBSystemName: "mongodb", attr.DBOperationName: "find", attr.DBNamespace: "shop", attr.DBCollectionName: "orders",
		attr.ClientAddress: "192.0.2.7", attr.ClientPort: "51234", attr.ConnectionID: "42", attr.RequestID: "1001",
		attr.SessionID: "01234567-89ab-cdef-0123-456789abcdef", attr.CursorID: "777",
		attr.ClientAppName: "synthetic-app", attr.ClientDriverName: "synthetic-driver", attr.ClientDriverVersion: "1.2.3",
	}
	for k, v := range want {
		if rm[k] != v {
			t.Errorf("root %s = %q, want %q", k, rm[k], v)
		}
	}
	tm := amap(round)
	for k, v := range map[string]string{
		attr.DBSystemName: "mongodb", attr.DBOperationName: "find", attr.DBNamespace: "shop", attr.DBCollectionName: "orders",
		attr.ServerAddress: "127.0.0.1", attr.ServerPort: "27017", attr.UpstreamConnectionID: "99", attr.RequestID: "1001",
	} {
		if tm[k] != v {
			t.Errorf("round-trip %s = %q, want %q", k, tm[k], v)
		}
	}
	// client.address is a root attribute only; the app and driver names too.
	for _, k := range []string{attr.ClientAddress, attr.ClientPort, attr.ConnectionID, attr.ClientAppName, attr.ClientDriverName, attr.ClientDriverVersion} {
		if _, ok := tm[k]; ok {
			t.Errorf("round-trip carries the root-only attribute %s", k)
		}
	}
	// The checkout span carries only optional server attributes at most.
	for k := range amap(checkout) {
		switch k {
		case attr.ServerAddress, attr.ServerPort, attr.UpstreamConnectionID:
		default:
			t.Errorf("checkout span carries %s", k)
		}
	}
}

// The name is the command alone when there is no collection; absent optional
// values are not set.
func TestNoCollectionNoOptionals_FR23(t *testing.T) {
	f, rec := setup(t)
	_, ex := f.Start(context.Background(), &spans.RootInfo{Command: "ping", DB: "admin", Client: netip.MustParseAddrPort("[2001:db8::1]:4000"), ConnID: 1, RequestID: 5})
	ex.End(nil, okReply())
	root, _, _, all := byName(t, rec)
	invariants(t, all)
	if root.Name() != "ping" {
		t.Fatalf("name %q", root.Name())
	}
	m := amap(root)
	for _, k := range []string{attr.DBCollectionName, attr.SessionID, attr.CursorID, attr.ClientAppName, attr.ClientDriverName, attr.ClientDriverVersion} {
		if _, ok := m[k]; ok {
			t.Errorf("%s set although absent", k)
		}
	}
	if m[attr.ClientAddress] != "2001:db8::1" || m[attr.ClientPort] != "4000" {
		t.Fatalf("IPv6 client: %q %q", m[attr.ClientAddress], m[attr.ClientPort])
	}
	// A missing database name is not invented.
	f2, rec2 := setup(t)
	_, ex2 := f2.Start(context.Background(), &spans.RootInfo{Command: "ping"})
	ex2.End(nil, nil)
	if _, ok := amap(rec2.Ended()[0])[attr.DBNamespace]; ok {
		t.Fatal("db.namespace set for an unknown database")
	}
}

// FR-50: a moreToCome request: root, checkout and round-trip; the barrier
// ping adds nothing (the caller never creates a span for it).
func TestMoreToComeShape_FR50(t *testing.T) {
	f, rec := setup(t)
	i := info()
	i.Command = "insert"
	ctx, ex := f.Start(context.Background(), i)
	ex.Checkout(ctx).End(nil, nil)
	rt := ex.RoundTrip(ctx, server, 3)
	rt.End(nil, nil) // ended when the message has been written
	ex.End(nil, nil)
	root, checkout, round, all := byName(t, rec)
	if len(all) != 3 || root == nil || checkout == nil || round == nil {
		t.Fatalf("spans: %d", len(all))
	}
	invariants(t, all)
	for _, s := range all {
		if failed(s) {
			t.Fatalf("%q failed", s.Name())
		}
	}
	if root.Name() != "insert orders" {
		t.Fatalf("name %q", root.Name())
	}
}

// FR-50: a synthesized error after a checkout attempt: root and checkout end
// in error, with class and detail and no status code; no round-trip span.
func TestSynthesizedAfterCheckout_FR50(t *testing.T) {
	cases := []struct {
		detail string
		class  string
	}{{"pool_wait_timeout", "timeout"}, {"pool_queue_full", "rejected"}, {"upstream_unavailable", "network"}, {"upstream_auth_failed", "server"}, {"upstream_incompatible", "config"}}
	for _, tc := range cases {
		t.Run(tc.detail, func(t *testing.T) {
			f, rec := setup(t)
			ctx, ex := f.Start(context.Background(), info())
			err := detailed{tc.detail}
			ex.Checkout(ctx).End(err, nil)
			ex.End(err, nil)
			root, checkout, round, all := byName(t, rec)
			if len(all) != 2 || root == nil || checkout == nil || round != nil {
				t.Fatalf("spans: %d (round-trip must be absent)", len(all))
			}
			invariants(t, all)
			for _, s := range []sdktrace.ReadOnlySpan{root, checkout} {
				if !failed(s) {
					t.Fatalf("%q did not end in error", s.Name())
				}
				m := amap(s)
				if m[attr.ErrorType] != tc.class || m[attr.ErrorDetail] != tc.detail {
					t.Fatalf("%q: error.type=%q detail=%q", s.Name(), m[attr.ErrorType], m[attr.ErrorDetail])
				}
				if _, ok := m[attr.DBResponseStatusCode]; ok {
					t.Fatalf("%q: a synthesized error carries a status code", s.Name())
				}
				if _, ok := m[attr.DBResponseCodeName]; ok {
					t.Fatalf("%q: a synthesized error carries a code name", s.Name())
				}
			}
		})
	}
}

// FR-50: a synthesized error with no checkout attempt: root only.
func TestSynthesizedWithoutCheckout_FR50(t *testing.T) {
	for _, d := range []string{"shutting_down", "command_blocked", "unsupported_opcode"} {
		t.Run(d, func(t *testing.T) {
			f, rec := setup(t)
			_, ex := f.Start(context.Background(), info())
			ex.End(detailed{d}, nil)
			all := rec.Ended()
			if len(all) != 1 || all[0].SpanKind() != trace.SpanKindServer {
				t.Fatalf("spans: %d", len(all))
			}
			invariants(t, all)
			m := amap(all[0])
			if !failed(all[0]) || m[attr.ErrorDetail] != d {
				t.Fatalf("root: failed=%v detail=%q", failed(all[0]), m[attr.ErrorDetail])
			}
			wantClass, _ := errclass.DetailClass(errclass.Detail(d))
			if m[attr.ErrorType] != string(wantClass) {
				t.Fatalf("error.type = %q, want %q", m[attr.ErrorType], wantClass)
			}
		})
	}
}

// FR-50: outcome unknown (FR-40): root and round-trip end in error, the
// checkout (which succeeded) does not.
func TestOutcomeUnknown_FR50(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	ex.Checkout(ctx).End(nil, nil)
	rt := ex.RoundTrip(ctx, server, 8)
	cause := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	rt.End(cause, nil)
	ex.End(cause, nil)
	root, checkout, round, all := byName(t, rec)
	if len(all) != 3 {
		t.Fatalf("spans: %d", len(all))
	}
	invariants(t, all)
	if !failed(root) || !failed(round) || failed(checkout) {
		t.Fatalf("failed: root=%v round=%v checkout=%v", failed(root), failed(round), failed(checkout))
	}
	for _, s := range []sdktrace.ReadOnlySpan{root, round} {
		m := amap(s)
		if m[attr.ErrorType] != "network" || m[attr.ErrorDetail] != "ECONNRESET" {
			t.Fatalf("%q: %q %q", s.Name(), m[attr.ErrorType], m[attr.ErrorDetail])
		}
		if _, ok := m[attr.DBResponseStatusCode]; ok {
			t.Fatal("a network error carries a status code")
		}
	}
}

// FR-23: an upstream ok:0 reply gives the status code and codeName on the
// root and the round-trip, class server, detail other.
func TestUpstreamErrorReply_FR23(t *testing.T) {
	f := wire.ReplyFields{HasOK: true, OK: 0, HasCode: true, Code: 11000, CodeName: "DuplicateKey"}
	for name, run := range map[string]func(ex spans.Exchange, ctx context.Context){
		"reply passed": func(ex spans.Exchange, ctx context.Context) {
			ex.Checkout(ctx).End(nil, nil)
			ex.RoundTrip(ctx, server, 4).End(nil, &f)
			ex.End(nil, &f)
		},
		"ReplyError passed": func(ex spans.Exchange, ctx context.Context) {
			ex.Checkout(ctx).End(nil, nil)
			ex.RoundTrip(ctx, server, 4).End(spans.ReplyError(f), nil)
			ex.End(spans.ReplyError(f), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fac, rec := setup(t)
			ctx, ex := fac.Start(context.Background(), info())
			run(ex, ctx)
			root, checkout, round, all := byName(t, rec)
			invariants(t, all)
			if failed(checkout) {
				t.Fatal("the checkout failed")
			}
			for _, s := range []sdktrace.ReadOnlySpan{root, round} {
				m := amap(s)
				if !failed(s) || m[attr.ErrorType] != "server" || m[attr.ErrorDetail] != "other" {
					t.Fatalf("%q: failed=%v %q %q", s.Name(), failed(s), m[attr.ErrorType], m[attr.ErrorDetail])
				}
				if m[attr.DBResponseStatusCode] != "11000" || m[attr.DBResponseCodeName] != "DuplicateKey" {
					t.Fatalf("%q: code %q name %q", s.Name(), m[attr.DBResponseStatusCode], m[attr.DBResponseCodeName])
				}
			}
		})
	}
}

// spans.ReplyError: a ServerError for ok:0, nil otherwise.
func TestReplyError_FR23(t *testing.T) {
	if spans.ReplyError(wire.ReplyFields{HasOK: true, OK: 1}) != nil {
		t.Fatal("ok:1 gave an error")
	}
	if spans.ReplyError(wire.ReplyFields{HasOK: true, OK: 1, HasWCECode: true, WCECode: 64}) != nil {
		t.Fatal("ok:1 with a writeConcernError gave an error")
	}
	if spans.ReplyError(wire.ReplyFields{}) != nil {
		t.Fatal("an empty reply gave an error")
	}
	err := spans.ReplyError(wire.ReplyFields{HasOK: true, OK: 0, HasCode: true, Code: 13, CodeName: "Unauthorized"})
	var se errclass.ServerError
	if !errors.As(err, &se) || se.ServerCode() != 13 || se.ServerCodeName() != "Unauthorized" {
		t.Fatalf("ReplyError = %v", err)
	}
	if c, d := errclass.Classify(err); c != errclass.ClassServer || d != errclass.DetailOther {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
	// An ok:0 reply without a code is still a server error, with no code.
	if err := spans.ReplyError(wire.ReplyFields{HasOK: true, OK: 0}); err == nil {
		t.Fatal("ok:0 without a code gave no error")
	}
}

// ok:1 replies never fail a span, even with a writeConcernError or writeErrors
// in the reply.
func TestOkReplyNeverFails_FR23(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	ex.Checkout(ctx).End(nil, nil)
	r := wire.ReplyFields{HasOK: true, OK: 1, HasWCECode: true, WCECode: 64}
	ex.RoundTrip(ctx, server, 1).End(nil, &r)
	ex.End(nil, &r)
	for _, s := range rec.Ended() {
		if failed(s) {
			t.Fatalf("%q failed on an ok:1 reply", s.Name())
		}
	}
}

// SPEC-001 FR-12, FR-25: values pass through the sanitisers.
func TestValuesAreSanitised_FR25(t *testing.T) {
	f, rec := setup(t)
	i := info()
	i.Command = "a\xffb" + strings.Repeat("c", 100)
	i.Collection = "col\xfflection" + strings.Repeat("x", 2000)
	i.AppName = strings.Repeat("€", 100)
	i.DriverName = strings.Repeat("d", 100)
	i.DriverVersion = "v\xff" + strings.Repeat("9", 100)
	_, ex := f.Start(context.Background(), i)
	ex.End(nil, nil)
	s := rec.Ended()[0]
	m := amap(s)
	if got := m[attr.DBOperationName]; len(got) > 64 || !utf8.ValidString(got) || !strings.HasPrefix(got, "a�b") {
		t.Fatalf("command attribute %q (%d bytes)", got, len(got))
	}
	if !utf8.ValidString(s.Name()) {
		t.Fatal("span name is not valid UTF-8")
	}
	col := m[attr.DBCollectionName]
	if !utf8.ValidString(col) || len(col) < 2000 || !strings.HasPrefix(col, "col�lection") {
		t.Fatalf("collection attribute: %d bytes, valid=%v", len(col), utf8.ValidString(col))
	}
	if got := m[attr.ClientAppName]; len(got) > 128 || !utf8.ValidString(got) || got == "" {
		t.Fatalf("app name %d bytes", len(got))
	}
	if got := m[attr.ClientDriverName]; len(got) > 64 {
		t.Fatalf("driver name %d bytes", len(got))
	}
	if got := m[attr.ClientDriverVersion]; len(got) > 64 || !utf8.ValidString(got) {
		t.Fatalf("driver version %d bytes", len(got))
	}
}

// The command name is not alias-mapped in spans (FR-12: as received).
func TestCommandAsReceived_FR12(t *testing.T) {
	f, rec := setup(t)
	_, ex := f.Start(context.Background(), &spans.RootInfo{Command: "isMaster"})
	ex.End(nil, nil)
	if got := amap(rec.Ended()[0])[attr.DBOperationName]; got != "isMaster" {
		t.Fatalf("operation name %q", got)
	}
}

// Step and Exchange End are safe to call with nil arguments and only once
// matters: a second End does not add spans.
func TestDoubleEnd_Safe(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	st := ex.Checkout(ctx)
	st.End(nil, nil)
	st.End(nil, nil)
	ex.End(nil, nil)
	ex.End(nil, nil)
	if n := len(rec.Ended()); n != 2 {
		t.Fatalf("ended spans = %d", n)
	}
}

// Attribute keys are checked against a sorted list so a stray key shows up in
// the failure text; the exchanges above already enforce the subset rule. This
// test lists every key actually used across all shapes.
func TestKeysUsedAreAllowlisted_FR23(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	ex.Checkout(ctx).End(detailed{"pool_wait_timeout"}, nil)
	ex.RoundTrip(ctx, server, 1).End(&net.OpError{Err: syscall.ECONNRESET}, nil)
	ex.End(&net.OpError{Err: syscall.ECONNRESET}, nil)
	var keys []string
	for _, s := range rec.Ended() {
		for k := range amap(s) {
			if !allowed[k] {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	if len(keys) != 0 {
		t.Fatalf("attributes outside the allowlist: %v", keys)
	}
	_ = attribute.Key("")
}

//go:noinline
func clobberStack(depth int) byte {
	var pad [512]byte
	for i := range pad {
		pad[i] = 0xFF
	}
	if depth == 0 {
		return pad[3]
	}
	return clobberStack(depth-1) ^ pad[depth%512]
}

//go:noinline
func endWithLocalCodeName(ex spans.Exchange, ctx context.Context) {
	// The code name is built from bytes held in this frame.
	var buf [16]byte
	n := copy(buf[:], "Unauthorized")
	f := wire.ReplyFields{HasOK: true, OK: 0, HasCode: true, Code: 13, CodeName: string(buf[:n])}
	st := ex.RoundTrip(ctx, server, 3)
	st.End(nil, &f)
	ex.End(nil, &f)
	for i := range buf {
		buf[i] = 'X' // the caller reuses its local storage
	}
}

// Regression (security veto): a span attribute built from a caller's local
// string stays correct after the caller's stack is reused. Nothing in a span
// may alias the caller's stack memory.
func TestAttributeSurvivesStackReuse_FR23(t *testing.T) {
	f, rec := setup(t)
	ctx, ex := f.Start(context.Background(), info())
	endWithLocalCodeName(ex, ctx)
	for i := 0; i < 20; i++ {
		clobberStack(40)
	}
	var round sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			round = s
		}
	}
	if round == nil {
		t.Fatal("no round-trip span")
	}
	for _, s := range rec.Ended() {
		if got := amap(s)[attr.DBResponseCodeName]; s.SpanKind() != trace.SpanKindInternal && got != "Unauthorized" {
			t.Fatalf("span %q code name = %q after the caller's stack was reused", s.Name(), got)
		}
		if got := amap(s)[attr.DBResponseStatusCode]; s.SpanKind() != trace.SpanKindInternal && got != "13" {
			t.Fatalf("span %q status code = %q", s.Name(), got)
		}
	}
}

// The same holds for an error whose text and detail live on the caller's
// stack: the recorded detail is a copy.
func TestErrorDetailSurvivesStackReuse_FR29(t *testing.T) {
	f, rec := setup(t)
	_, ex := f.Start(context.Background(), info())
	func() {
		var buf [32]byte
		n := copy(buf[:], "shutting_down")
		ex.End(&detailed{string(buf[:n])}, nil)
		for i := range buf {
			buf[i] = 'Z'
		}
	}()
	for i := 0; i < 20; i++ {
		clobberStack(40)
	}
	got := amap(rec.Ended()[0])[attr.ErrorDetail]
	if got != "shutting_down" {
		t.Fatalf("detail = %q", got)
	}
}
