package proxyapp_test

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func rsTwin(t *testing.T) (*mongosim.Cluster, []string) {
	c := mongosim.StartReplicaSet(t, 1, mongosim.ClusterOptions{SetName: "rs0"})
	c.SetPrimary(0)
	return c, c.Addrs()
}

func rsArgs() []string { return []string{"--upstream.replica_set=rs0"} }

// AC-25 (in-process): corral_upstream_available matches readiness.
func TestUpstreamAvailableMatchesReadiness_AC25(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	a := start(t, []string{twin.Addr()}, cfgOpts{})
	a.waitReady(true, "")
	eventually(t, "gauge 1", func() bool { return a.val("corral_upstream_available") == 1 })
	twin.Stop()
	a.waitReady(false, health.ReasonUpstreamUnavailable)
	eventually(t, "gauge 0", func() bool { return a.val("corral_upstream_available") == 0 })
	if len(a.records("upstream unavailable")) == 0 {
		t.Fatal("no upstream unavailable record on the transition")
	}
}

// AC-33 (in-process wiring), FR-35: a not-primary reply is forwarded unchanged;
// idle pool connections to that host close with not_primary; new leases wait
// until a check confirms a primary; the marking record carries the code.
func TestNotPrimaryReplyMarksHost_AC33(t *testing.T) {
	cl, addrs := rsTwin(t)
	a := start(t, addrs, cfgOpts{args: rsArgs()})
	a.waitReady(true, "")
	c := a.dial()
	if m := send(t, c, findCmd(1, "warm")); m.Header.ResponseTo != 1 {
		t.Fatal("warm-up failed")
	}
	cl.Member(0).AddRule(mongosim.Match{Marker: "np"}, mongosim.ReplyError(10107, "NotWritablePrimary"))
	m := send(t, c, insertCmd(2, "np"))
	if code(m) != 10107 || m.Docs[0].Lookup("codeName").StringValue() != "NotWritablePrimary" {
		t.Fatalf("the reply was altered: %v", m.Docs[0])
	}
	rec := a.records("upstream target marked not primary")
	eventually(t, "marking record", func() bool { rec = a.records("upstream target marked not primary"); return len(rec) == 1 })
	at := attrs(rec[0])
	if at["db.response.status_code"].String() != "10107" || at["error"].String() != "server" {
		t.Fatalf("record attributes: %v", at)
	}
	eventually(t, "pool connections closed as not_primary", func() bool {
		return a.val("corral_upstream_connections_closed_total", "kind", "pool", "reason", "not_primary") >= 1
	})
	// While no primary is known, requests wait and time out with 262.
	cl.NoPrimary()
	begin := time.Now()
	m = send(t, c, findCmd(3, "wait"))
	if code(m) != 262 || time.Since(begin) < time.Second {
		t.Fatalf("code %d after %v, want 262 after the checkout timeout", code(m), time.Since(begin))
	}
	// A primary reappears: requests succeed again.
	cl.SetPrimary(0)
	eventually(t, "requests served again", func() bool {
		m := send(t, c, findCmd(4, "again"))
		return code(m) != 262 && code(m) != 6
	})
}

// AC-33: writeConcernError 91 with ok:1 marks; code 11000 does not.
func TestWriteConcernErrorMarks_AC33(t *testing.T) {
	cl, addrs := rsTwin(t)
	a := start(t, addrs, cfgOpts{args: rsArgs()})
	a.waitReady(true, "")
	c := a.dial()
	send(t, c, findCmd(1, "warm"))
	cl.Member(0).AddRule(mongosim.Match{Marker: "dup"}, mongosim.ReplyError(11000, "DuplicateKey"))
	send(t, c, insertCmd(2, "dup"))
	time.Sleep(300 * time.Millisecond)
	if n := len(a.records("upstream target marked not primary")); n != 0 {
		t.Fatalf("11000 marked the host (%d records)", n)
	}
	cl.Member(0).AddRule(mongosim.Match{Marker: "wce"}, mongosim.ReplyWriteConcernError(91))
	send(t, c, insertCmd(3, "wce"))
	eventually(t, "marking by writeConcernError", func() bool { return len(a.records("upstream target marked not primary")) == 1 })
}

