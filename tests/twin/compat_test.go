//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func hello(t *testing.T, c *corral) (int64, int64, int64) {
	t.Helper()
	m := rt(t, c.dial(), cmd(1, "hello", 1, "$db", "admin"))
	mn, _ := intField(doc(m), "minWireVersion")
	mx, _ := intField(doc(m), "maxWireVersion")
	return mn, mx, code(m)
}

// AC-10 (FR-18, FR-14): the wire-version acceptance rule and the clamped reply.
func TestWireVersionAcceptance_AC10(t *testing.T) {
	for _, tc := range []struct {
		name             string
		min, max         int32
		wantMin, wantMax int64
	}{
		{"6-27 reports 6-25", 6, 27, 6, 25},
		{"21-21 accepted", 21, 21, 21, 21},
		{"25-30 accepted", 25, 30, 25, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := startMongos(t, opts{}, func(h *mongosim.HelloSpec) { h.MinWire, h.MaxWire = tc.min, tc.max })
			c.waitReady()
			mn, mx, code := hello(t, c)
			if code != 0 || mn != tc.wantMin || mx != tc.wantMax {
				t.Fatalf("reply %d-%d code %d, want %d-%d", mn, mx, code, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// AC-10: ranges that do not overlap 21-25 make the upstream incompatible:
// readiness, code 6 at once for handshakes and requests, one error record.
func TestWireVersionIncompatible_AC10(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max int32
	}{{"6-17", 6, 17}, {"26-30", 26, 30}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := startMongos(t, opts{}, func(h *mongosim.HelloSpec) { h.MinWire, h.MaxWire = tc.min, tc.max })
			c.waitReadyz(503, "upstream_incompatible")
			begin := time.Now()
			if _, _, code := hello(t, c); code != 6 {
				t.Fatalf("handshake: code %d", code)
			}
			if m := rt(t, c.dial(), find(2, "x")); code(m) != 6 {
				t.Fatalf("request: %v", doc(m))
			}
			if time.Since(begin) > time.Second {
				t.Fatal("the refusals were not immediate")
			}
			recs := c.waitRecords("upstream incompatible", 1)
			time.Sleep(1200 * time.Millisecond)
			if n := len(c.records("upstream incompatible")); n != 1 {
				t.Fatalf("%d incompatible records, want one per transition", n)
			}
			if recs[0]["reason"] != "wire_version" || recs[0]["level"] != "ERROR" {
				t.Fatalf("record %v", recs[0])
			}
		})
	}
}

// AC-10: compatible to incompatible and back (single-mongos host re-pointed at a
// replica-set member, Amendment 3).
func TestCompatibleIncompatibleTransitions_AC10(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.min_size": "1", "pool.max_size": "1", "pool.checkout_timeout": "30s"}})
	c.waitReady()
	open := c.dial()
	rt(t, open, find(1, "pre"))
	waitIdlePool(t, c, tw, 1)
	// A request waiting at the moment of the switch.
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	h := c.dial()
	h.Send(find(1, "hold"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	waiter := c.dial()
	waiter.Send(find(1, "waiter"))
	time.Sleep(300 * time.Millisecond)
	inc := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "incompatible")
	spec := tw.Hello()
	tw.SetHello(mongosim.HelloSpec{Role: mongosim.Primary, SetName: "rs0", Hosts: []string{tw.Addr()}, Primary: tw.Addr(), ElectionID: make([]byte, 12), SetVersion: 1})
	c.waitReadyz(503, "upstream_incompatible")
	if m, err := waiter.ReadMessage(10 * time.Second); err != nil || code(m) != 6 {
		t.Fatalf("the waiting request: %v %v", err, m)
	}
	// The open client connection stays open; its next handshake and request get 6.
	if m := rt(t, open, cmd(2, "hello", 1, "$db", "admin")); code(m) != 6 {
		t.Fatalf("handshake while incompatible: %v", doc(m))
	}
	if m := rt(t, open, find(3, "while")); code(m) != 6 {
		t.Fatalf("request while incompatible: %v", doc(m))
	}
	hold.Release()
	h.ReadMessage(5 * time.Second)
	_ = inc
	// Idle pool connections are closed with reason=incompatible.
	eventually(t, "idle connections closed as incompatible", func() bool {
		return c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "incompatible") > inc
	})
	// Back: the next handshake succeeds with the new parameters.
	spec.MaxWriteBatchSize = 4242
	tw.SetHello(spec)
	c.waitReady()
	m := rt(t, open, cmd(4, "hello", 1, "$db", "admin"))
	if v, _ := intField(doc(m), "maxWriteBatchSize"); code(m) != 0 || v != 4242 {
		t.Fatalf("handshake after recovery: %v", doc(m))
	}
}

// AC-10: after the upstream has been reached once and then becomes unreachable,
// handshakes succeed with the cached parameters.
func TestHandshakeUsesCachedParameters_AC10(t *testing.T) {
	c, tw := startMongos(t, opts{}, func(h *mongosim.HelloSpec) { h.MaxWriteBatchSize = 777 })
	c.waitReady()
	tw.Stop()
	c.waitReadyz(503, "upstream_unavailable")
	m := rt(t, c.dial(), cmd(1, "hello", 1, "$db", "admin"))
	if v, _ := intField(doc(m), "maxWriteBatchSize"); code(m) != 0 || v != 777 {
		t.Fatalf("%v", doc(m))
	}
}
