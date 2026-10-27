//go:build twin

package twin

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// FR-47 step 4: with the upstream unreachable after ready, requests wait in the
// checkout queue. When the drain ends (a short drain_timeout) every queued
// client is closed with NO reply, never a synthesized error such as code 6; each
// request is counted as proxy_error; and the process exits 0. Many queued
// clients and repeated runs make a close that races a synthesized reply show up
// reliably.
func TestQueuedClientsCutAtDrainEndGetNoReply_FR47(t *testing.T) {
	const clients = 40
	for run := 0; run < 3; run++ {
		o, col := traced(t, opts{
			holdAt: []string{"shutdown_pool_close"},
			set:    map[string]string{"pool.checkout_timeout": "120s", "proxy.shutdown.drain_timeout": "1s", "pool.max_size": "2"},
		})
		c, tw := startMongos(t, o)
		c.waitReady()
		tw.Stop() // unreachable after ready: requests wait in the queue
		c.waitReadyz(503, "upstream_unavailable")
		var conns []*rawwire.Conn
		for i := 0; i < clients; i++ {
			conn := c.dial()
			conn.Send(find(int32(i+1), "queued"))
			conns = append(conns, conn)
		}
		eventually(t, "every client queued", func() bool { return c.metric("corral_pool_waiters") >= clients })
		proxyErrors := c.metric("corral_requests_total", "outcome", "proxy_error")
		netErrors := c.metric("corral_request_errors_total", "error_class", "network")
		shutdownCloses := c.metric("corral_client_connections_closed_total", "reason", "shutdown")
		c.p.Signal(syscall.SIGTERM)
		// At the drain end the connections are closed (the hook holds before the
		// pools close); no client may get any byte.
		if err := c.p.WaitHeld("shutdown_pool_close", 30*time.Second); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan string, clients)
		for _, conn := range conns {
			wg.Add(1)
			go func(conn *rawwire.Conn) {
				defer wg.Done()
				if err := conn.ExpectClosedNoBytes(15 * time.Second); err != nil {
					errs <- err.Error()
				}
			}(conn)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Errorf("run %d: a queued client was not closed with no reply: %s", run, e)
		}
		// Each cut request is counted as a proxy error.
		if got := c.metric("corral_requests_total", "outcome", "proxy_error") - proxyErrors; got < clients {
			t.Errorf("run %d: %v proxy_error requests counted for %d cut clients", run, got, clients)
		}
		// The queued clients are closed with reason shutdown.
		if got := c.metric("corral_client_connections_closed_total", "reason", "shutdown") - shutdownCloses; got < clients {
			t.Errorf("run %d: %v connections closed with reason shutdown for %d cut clients", run, got, clients)
		}
		// They end as network errors, like a request cut in flight by the same
		// drain end (class network, detail eof).
		if got := c.metric("corral_request_errors_total", "error_class", "network") - netErrors; got < clients {
			t.Errorf("run %d: %v network request errors counted for %d cut clients", run, got, clients)
		}
		c.p.Release("shutdown_pool_close")
		if code, err := c.p.Wait(30 * time.Second); err != nil || code != 0 {
			t.Fatalf("run %d: exit %d, %v", run, code, err)
		}
		// The spans (flushed at exit) carry the detail: error.type network and
		// corral.error.detail eof, with no synthesized-error detail such as
		// pool_wait_timeout or upstream_unavailable.
		cut := 0
		for _, sp := range spansNamed(col.Spans(), "find c") {
			if !isErrSpan(sp) {
				continue
			}
			switch d := sp.Attributes["corral.error.detail"]; {
			case sp.Attributes["error.type"] == "network" && d == "eof":
				cut++
			case d == "upstream_unavailable" || d == "pool_wait_timeout":
				t.Errorf("run %d: a queued request ended with the synthesized detail %q", run, d)
			}
		}
		if cut < clients {
			t.Errorf("run %d: %d spans ended network/eof, want at least %d", run, cut, clients)
		}
		if t.Failed() {
			return
		}
	}
}
