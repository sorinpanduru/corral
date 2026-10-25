package exchange_test

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

func code(m *rawwire.Message) int64 { return m.Docs[0].Lookup("code").AsInt64() }

// FR-13, FR-14: a handshake is answered locally over OP_MSG and OP_QUERY and
// never reaches the upstream; connectionId is the client connection's ID.
func TestHandshakeAnsweredLocally_FR13(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 5, Body: bson.D{{Key: "hello", Value: 1}, {Key: "helloOk", Value: true}, {Key: "$db", Value: "admin"}}}))
	d := m.Docs[0]
	if m.Header.ResponseTo != 5 || !d.Lookup("isWritablePrimary").Boolean() || d.Lookup("msg").StringValue() != "isdbgrid" || d.Lookup("ok").AsFloat64() != 1 {
		t.Fatalf("OP_MSG handshake reply: %v", d)
	}
	if !d.Lookup("helloOk").Boolean() {
		t.Fatal("helloOk not echoed")
	}
	if d.Lookup("connectionId").AsInt64() < 1 {
		t.Fatalf("connectionId = %v", d.Lookup("connectionId"))
	}
	// OP_QUERY isMaster: an OP_REPLY with the same document shape.
	m = roundTrip(t, c, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 6, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: bson.D{{Key: "isMaster", Value: 1}}}))
	if m.Header.OpCode != rawwire.OpCodeReply || m.Header.ResponseTo != 6 {
		t.Fatalf("OP_QUERY reply: opcode %d responseTo %d", m.Header.OpCode, m.Header.ResponseTo)
	}
	if !m.Docs[0].Lookup("ismaster").Boolean() || m.Flags&2 != 0 {
		t.Fatalf("OP_QUERY handshake reply: %v flags %#x", m.Docs[0], m.Flags)
	}
	if r.twin.ConnAttempts() != 0 {
		t.Fatal("a handshake reached the upstream (or opened a pool connection)")
	}
	if len(r.obs.hs) < 2 {
		t.Fatalf("HandshakeAnswered calls = %d", len(r.obs.hs))
	}
}

// FR-8, R-1: a non-handshake OP_QUERY gets the 352 reply with QueryFailure; so
// do OP_QUERY auth and blocked commands. The connection stays open.
func TestOpQueryRefused_FR8(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	for _, cmd := range []string{"find", "saslStart", "currentOp", "getnonce", "HELLO"} {
		m := roundTrip(t, c, queryCmd(cmd))
		if m.Header.OpCode != rawwire.OpCodeReply || m.Flags&2 == 0 {
			t.Fatalf("%s: opcode %d flags %#x, want an OP_REPLY with QueryFailure", cmd, m.Header.OpCode, m.Flags)
		}
		d := m.Docs[0]
		if d.Lookup("code").AsInt64() != 352 || d.Lookup("codeName").StringValue() != "UnsupportedOpQueryCommand" || d.Lookup("ok").AsFloat64() != 0 {
			t.Fatalf("%s: %v", cmd, d)
		}
		if _, err := d.LookupErr("errorLabels"); err == nil {
			t.Fatal("a 352 refusal carries labels")
		}
	}
	// Still usable.
	if m := roundTrip(t, c, ping(9)); m.Header.ResponseTo != 9 {
		t.Fatal("connection unusable after the refusals")
	}
	res := r.waitResults(6)
	for _, x := range res[:5] {
		if x.Outcome != proxy.OutcomeProxyError || detailOf(x.Err) != "unsupported_opcode" {
			t.Fatalf("result %+v", x)
		}
	}
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	if len(r.obs.protoErrs) != 0 || r.obs.authRej != 0 || len(r.obs.blocked) != 0 {
		t.Fatal("OP_QUERY refusals counted as protocol errors, auth rejections or blocked commands")
	}
}

