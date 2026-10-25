//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// mtcInsert is an unacknowledged write (moreToCome).
func mtcInsert(id int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Flags: mongosim.FlagMoreToCome,
		Body: d("insert", "c", "$db", "d", "writeConcern", d("w", 0), "corraltestMarker", marker, "documents", []any{d("_id", id)})})
}

func pingsOf(tw *mongosim.Server) []mongosim.Message {
	var out []mongosim.Message
	for _, m := range tw.ReceivedCommand("ping") {
		if m.AppName == "corral" {
			out = append(out, m)
		}
	}
	return out
}

func insertConn(tw *mongosim.Server, marker string) int {
	for _, m := range tw.ReceivedCommand("insert") {
		if m.Marker == marker {
			return m.ConnIndex
		}
	}
	return -1
}

// AC-14 (FR-29, FR-50): after a moreToCome message corral sends its own ping on
// the same upstream connection; while the ping reply is delayed that connection
// is not leased to another client and the same client's next request is not
// forwarded; both proceed after the reply; the ping is not counted or traced.
func TestBarrierHoldsConnectionAndClient_AC14(t *testing.T) {
	o, col := traced(t, opts{set: map[string]string{"pool.max_size": "2"}})
	c, tw := startMongos(t, o)
	c.waitReady()
	reqs := c.metric("corral_requests_total", "command", "ping")
	hold := tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral"}, mongosim.Hold())
	a := c.dial()
	a.Send(mtcInsert(1, "mtc"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("no barrier ping reached the twin")
	}
	ic := insertConn(tw, "mtc")
	if pg := pingsOf(tw); len(pg) != 1 || pg[0].ConnIndex != ic || pg[0].DB != "admin" {
		t.Fatalf("barrier ping %+v, insert on connection %d", pg, ic)
	}
	// The same client's next request is not forwarded while the barrier runs.
	a.Send(find(2, "next"))
	// Another client is served on a different upstream connection.
	m := rt(t, c.dial(), find(1, "other"))
	if !isOK(m) {
		t.Fatalf("another client: %v", doc(m))
	}
	for _, x := range tw.Received() {
		if x.Marker == "other" && x.ConnIndex == ic {
			t.Fatal("the barrier connection was leased to another client")
		}
		if x.Marker == "next" {
			t.Fatal("the next request of the client was forwarded during the barrier")
		}
	}
	hold.Release()
	m, err := a.ReadMessage(10 * time.Second)
	if err != nil || !isOK(m) || m.Header.ResponseTo != 2 {
		t.Fatalf("the next request after the barrier: %v %v", err, m)
	}
	if got := c.metric("corral_requests_total", "command", "ping"); got != reqs {
		t.Fatalf("the barrier ping was counted in requests_total (%v -> %v)", reqs, got)
	}
	c.stop()
	if n := len(spansNamed(col.Spans(), "ping")); n != 0 {
		t.Fatalf("%d ping spans", n)
	}
}

// AC-14: a failed ping discards the upstream connection, the client connection
// stays open and its next request succeeds.
func TestBarrierFailureDiscardsUpstream_AC14(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral", Once: true}, mongosim.ResetAfterReceive())
	a := c.dial()
	a.Send(mtcInsert(1, "mtc-fail"))
	eventually(t, "the upstream connection to be discarded", func() bool {
		return c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "error") >= 1
	})
	if m := rt(t, a, find(2, "after-fail")); !isOK(m) {
		t.Fatalf("the client's next request: %v", doc(m))
	}
	if len(c.records("upstream barrier failed")) == 0 {
		c.waitRecords("upstream barrier failed", 1)
	}
}

// AC-14: a ping reply delayed for three times upstream.connect_timeout is not
// cut off (the barrier has no timeout) and completes when the reply arrives.
func TestBarrierNotCutOffByConnectTimeout_AC14(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"upstream.connect_timeout": "500ms"}})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral"}, mongosim.Hold())
	a := c.dial()
	a.Send(mtcInsert(1, "mtc-slow"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("no barrier ping")
	}
	ic := insertConn(tw, "mtc-slow")
	time.Sleep(1700 * time.Millisecond) // more than three connect timeouts
	for _, ci := range tw.Conns() {
		if ci.Index == ic && !ci.Open {
			t.Fatal("the barrier connection was closed while the ping reply was pending")
		}
	}
	hold.Release()
	if m := rt(t, a, find(2, "after-slow")); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
}

