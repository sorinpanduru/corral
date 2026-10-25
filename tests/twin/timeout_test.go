//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-34 (FR-57): client I/O timeouts.
func TestClientIOTimeouts_AC34(t *testing.T) {
	const io = time.Second
	c, tw := startMongos(t, opts{set: map[string]string{"proxy.client_io_timeout": "1s", "pool.max_size": "1", "pool.checkout_timeout": "3s"}})
	c.waitReady()

	t.Run("header then stall", func(t *testing.T) {
		c := c.with(t)
		base := c.metric("corral_client_connections_closed_total", "reason", "io_timeout")
		conn := c.dial()
		begin := time.Now()
		conn.Send([]byte{0x30, 0, 0, 0, 1, 0, 0, 0}) // eight bytes of a header
		closedNoBytes(t, conn)
		if d := time.Since(begin); d < 700*time.Millisecond || d > 5*time.Second {
			t.Fatalf("closed after %v, want about %v", d, io)
		}
		c.waitDelta(base, 1, "corral_client_connections_closed_total", "reason", "io_timeout")
	})

	t.Run("client stops reading a large reply", func(t *testing.T) {
		c := c.with(t)
		base := c.metric("corral_client_connections_closed_total", "reason", "io_timeout")
		up := c.metric("corral_upstream_connections_closed_total", "kind", "pool")
		conn := c.dial()
		conn.Send(cmd(1, "find", "c", "$db", "d", "corraltestReplyBytes", 40_000_000))
		c.waitDelta(base, 1, "corral_client_connections_closed_total", "reason", "io_timeout")
		eventually(t, "the upstream connection to be discarded", func() bool {
			return c.metric("corral_upstream_connections_closed_total", "kind", "pool") > up
		})
	})

	t.Run("upstream delay of twice the timeout is not cut off", func(t *testing.T) {
		tw.AddRule(mongosim.Match{Marker: "slow2", Once: true}, mongosim.Delay(2*io))
		m := rt(t, c.dial(), find(1, "slow2"))
		if !isOK(m) {
			t.Fatalf("%v", doc(m))
		}
	})

	t.Run("queue wait is not charged", func(t *testing.T) {
		c := c.with(t)
		rule := tw.AddRule(mongosim.Match{Marker: "pool-hold"}, mongosim.Hold())
		a := c.dial()
		a.Send(find(1, "pool-hold"))
		if !rule.WaitHeld(1, 10*time.Second) {
			t.Fatal("not held")
		}
		base := c.metric("corral_client_connections_closed_total", "reason", "io_timeout")
		small := c.dial()
		small.Send(find(1, "queued-small"))
		large := c.dial()
		go large.Send(bigInsert(2, 300_000, "queued-large"))
		time.Sleep(2*io + 300*time.Millisecond) // more than twice the I/O timeout
		rule.Release()
		for name, conn := range map[string]*rawwire.Conn{"small": small, "large": large} {
			m, err := conn.ReadMessage(10 * time.Second)
			if err != nil || !isOK(m) {
				t.Fatalf("%s request waiting in the queue: %v %v", name, err, m)
			}
		}
		a.ReadMessage(5 * time.Second)
		c.steady(base, "corral_client_connections_closed_total", "reason", "io_timeout")
	})
}
