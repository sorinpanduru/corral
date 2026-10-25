//go:build twin

package twin

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// Regression tests for the review fixes of SPEC-002 (spec behaviour, not
// implementation details).

// FR-58 / metrics: a message refused after its first bytes are read is counted
// with its command label, not "other": an insert with a bad CRC-32C.
func TestBadChecksumCountedWithCommandLabel_FR58(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	reqs := c.metric("corral_requests_total", "command", "insert", "outcome", "proxy_error")
	proto := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "checksum_mismatch")
	conn := c.dial()
	conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Checksum: rawwire.ChecksumInvalid,
		Body: d("insert", "c", "$db", "d", "documents", bson.A{d("_id", 1)})}))
	closedNoBytes(t, conn)
	c.waitDelta(reqs, 1, "corral_requests_total", "command", "insert", "outcome", "proxy_error")
	c.waitDelta(proto, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "checksum_mismatch")
}

// FR-60 / metrics: an aggregate refused for a kind-1 pipeline or a duplicated
// pipeline is labelled aggregate.
func TestRefusedAggregateLabelled_FR60(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	cases := map[string][]byte{
		"kind-1 pipeline": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1,
			Body:      d("aggregate", "c", "$db", "d", "cursor", d()),
			Sequences: []rawwire.Sequence{{ID: "pipeline", Docs: []bson.D{d("$currentOp", d())}}}}),
		"duplicated pipeline": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("aggregate", "c", "$db", "d",
			"pipeline", bson.A{d("$match", d())}, "cursor", d(), "pipeline", bson.A{d("$currentOp", d())})}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c := c.with(t)
			before := c.metric("corral_requests_total", "command", "aggregate", "outcome", "proxy_error")
			conn := c.dial()
			conn.Send(raw)
			closedNoBytes(t, conn)
			c.waitDelta(before, 1, "corral_requests_total", "command", "aggregate", "outcome", "proxy_error")
		})
	}
}

// FR-49 / SPEC-001 FR-8: with tracing on, the "client command blocked" record
// carries trace_id and span_id (the span exists before the record is written).
func TestBlockedRecordCarriesTraceIDs_FR49(t *testing.T) {
	o, _ := traced(t, opts{})
	c, _ := startMongos(t, o)
	c.waitReady()
	if m := rt(t, c.dial(), cmd(1, "currentOp", 1, "$db", "admin")); code(m) != 13 {
		t.Fatalf("%v", doc(m))
	}
	r := c.waitRecords("client command blocked", 1)[0]
	if s, _ := r["trace_id"].(string); len(s) != 32 {
		t.Fatalf("trace_id %v in %v", r["trace_id"], r)
	}
	if s, _ := r["span_id"].(string); len(s) != 16 {
		t.Fatalf("span_id %v in %v", r["span_id"], r)
	}
}

// FR-8: a $query-wrapped OP_QUERY handshake with $readPreference before $query
// still gets the handshake reply.
func TestOpQueryReadPreferenceBeforeQuery_FR8(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	m := rt(t, c.dial(), rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 1, FullCollection: "admin.$cmd", NumberToReturn: -1,
		Query: d("$readPreference", d("mode", "primaryPreferred"), "$query", d("isMaster", 1))}))
	if m.Header.OpCode != rawwire.OpCodeReply || m.Flags != 0 || !isOK(m) || strField(doc(m), "msg") != "isdbgrid" {
		t.Fatalf("opcode %d flags %d %v", m.Header.OpCode, m.Flags, doc(m))
	}
}

// FR-38: an insert with about 66 KB of fields before lsid and txnNumber that
// times out in the queue still gets RetryableWriteError (the labels come from
// the whole message).
func TestQueuedLargeInsertKeepsRetryLabel_FR38(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "1s"}})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "hold"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	b := c.dial()
	big := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Body: d("insert", "c", "$db", "d",
		"filler", strings.Repeat("f", 66_000),
		"lsid", d("id", bson.Binary{Subtype: 4, Data: make([]byte, 16)}), "txnNumber", int64(7),
		"documents", bson.A{d("_id", 1)})})
	go b.Send(big)
	m, err := b.ReadMessage(10 * time.Second)
	hold.Release()
	if err != nil || code(m) != 262 || !hasLabel(m, "RetryableWriteError") {
		t.Fatalf("queued large insert: %v %v", err, m)
	}
}

