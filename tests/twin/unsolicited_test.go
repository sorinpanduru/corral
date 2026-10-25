//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func waitIdlePool(t *testing.T, c *corral, tw *mongosim.Server, n int) []int {
	t.Helper()
	var idle []int
	eventually(t, "idle pool connections", func() bool {
		idle = tw.IdleConns("corral")
		return len(idle) >= n
	})
	return idle
}

// AC-46 (FR-62): unsolicited bytes on an idle pool connection discard it and
// count an upstream unexpected_response; the next request is sent on another
// connection and succeeds. The same with the twin closing the idle connection
// gives reason=error. FR-62 allows detection while idle or, at the latest, at
// checkout before any request is written on the connection, so the checks
// follow the next request.
func TestUnsolicitedBytesOnIdleConnection_AC46(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.min_size": "1", "pool.max_size": "1"}})
	c.waitReady()
	idle := waitIdlePool(t, c, tw, 1)
	base := c.metric("corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response")
	if err := tw.SendRaw(idle[0], []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if m := rt(t, c.dial(), find(1, "after-unsolicited")); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	c.waitDelta(base, 1, "corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response")
	for _, x := range tw.ReceivedCommand("find") {
		if x.Marker == "after-unsolicited" && x.ConnIndex == idle[0] {
			t.Fatal("the request was written on the connection that had received unsolicited bytes")
		}
	}
	eventually(t, "the discarded connection to be closed", func() bool {
		for _, ci := range tw.Conns() {
			if ci.Index == idle[0] {
				return !ci.Open
			}
		}
		return false
	})
	// The twin closes the idle connection.
	idle = waitIdlePool(t, c, tw, 1)
	errBase := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "error")
	if err := tw.CloseConn(idle[0], false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if m := rt(t, c.dial(), find(2, "after-close")); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	c.waitDelta(errBase, 1, "corral_upstream_connections_closed_total", "kind", "pool", "reason", "error")
}
