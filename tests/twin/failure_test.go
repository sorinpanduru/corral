//go:build twin

package twin

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-20 (FR-40): the twin resets after receiving a request: the client
// connection is closed with no reply and the upstream connection is discarded;
// the spans are root and round-trip, both in error.
func TestUpstreamResetAfterRequest_AC20(t *testing.T) {
	o, col := traced(t, opts{})
	c, tw := startMongos(t, o)
	c.waitReady()
	tw.AddRule(mongosim.Match{Marker: "reset", Once: true}, mongosim.ResetAfterReceive())
	closedBase := c.metric("corral_upstream_connections_closed_total", "kind", "pool")
	conn := c.dial()
	conn.Send(find(1, "reset"))
	closedNoBytes(t, conn)
	eventually(t, "the upstream connection to be discarded", func() bool {
		return c.metric("corral_upstream_connections_closed_total", "kind", "pool") > closedBase
	})
	c.stop()
	var errs int
	for _, s := range spansNamed(col.Spans(), "find c") {
		if isErrSpan(s) {
			errs++
		}
	}
	if errs != 2 {
		t.Fatalf("%d error spans named %q, want root and round-trip", errs, "find c")
	}
}

// AC-20: the Go driver's retryable write succeeds on its retry after a reset.
func TestRetryableWriteSucceedsAfterReset_AC20(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	tw.AddRule(mongosim.Match{Command: "insert", Once: true}, mongosim.ResetAfterReceive())
	cl := goClient(t, c.proxy)
	coll := cl.Database("d").Collection("c")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := len(tw.ReceivedCommand("insert")); n != 2 {
		t.Fatalf("the twin received %d inserts, want 2 (original and retry)", n)
	}
}

// AC-21 (FR-41): a client that disconnects while its request is held closes
// that upstream connection (client_abandoned) and the next request succeeds; a
// fully read request that disconnects while queued is never sent.
func TestClientDisconnectAbandonsUpstream_AC21(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.max_size": "1"}})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	base := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
	a := c.dial()
	a.Send(find(1, "hold"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("request not held")
	}
	a.Close()
	c.waitDelta(base, 1, "corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
	hold.Release()
	if m := rt(t, c.dial(), find(2, "after")); !isOK(m) {
		t.Fatalf("the next request: %v", doc(m))
	}
	// Queued: A holds the only connection; B's small request is queued, then B
	// leaves; the twin never receives it.
	hold2 := tw.AddRule(mongosim.Match{Marker: "hold2"}, mongosim.Hold())
	a2 := c.dial()
	a2.Send(find(1, "hold2"))
	if !hold2.WaitHeld(1, 10*time.Second) {
		t.Fatal("request not held")
	}
	b := c.dial()
	b.Send(find(1, "queued-gone"))
	time.Sleep(300 * time.Millisecond)
	b.Close()
	time.Sleep(300 * time.Millisecond)
	hold2.Release()
	a2.ReadMessage(10 * time.Second)
	time.Sleep(500 * time.Millisecond)
	for _, m := range tw.Received() {
		if m.Marker == "queued-gone" {
			t.Fatal("the twin received the request of a client that left while queued")
		}
	}
}

// AC-22 (FR-42): a panic in one connection's handler closes that connection;
// a concurrent client completes; the error record is class internal.
func TestHandlerPanicIsolated_AC22(t *testing.T) {
	c, tw := startMongos(t, opts{panicCmd: "boomPanicCmd"})
	c.waitReady()
	tw.AddRule(mongosim.Match{Marker: "slowok"}, mongosim.Delay(600*time.Millisecond))
	other := c.dial()
	other.Send(find(1, "slowok"))
	bad := c.dial()
	bad.Send(cmd(1, "boomPanicCmd", 1, "$db", "d"))
	closedNoBytes(t, bad)
	m, err := other.ReadMessage(10 * time.Second)
	if err != nil || !isOK(m) {
		t.Fatalf("a concurrent client was affected: %v", err)
	}
	recs := c.waitRecords("request handler panic recovered", 1)
	r := recs[0]
	if r["error"] != "internal" || r.Component() != "proxy" {
		t.Fatalf("record %v", r)
	}
	if rt2 := rt(t, c.dial(), ping(5)); !isOK(rt2) {
		t.Fatal("corral stopped serving")
	}
	_ = rawwire.OpCodeMsg
}

// AC-23 (FR-43): while the upstream is down, open client connections stay open;
// after recovery the same connections succeed.
func TestOpenConnectionsSurviveUpstreamOutage_AC23(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	a, b := c.dial(), c.dial()
	rt(t, a, ping(1))
	rt(t, b, find(1, "pre"))
	tw.Stop()
	c.waitReadyz(503, "upstream_unavailable")
	for i, conn := range []*rawwire.Conn{a, b} {
		if _, err := conn.ReadMessage(500 * time.Millisecond); err != rawwire.ErrTimeout {
			t.Fatalf("connection %d during the outage: %v", i, err)
		}
	}
	tw.Restart()
	c.waitReady()
	for i, conn := range []*rawwire.Conn{a, b} {
		if m := rt(t, conn, find(int32(10+i), "post")); !isOK(m) {
			t.Fatalf("connection %d after recovery: %v", i, doc(m))
		}
	}
}