// relayWithOpReply is an upstream stand-in in front of a twin: a request that
// carries marker is answered with an OP_REPLY frame instead of being forwarded.
func relayWithOpReply(t *testing.T, upstream, marker string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer up.Close()
				go io.Copy(c, up)
				for {
					hdr := make([]byte, 16)
					if _, err := io.ReadFull(c, hdr); err != nil {
						return
					}
					n := int(binary.LittleEndian.Uint32(hdr))
					body := make([]byte, n-16)
					if _, err := io.ReadFull(c, body); err != nil {
						return
					}
					if bytes.Contains(body, []byte(marker)) {
						reqID := binary.LittleEndian.Uint32(hdr[4:])
						docb := bsonOf(d("ok", float64(1)))
						rep := make([]byte, 16, 36+len(docb))
						binary.LittleEndian.PutUint32(rep[0:], uint32(36+len(docb)))
						binary.LittleEndian.PutUint32(rep[8:], reqID)
						binary.LittleEndian.PutUint32(rep[12:], 1) // OP_REPLY
						rep = append(rep, make([]byte, 20)...)
						binary.LittleEndian.PutUint32(rep[16+16:], 1) // numberReturned
						rep = append(rep, docb...)
						c.Write(rep)
						continue
					}
					up.Write(append(hdr, body...))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// FR-58: an upstream OP_REPLY in answer to an OP_MSG is malformed_message.
func TestUpstreamOpReplyIsMalformed_FR58(t *testing.T) {
	tw := mongosTwin(t)
	relay := relayWithOpReply(t, tw.Addr(), "oprep-marker")
	c := start(t, opts{upstream: []string{relay}})
	c.waitReady()
	before := c.metric("corral_protocol_errors_total", "side", "upstream", "error_detail", "malformed_message")
	conn := c.dial()
	conn.Send(find(1, "oprep-marker"))
	closedNoBytes(t, conn)
	c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "upstream", "error_detail", "malformed_message")
}

// FR-37: a network error on a leased upstream connection starts an immediate
// monitoring check (the heartbeat here is 30 s).
func TestLeasedConnectionResetTriggersCheck_FR37(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"upstream.heartbeat_interval": "30s"}})
	c.waitReady()
	rt(t, c.dial(), find(1, "warm"))
	time.Sleep(700 * time.Millisecond)
	checks := monitorChecks(tw)
	tw.AddRule(mongosim.Match{Marker: "reset", Once: true}, mongosim.ResetAfterReceive())
	conn := c.dial()
	conn.Send(find(2, "reset"))
	closedNoBytes(t, conn)
	eventually(t, "an immediate monitoring check", func() bool { return monitorChecks(tw) > checks })
}

// FR-49: the "upstream connection failed" record has only the FR-49 keys, even
// when an upstream error reply caused it (no db.response.status_code).
func TestConnectionFailedRecordKeys_FR49(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.checkout_timeout": "1s"}})
	c.waitReady()
	tw.FailChecksOn("corral", 3, mongosim.CheckError(11600, "InterruptedAtShutdown"))
	rt(t, c.dial(), find(1, "x"))
	rec := c.waitRecords("upstream connection failed", 1)[0]
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "component": true, "error": true,
		"corral.error.detail": true, "server.address": true, "server.port": true, "trace_id": true, "span_id": true, "suppressed_count": true}
	for k := range rec {
		if !allowed[k] {
			t.Fatalf("the record carries %q: %v", k, rec)
		}
	}
}

// FR-22: transient SCRAM failures on the monitoring connection make /readyz 503
// upstream_unavailable (not ready, not upstream_auth_failed).
func TestTransientMonitorAuthFailureNotReady_FR22(t *testing.T) {
	for name, o := range map[string]mongosim.AuthOutcome{"10107": mongosim.ErrorCode(10107, "NotWritablePrimary"), "reset": mongosim.AuthReset} {
		t.Run(name, func(t *testing.T) {
			a := newAuth(t)
			tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: a.users(), RequireAuth: true})
			tw.SetAuthOutcome(o)
			c := start(t, opts{upstream: []string{tw.Addr()}, env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
			c.waitReadyz(503, "upstream_unavailable")
			time.Sleep(1200 * time.Millisecond)
			if code, body := c.readyz(); code != 503 || body != "upstream_unavailable" {
				t.Fatalf("readyz %d %q", code, body)
			}
			tw.SetAuthOutcome(mongosim.Succeed)
			c.waitReady()
		})
	}
}

// G-9 / FR-46: pool-only transient authentication failures leave readiness
// alone (the monitoring connection is fine).
func TestPoolOnlyTransientAuthFailureKeepsReadiness_G9(t *testing.T) {
	a := newAuth(t)
	tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: a.users(), RequireAuth: true})
	c := start(t, opts{upstream: []string{tw.Addr()}, env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256", "pool.checkout_timeout": "1s"}})
	c.waitReady()
	// Wait until the monitoring connection has authenticated (it does so on its
	// check after the target is known), then fail only the next conversation,
	// which is the pool connection's.
	eventually(t, "an authenticated monitoring connection", func() bool {
		for _, ci := range tw.Conns() {
			if ci.AppName == mongosim.MonitorAppName && ci.Authenticated && ci.Open {
				return true
			}
		}
		return false
	})
	tw.QueueAuthOutcomes(mongosim.ErrorCode(91, "ShutdownInProgress"))
	conn := c.dial()
	conn.Send(find(1, "x"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if code, body := c.readyz(); code != 200 {
			t.Fatalf("readiness changed by a pool-only transient auth failure: %d %q", code, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