// M3 (review): a barrier ping reply with a marking code emits both the
// marked-not-primary record and the barrier-failed record.
func TestBarrierMarkingEmitsBothRecords_M3(t *testing.T) {
	cl, addrs := rsTwin(t)
	a := start(t, addrs, cfgOpts{args: rsArgs()})
	a.waitReady(true, "")
	cl.Member(0).AddRule(mongosim.Match{Command: "ping"}, mongosim.ReplyError(10107, "NotWritablePrimary"))
	c := a.dial()
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: 2, Body: bsonD("insert", "c", "$db", "d")}))
	eventually(t, "both records", func() bool {
		return len(a.records("upstream target marked not primary")) == 1 && len(a.records("upstream barrier failed")) == 1
	})
	at := attrs(a.records("upstream target marked not primary")[0])
	if at["db.response.status_code"].String() != "10107" {
		t.Fatalf("the marking record lacks the ping reply's code: %v", at)
	}
	bf := attrs(a.records("upstream barrier failed")[0])
	for _, k := range []string{"error", "corral.error.detail", "server.address", "server.port"} {
		if _, ok := bf[k]; !ok {
			t.Fatalf("barrier failed record lacks %s: %v", k, bf)
		}
	}
}

// M4 (review): upstream protocol errors on forwarded and barrier replies are
// counted (side=upstream) and logged.
func TestUpstreamProtocolErrorsCounted_M4(t *testing.T) {
	cases := []struct {
		name   string
		rule   mongosim.Match
		action mongosim.Action
		detail string
		send   func(c *rawwire.Conn)
	}{
		{"forwarded unknown responseTo", mongosim.Match{Marker: "bad"}, mongosim.ReplyUnknownResponseTo(), "unexpected_response", func(c *rawwire.Conn) { c.Send(findCmd(1, "bad")) }},
		{"forwarded moreToCome reply", mongosim.Match{Marker: "bad"}, mongosim.ReplyMoreToCome(), "unexpected_response", func(c *rawwire.Conn) { c.Send(findCmd(1, "bad")) }},
		{"forwarded oversize", mongosim.Match{Marker: "bad"}, mongosim.ReplyOversize(), "message_too_large", func(c *rawwire.Conn) { c.Send(findCmd(1, "bad")) }},
		{"forwarded bad checksum", mongosim.Match{Marker: "bad"}, mongosim.ReplyChecksum(false), "checksum_mismatch", func(c *rawwire.Conn) { c.Send(findCmd(1, "bad")) }},
		{"forwarded malformed", mongosim.Match{Marker: "bad"}, mongosim.ReplyMalformed(), "malformed_message", func(c *rawwire.Conn) { c.Send(findCmd(1, "bad")) }},
		{"barrier unknown responseTo", mongosim.Match{Command: "ping"}, mongosim.ReplyUnknownResponseTo(), "unexpected_response", func(c *rawwire.Conn) {
			c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: 2, Body: bsonD("insert", "c", "$db", "d")}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
			a := start(t, []string{twin.Addr()}, cfgOpts{})
			a.waitReady(true, "")
			twin.AddRule(tc.rule, tc.action)
			c := a.dial()
			tc.send(c)
			eventually(t, "upstream protocol error counted", func() bool {
				return a.val("corral_protocol_errors_total", "side", "upstream", "error_detail", tc.detail) == 1
			})
			recs := a.records("upstream protocol error")
			if len(recs) != 1 {
				t.Fatalf("%d upstream protocol error records, want 1", len(recs))
			}
			at := attrs(recs[0])
			if at["error"].String() != "protocol" || at["corral.error.detail"].String() != tc.detail {
				t.Fatalf("record: %v", at)
			}
			if a.val("corral_protocol_errors_total", "side", "client", "error_detail", tc.detail) != 0 {
				t.Fatal("counted on the client side")
			}
		})
	}
}

// M5 (review), AC-39: a monitor authentication failure is counted exactly once
// per failing check, in both connect_errors{monitor,server} and
// auth_failures{monitor}; the subset rule holds for each kind.
func TestMonitorAuthFailureCountedOnce_AC39(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: map[string]string{"svc": "synthetic-pass"}})
	twin.SetAuthOutcome(mongosim.Fail18)
	a := start(t, []string{twin.Addr()}, cfgOpts{
		args: []string{"--upstream.auth.mechanism=SCRAM-SHA-256"},
		env:  []string{"CORRAL_UPSTREAM_AUTH_USERNAME=svc", "CORRAL_UPSTREAM_AUTH_PASSWORD=synthetic-pass"},
	})
	a.waitReady(false, health.ReasonUpstreamAuthFailed)
	eventually(t, "several failing checks", func() bool { return a.val("corral_upstream_auth_failures_total", "kind", "monitor") >= 3 })
	starts := 0
	for _, m := range twin.ReceivedCommand("saslStart") {
		if m.AppName == "corral-monitor" {
			starts++
		}
	}
	time.Sleep(50 * time.Millisecond)
	af := a.val("corral_upstream_auth_failures_total", "kind", "monitor")
	ce := a.val("corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "server")
	if af > float64(starts+1) || af < float64(starts-1) {
		t.Fatalf("auth_failures{monitor}=%v for %d authentication attempts: each failure must count once", af, starts)
	}
	if ce != af {
		t.Fatalf("connect_errors{monitor,server}=%v, auth_failures{monitor}=%v: must be equal for monitor-only failures", ce, af)
	}
	for _, kind := range []string{"pool", "monitor"} {
		if a.val("corral_upstream_auth_failures_total", "kind", kind) > a.val("corral_upstream_connect_errors_total", "kind", kind, "error_class", "server") {
			t.Fatalf("%s: auth failures exceed connect errors with class server", kind)
		}
	}
}