// FR-23, FR-33, AC-11, AC-17: auth and blocked commands over OP_MSG are refused
// locally and never forwarded.
func TestAuthAndBlockedRefusals_AC17(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	type want struct {
		body bson.D
		code int64
	}
	cases := []want{
		{bson.D{{Key: "saslStart", Value: 1}, {Key: "mechanism", Value: "SCRAM-SHA-256"}, {Key: "$db", Value: "admin"}}, 18},
		{bson.D{{Key: "saslContinue", Value: 1}, {Key: "$db", Value: "admin"}}, 18},
		{bson.D{{Key: "authenticate", Value: 1}, {Key: "$db", Value: "admin"}}, 18},
		{bson.D{{Key: "getnonce", Value: 1}, {Key: "$db", Value: "admin"}}, 115},
		{bson.D{{Key: "logout", Value: 1}, {Key: "$db", Value: "admin"}}, 115},
		{bson.D{{Key: "currentOp", Value: 1}, {Key: "$db", Value: "admin"}}, 13},
		{bson.D{{Key: "killOp", Value: 1}, {Key: "op", Value: 5}, {Key: "$db", Value: "admin"}}, 13},
		{bson.D{{Key: "killAllSessions", Value: bson.A{}}, {Key: "$db", Value: "admin"}}, 13},
		{bson.D{{Key: "killAllSessionsByPattern", Value: bson.A{}}, {Key: "$db", Value: "admin"}}, 13},
		{bson.D{{Key: "aggregate", Value: 1}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$currentOp", Value: bson.D{}}}}}, {Key: "cursor", Value: bson.D{}}, {Key: "$db", Value: "admin"}}, 13},
	}
	for i, tc := range cases {
		m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: int32(i + 1), Body: tc.body}))
		if code(m) != tc.code || m.Header.ResponseTo != int32(i+1) {
			t.Fatalf("case %d: code %d responseTo %d, want %d", i, code(m), m.Header.ResponseTo, tc.code)
		}
		if _, err := m.Docs[0].LookupErr("errorLabels"); err == nil {
			t.Fatalf("case %d: labels on a policy refusal", i)
		}
	}
	if r.twin.ConnAttempts() != 0 {
		t.Fatal("a refused command reached the upstream")
	}
	// Case-sensitive: CurrentOp and an aggregate with $match are forwarded.
	roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 50, Body: bson.D{{Key: "CurrentOp", Value: 1}, {Key: "$db", Value: "admin"}}}))
	roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 51, Body: bson.D{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}}, {Key: "cursor", Value: bson.D{}}, {Key: "$db", Value: "d"}}}))
	if len(r.twin.ReceivedCommand("CurrentOp")) != 1 || len(r.twin.ReceivedCommand("aggregate")) != 1 {
		t.Fatal("case-variant or ordinary commands were not forwarded")
	}
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	if r.obs.authRej != 5 || len(r.obs.blocked) != 5 {
		t.Fatalf("authRejected=%d blocked=%d, want 5 and 5", r.obs.authRej, len(r.obs.blocked))
	}
}