// AC-14: when a check of the host fails twice during a delayed barrier (the
// host becomes unknown), the barrier is aborted at once: the upstream connection
// is discarded and the client connection stays open.
func TestBarrierAbortedWhenHostUnknown_AC14(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral"}, mongosim.Hold())
	a := c.dial()
	a.Send(mtcInsert(1, "mtc-unknown"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("no barrier ping")
	}
	base := c.metric("corral_upstream_connections_closed_total", "kind", "pool")
	tw.FailChecks(-1, mongosim.CheckReset)
	eventually(t, "the barrier to be aborted", func() bool {
		return c.metric("corral_upstream_connections_closed_total", "kind", "pool") > base
	})
	tw.ClearChecks()
	hold.Release()
	c.waitReady()
	if m := rt(t, a, find(2, "after-unknown")); !isOK(m) {
		t.Fatalf("the client connection did not stay usable: %v", doc(m))
	}
}

// AC-14 / AC-42: a client that disconnects during its delayed barrier does not
// abandon the connection: the barrier completes and the same upstream
// connection serves the next lease.
func TestClientLeavesDuringBarrier_AC42(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.max_size": "1"}})
	c.waitReady()
	ab := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
	hold := tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral"}, mongosim.Hold())
	a := c.dial()
	a.Send(mtcInsert(1, "mtc-leave"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("no barrier ping")
	}
	ic := insertConn(tw, "mtc-leave")
	a.Close()
	time.Sleep(300 * time.Millisecond)
	hold.Release()
	if m := rt(t, c.dial(), find(2, "next-lease")); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	var got = -2
	for _, x := range tw.Received() {
		if x.Marker == "next-lease" {
			got = x.ConnIndex
		}
	}
	if got != ic {
		t.Fatalf("the next lease used connection %d, want the barrier connection %d", got, ic)
	}
	c.steady(ab, "corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
}

// AC-42 (FR-29, FR-35): in single-mongos mode a ping reply with 10107 does not
// mark the mongos and one with 91 does; the failed barrier discards the
// connection either way.
func TestBarrierPingErrorsSingleMongos_AC42(t *testing.T) {
	for _, tc := range []struct {
		code   int32
		name   string
		marked bool
	}{{10107, "NotWritablePrimary", false}, {91, "ShutdownInProgress", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, tw := startMongos(t, opts{})
			c.waitReady()
			tw.AddRule(mongosim.Match{Command: "ping", AppName: "corral", Once: true}, mongosim.ReplyError(tc.code, tc.name))
			a := c.dial()
			a.Send(mtcInsert(1, "mtc-err"))
			eventually(t, "the upstream connection to be discarded", func() bool {
				return c.metric("corral_upstream_connections_closed_total", "kind", "pool") >= 1
			})
			if tc.marked {
				eventually(t, "the target marked not primary", func() bool { return len(c.records("upstream target marked not primary")) > 0 })
			} else {
				time.Sleep(700 * time.Millisecond)
				if n := len(c.records("upstream target marked not primary")); n != 0 {
					t.Fatalf("10107 marked a mongos (%d records)", n)
				}
				if code, _ := c.readyz(); code != 200 {
					t.Fatalf("readiness %d", code)
				}
			}
		})
	}
}

// AC-42 and AC-54 (replica-set mode): a barrier ping reply with 10107 or 91
// marks the host not primary and discards the connection, and emits both the
// "upstream target marked not primary" record (with the reply's code) and the
// "upstream barrier failed" record.
func TestBarrierPingNotPrimaryReplicaSet_AC42(t *testing.T) {
	for _, tc := range []struct {
		code int32
		name string
	}{{10107, "NotWritablePrimary"}, {91, "ShutdownInProgress"}} {
		t.Run(tc.name, func(t *testing.T) {
			cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
			c := start(t, opts{upstream: cl.Addrs(), rs: "rs0"})
			c.waitReady()
			pri := cl.Member(cl.PrimaryIndex())
			pri.AddRule(mongosim.Match{Command: "ping", AppName: "corral", Once: true}, mongosim.ReplyError(tc.code, tc.name))
			a := c.dial()
			a.Send(mtcInsert(1, "mtc-rs"))
			marked := c.waitRecords("upstream target marked not primary", 1)
			barrier := c.waitRecords("upstream barrier failed", 1)
			if got := marked[0]["db.response.status_code"]; got != itoa(tc.code) {
				t.Fatalf("db.response.status_code %v, want %d", got, tc.code)
			}
			if marked[0]["error"] != "server" || marked[0]["corral.error.detail"] != "other" {
				t.Fatalf("marked record %v", marked[0])
			}
			_ = barrier
			eventually(t, "the connection to be discarded", func() bool {
				return c.metric("corral_upstream_connections_closed_total", "kind", "pool") >= 1
			})
		})
	}
}