// FR-22: a pool authentication failure makes readiness upstream_auth_failed and
// fails queued requests with 6.
func TestPoolAuthFailureFailsRequests_FR22(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: map[string]string{"svc": "synthetic-pass"}})
	a := start(t, []string{twin.Addr()}, cfgOpts{
		args: []string{"--upstream.auth.mechanism=SCRAM-SHA-256"},
		env:  []string{"CORRAL_UPSTREAM_AUTH_USERNAME=svc", "CORRAL_UPSTREAM_AUTH_PASSWORD=synthetic-pass"},
	})
	a.waitReady(true, "")
	twin.SetAuthOutcome(mongosim.Fail18)
	c := a.dial()
	m := send(t, c, findCmd(1, "x"))
	if code(m) != 6 {
		t.Fatalf("code %d, want 6 once authentication fails", code(m))
	}
	a.waitReady(false, health.ReasonUpstreamAuthFailed)
	if m := send(t, a.dial(), findCmd(2, "y")); code(m) != 6 {
		t.Fatalf("code %d with auth failing", code(m))
	}
}

// FR-62 wiring: unsolicited bytes on an idle pool connection reach DiscardIdle.
func TestUnsolicitedBytesDiscardIdleConnection_FR62(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	a := start(t, []string{twin.Addr()}, cfgOpts{})
	a.waitReady(true, "")
	send(t, a.dial(), findCmd(1, "x"))
	var idx []int
	eventually(t, "an idle pool connection", func() bool { idx = twin.IdleConns("corral"); return len(idx) > 0 })
	twin.SendRaw(idx[0], []byte("0123456789"))
	eventually(t, "the connection discarded as error", func() bool {
		return a.val("corral_upstream_connections_closed_total", "kind", "pool", "reason", "error") >= 1
	})
	eventually(t, "unsolicited data counted", func() bool {
		return a.val("corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response") >= 1
	})
}

// FR-18: a switch to an incompatible upstream closes idle pool connections with
// reason incompatible and fails requests with 6.
func TestIncompatibleClosesIdleConnections_FR18(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	a := start(t, []string{twin.Addr()}, cfgOpts{})
	a.waitReady(true, "")
	c := a.dial()
	send(t, c, findCmd(1, "x"))
	twin.SetHello(mongosim.HelloSpec{Role: mongosim.Mongos, MinWire: 6, MaxWire: 17})
	a.waitReady(false, health.ReasonUpstreamIncompatible)
	eventually(t, "idle connection closed as incompatible", func() bool {
		return a.val("corral_upstream_connections_closed_total", "kind", "pool", "reason", "incompatible") >= 1
	})
	if m := send(t, c, findCmd(2, "y")); code(m) != 6 {
		t.Fatalf("code %d, want 6", code(m))
	}
	if len(a.records("upstream incompatible")) != 1 {
		t.Fatalf("incompatible records = %d", len(a.records("upstream incompatible")))
	}
}

// FR-37: a pool connection failure triggers an immediate check even with a very
// long heartbeat.
func TestPoolFailureTriggersImmediateCheck_FR37(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	a := start(t, []string{twin.Addr()}, cfgOpts{args: []string{"--upstream.heartbeat_interval=1h", "--pool.checkout_timeout=1500ms"}})
	a.waitReady(true, "")
	twin.RefuseApp("corral", true)
	before := twin.ConnAttemptsBy("corral-monitor")
	c := a.dial()
	c.Send(findCmd(1, "x"))
	eventually(t, "a monitoring check beyond the heartbeat", func() bool {
		return twin.ConnAttemptsBy("corral-monitor") > before || len(twin.ReceivedCommand("hello")) > 0
	})
	// Readiness follows monitoring: the monitor still succeeds, so it stays ready.
	if s := a.readiness(); !s.Ready {
		t.Fatalf("pool failures alone changed readiness: %+v", s)
	}
}
