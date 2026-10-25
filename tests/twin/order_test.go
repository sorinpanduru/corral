//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// AC-40 (FR-13, FR-16, FR-61): names are matched exactly and case-sensitively,
// and the order of local handling is fixed.
func TestCaseSensitivityAndOrder_AC40(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	// OP_MSG HELLO and IsMaster are forwarded (ordinary commands).
	for i, name := range []string{"HELLO", "IsMaster"} {
		marker := "case-" + name
		m := rt(t, conn, mk(int32(i+1), marker, name, 1, "$db", "admin"))
		if code(m) == 6 || code(m) == 186 {
			t.Fatalf("%s was handled as a handshake: %v", name, doc(m))
		}
		found := false
		for _, x := range tw.Received() {
			found = found || x.Marker == marker
		}
		if !found {
			t.Fatalf("%s was not forwarded to the twin", name)
		}
		// Over OP_QUERY they get 352.
		q := rt(t, c.dial(), rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 9, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: d(name, 1)}))
		if code(q) != 352 {
			t.Fatalf("%s over OP_QUERY: %v", name, doc(q))
		}
	}
	// SaslStart is forwarded, not refused with 18.
	m := rt(t, conn, mk(5, "case-sasl", "SaslStart", 1, "$db", "admin"))
	if code(m) == 18 {
		t.Fatalf("SaslStart refused with 18: %v", doc(m))
	}
	found := false
	for _, x := range tw.Received() {
		found = found || x.Marker == "case-sasl"
	}
	if !found {
		t.Fatal("SaslStart not forwarded")
	}
}

// AC-40: with the upstream never reached.
func TestOrderNeverReached_AC40(t *testing.T) {
	c := start(t, opts{})
	t.Run("hello with client: 6 then 186", func(t *testing.T) {
		conn := c.dial()
		if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin", "client", clientMeta())); code(m) != 6 {
			t.Fatalf("first: %v", doc(m))
		}
		if m := rt(t, conn, cmd(2, "hello", 1, "$db", "admin", "client", clientMeta())); code(m) != 186 {
			t.Fatalf("second: %v", doc(m))
		}
	})
	t.Run("loadBalanced with client: 72 then 186", func(t *testing.T) {
		conn := c.dial()
		if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin", "loadBalanced", true, "client", clientMeta())); code(m) != 72 {
			t.Fatalf("first: %v", doc(m))
		}
		if m := rt(t, conn, cmd(2, "hello", 1, "$db", "admin", "client", clientMeta())); code(m) != 186 {
			t.Fatalf("second: %v", doc(m))
		}
	})
	t.Run("local refusals come before fail-fast", func(t *testing.T) {
		conn := c.dial()
		if m := rt(t, conn, cmd(1, "saslStart", 1, "$db", "admin")); code(m) != 18 {
			t.Fatalf("saslStart: %v", doc(m))
		}
		if m := rt(t, conn, cmd(2, "killOp", 1, "op", 1, "$db", "admin")); code(m) != 13 {
			t.Fatalf("killOp: %v", doc(m))
		}
		if m := rt(t, conn, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 3, FullCollection: "d.c", NumberToReturn: 1, Query: d("find", "c")})); code(m) != 352 {
			t.Fatalf("OP_QUERY: %v", doc(m))
		}
		if m := rt(t, conn, cmd(4, "find", "c", "$db", "d")); code(m) != 6 {
			t.Fatalf("find: %v", doc(m))
		}
	})
}

// AC-40: during shutdown after step 3 (hook held), a saslStart gets 91; AC-26:
// any request on a connection not yet closed gets 91, then the connection closes.
func TestShutdownStep3Refusals_AC40(t *testing.T) {
	c, _ := startMongos(t, opts{holdAt: []string{"shutdown_step3"}})
	c.waitReady()
	idle := c.dial()
	idle2 := c.dial()
	rt(t, idle, ping(1)) // both connections are established and served
	rt(t, idle2, ping(1))
	c.p.Signal(sigterm)
	if err := c.p.WaitHeld("shutdown_step3", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if m := rt(t, idle, cmd(2, "saslStart", 1, "$db", "admin")); code(m) != 91 {
		t.Fatalf("saslStart during shutdown: %v", doc(m))
	}
	if m := rt(t, idle2, find(2, "late")); code(m) != 91 {
		t.Fatalf("find during shutdown: %v", doc(m))
	}
	closedNoBytes(t, idle)
	closedNoBytes(t, idle2)
	c.p.Release("shutdown_step3")
	if code, err := c.p.Wait(20 * time.Second); err != nil || code != 0 {
		t.Fatalf("exit %d %v", code, err)
	}
}
