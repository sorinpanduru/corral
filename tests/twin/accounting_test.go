//go:build twin

package twin

import (
	"net"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// AC-39 (FR-2, FR-44): with proxy.max_client_connections=1, three connections
// raise accepted_total by 3 and the open gauge by at most 1; over-limit ones are
// closed with no bytes and counted limit_exceeded.
func TestConnectionLimitAccounting_AC39(t *testing.T) {
	c, _ := startMongos(t, opts{set: map[string]string{"proxy.max_client_connections": "1"}})
	c.waitReady()
	acc := c.metric("corral_client_connections_accepted_total")
	lim := c.metric("corral_client_connections_closed_total", "reason", "limit_exceeded")
	first := c.dial()
	rt(t, first, ping(1))
	var maxGauge float64
	for i := 0; i < 2; i++ {
		over := c.dial()
		closedNoBytes(t, over)
		if g := c.metric("corral_client_connections"); g > maxGauge {
			maxGauge = g
		}
	}
	c.waitDelta(acc, 3, "corral_client_connections_accepted_total")
	c.waitDelta(lim, 2, "corral_client_connections_closed_total", "reason", "limit_exceeded")
	if maxGauge > 1 || c.metric("corral_client_connections") > 1 {
		t.Fatalf("corral_client_connections reached %v", maxGauge)
	}
}

// AC-39 (G-4): across a mixed run of synthesized errors, the sum of
// request_errors_total equals the sum of requests_total{proxy_error}.
func TestRequestErrorsEqualProxyErrors_AC39(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "700ms"}})
	c.waitReady()
	conn := c.dial()
	rt(t, conn, cmd(1, "saslStart", 1, "$db", "admin"))
	rt(t, conn, cmd(2, "killOp", 1, "op", 1, "$db", "admin"))
	rt(t, conn, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 3, FullCollection: "d.c", NumberToReturn: 1, Query: d("find", "c")}))
	rt(t, conn, cmd(4, "getnonce", 1, "$db", "admin"))
	// Pool wait timeout.
	h := tw.AddRule(mongosimMatch("hold"), holdAction())
	a := c.dial()
	a.Send(find(1, "hold"))
	h.WaitHeld(1, 10*time.Second)
	if m := rt(t, c.dial(), find(2, "x")); code(m) != 262 {
		t.Fatalf("%v", doc(m))
	}
	h.Release()
	a.ReadMessage(5 * time.Second)
	// Protocol errors closing connections.
	for _, raw := range [][]byte{rawwire.HTTPGet(), rawwire.RawHeader(15, 1, 0, rawwire.OpCodeMsg, nil)} {
		x := c.dial()
		x.Send(raw)
		closedNoBytes(t, x)
	}
	eventually(t, "the error counts to agree", func() bool {
		var errs float64
		for _, cl := range []string{"network", "timeout", "protocol", "server", "config", "rejected", "internal"} {
			errs += c.metric("corral_request_errors_total", "error_class", cl)
		}
		pe := c.metric("corral_requests_total", "outcome", "proxy_error")
		return pe >= 7 && errs == pe
	})
}

// AC-52 (FR-41, FR-48; R-3): a client that closes mid-message after the first
// byte is one proxy_error request, with the command label if its name was read.
func TestClientClosesMidMessage_AC52(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	t.Run("command name read", func(t *testing.T) {
		c := c.with(t)
		req := c.metric("corral_requests_total", "command", "find", "outcome", "proxy_error")
		neterr := c.metric("corral_request_errors_total", "error_class", "network")
		proto := c.metric("corral_protocol_errors_total")
		closed := c.metric("corral_client_connections_closed_total", "reason", "client_closed")
		dur := c.metric("corral_request_duration_seconds_count", "command", "find")
		raw := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("find", "c", "$db", "d", "filter", d("pad", string(make([]byte, 400))))})
		conn := c.dial()
		conn.SendSplit(raw[:16+100], []int{16 + 100}, 0)
		conn.Close()
		c.waitDelta(req, 1, "corral_requests_total", "command", "find", "outcome", "proxy_error")
		c.waitDelta(neterr, 1, "corral_request_errors_total", "error_class", "network")
		c.waitDelta(closed, 1, "corral_client_connections_closed_total", "reason", "client_closed")
		c.waitDelta(dur, 1, "corral_request_duration_seconds_count", "command", "find")
		c.steady(proto, "corral_protocol_errors_total")
	})
	t.Run("command name not read", func(t *testing.T) {
		c := c.with(t)
		req := c.metric("corral_requests_total", "command", "other", "outcome", "proxy_error")
		conn := c.dial()
		conn.Send([]byte{0x30, 0, 0, 0, 1, 0, 0, 0})
		conn.Close()
		c.waitDelta(req, 1, "corral_requests_total", "command", "other", "outcome", "proxy_error")
	})
	t.Run("partly streamed insert", func(t *testing.T) {
		c := c.with(t)
		rt(t, c.dial(), find(1, "warm"))
		ab := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
		raw := bigInsert(5, 5<<20, "partial")
		conn := c.dial()
		go conn.Send(raw[:len(raw)-(512<<10)])
		eventually(t, "the twin to receive part of the message", func() bool {
			for _, ci := range tw.Conns() {
				if ci.AppName == "corral" && ci.PartialBytes > 1<<20 {
					return true
				}
			}
			return false
		})
		if tc, ok := conn.NetConn().(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		conn.Close()
		c.waitDelta(ab, 1, "corral_upstream_connections_closed_total", "kind", "pool", "reason", "client_abandoned")
		for _, x := range tw.ReceivedCommand("insert") {
			if x.Marker == "partial" {
				t.Fatal("the twin received the complete message")
			}
		}
	})
}