// FR-39 (AC-13): checkout refusals carry labels for 262, 6 and 91; the labels
// follow the request shape; moreToCome refusals get no reply.
func TestCheckoutRefusalReplies_AC13(t *testing.T) {
	r := newRig(t, opts{})
	r.gate.set(pool.Admission{Kind: pool.FailFast, Detail: "upstream_auth_failed"})
	c := r.dial()
	retryable := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "txnNumber", Value: int64(3)}, {Key: "$db", Value: "d"}}})
	m := roundTrip(t, c, retryable)
	if code(m) != 6 || m.Docs[0].Lookup("codeName").StringValue() != "HostUnreachable" {
		t.Fatalf("reply: %v", m.Docs[0])
	}
	labels := func(m *rawwire.Message) []string {
		v, err := m.Docs[0].LookupErr("errorLabels")
		if err != nil {
			return nil
		}
		vals, _ := v.Array().Values()
		var out []string
		for _, x := range vals {
			out = append(out, x.StringValue())
		}
		return out
	}
	if l := labels(m); len(l) != 1 || l[0] != "RetryableWriteError" {
		t.Fatalf("labels %v, want RetryableWriteError", l)
	}
	inTxn := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Body: bson.D{{Key: "find", Value: "c"}, {Key: "autocommit", Value: false}, {Key: "$db", Value: "d"}}})
	if l := labels(roundTrip(t, c, inTxn)); len(l) != 1 || l[0] != "TransientTransactionError" {
		t.Fatalf("labels %v, want TransientTransactionError", l)
	}
	if l := labels(roundTrip(t, c, findM(3, "x"))); len(l) != 0 {
		t.Fatalf("a plain read got labels %v", l)
	}
	// Details to codes.
	for detail, want := range map[string]int64{"upstream_unavailable": 6, "upstream_incompatible": 6} {
		r.gate.set(pool.Admission{Kind: pool.FailFast, Detail: detail})
		if m := roundTrip(t, c, findM(4, "x")); code(m) != want {
			t.Fatalf("%s: code %d", detail, code(m))
		}
	}
	// moreToCome: no reply, and counted as dropped.
	c.Send(insertMTC(5, "m"))
	noMsg(t, c, 200*time.Millisecond)
	r.gate.set(pool.Admission{Kind: pool.Ready, Target: r.twin.Addr()})
	if m := roundTrip(t, c, ping(6)); m.Header.ResponseTo != 6 {
		t.Fatal("connection unusable")
	}
	if r.twin.ConnAttempts() == 0 {
		t.Fatal("the last ping must have used the upstream")
	}
}

// FR-27: queue full and wait timeout replies (code 262, labels).
func TestQueueFullAndTimeoutReplies_AC13(t *testing.T) {
	r := newRig(t, opts{poolMax: 1, maxWaiters: 1, checkout: 400 * time.Millisecond})
	h := r.twin.AddRule(mongosimMatch("blocker"), holdAction())
	a := r.dial()
	a.Send(findM(1, "blocker"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("blocker not held")
	}
	w := r.dial()
	w.Send(findM(2, "waiter"))
	eventually(t, "waiter queued", func() bool { return r.p.Stats().Waiters == 1 })
	full := r.dial()
	m := roundTrip(t, full, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "txnNumber", Value: int64(1)}, {Key: "$db", Value: "d"}}}))
	if code(m) != 262 {
		t.Fatalf("queue full: code %d", code(m))
	}
	if v, err := m.Docs[0].LookupErr("errorLabels"); err != nil {
		t.Fatalf("queue-full reply lacks labels: %v", v)
	}
	m, err := w.ReadMessage(5 * time.Second)
	if err != nil || code(m) != 262 {
		t.Fatalf("wait timeout reply: %v", err)
	}
	h.Release()
}

// FR-47: after the listener is closed, a message first read late gets 91 and the
// connection is then closed; an idle connection is closed at once.
func TestShutdownLateMessage_FR47(t *testing.T) {
	r := newRig(t, opts{})
	busy := r.dial()
	roundTrip(t, busy, ping(1))
	idle := r.dial()
	r.sd.Begin()
	r.sd.ListenerClosed()
	// A message first read after step 3.
	m := roundTrip(t, busy, findM(2, "late"))
	if code(m) != 91 || m.Docs[0].Lookup("codeName").StringValue() != "ShutdownInProgress" {
		t.Fatalf("late message: %v", m.Docs[0])
	}
	if _, err := busy.ReadMessage(5 * time.Second); err == nil {
		t.Fatal("the connection stayed open after the 91 reply")
	}
	_ = idle
}

// FR-42: a panic in a handler closes that connection only.
func TestPanicRecovered_FR42(t *testing.T) {
	r := newRig(t, opts{panicCommand: "boom"})
	bad := r.dial()
	bad.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "boom", Value: 1}, {Key: "$db", Value: "d"}}}))
	if err := bad.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("connection not closed: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseInternalError {
		t.Fatalf("close reason %q", reason)
	}
	r.obs.mu.Lock()
	n := len(r.obs.panics)
	r.obs.mu.Unlock()
	if n != 1 {
		t.Fatalf("PanicRecovered calls = %d", n)
	}
	if m := roundTrip(t, r.dial(), ping(2)); m.Header.ResponseTo != 2 {
		t.Fatal("another client was affected")
	}
}
