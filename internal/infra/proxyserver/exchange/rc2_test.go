package exchange_test

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// Security RC2, FR-57: the client I/O timeout bounds the whole write of one
// reply, not each chunk of it. A client that drips a multi-chunk reply (here
// 16 KiB every 50 ms, far below the chunk size, so each 64 KiB chunk alone
// would finish within the timeout) is closed with io_timeout close to the
// timeout, not after a multiple of it, and its upstream lease is released so
// that another client is served at once. Waiting for the upstream is still not
// charged (TestUpstreamDelayAndQueueNotCharged_AC34).
func TestSlowDrippingReaderBoundedByOneTimeout_RC2(t *testing.T) {
	const timeout = time.Second
	r := newRig(t, opts{ioTimeout: timeout, poolMax: 1})
	c := r.dial()
	var firstByte atomic.Int64 // unix nanos of the first reply byte the client read
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c", bson.E{Key: "corraltestReplyBytes", Value: 40 << 20})}))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 16<<10)
		nc := c.NetConn()
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			_ = nc.SetReadDeadline(time.Now().Add(time.Second))
			n, err := nc.Read(buf)
			if n > 0 {
				firstByte.CompareAndSwap(0, time.Now().UnixNano())
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}()
	if reason := r.waitServed(); reason != proxy.CloseIOTimeout {
		t.Fatalf("close reason %q, want io_timeout", reason)
	}
	// The bound runs from the first reply byte: the time spent waiting for the
	// upstream before it is not charged to the client (FR-57), so it must not
	// count here either. A per-chunk deadline would take about 125 s.
	fb := firstByte.Load()
	if fb == 0 {
		t.Fatal("the client never received a reply byte")
	}
	if d := time.Since(time.Unix(0, fb)); d > 3*timeout {
		t.Fatalf("closed %v after the first reply byte: the timeout (%v) is applied per chunk, not to the reply", d, timeout)
	}
	if n := r.obs.nTimeouts(); n != 1 {
		t.Fatalf("ClientIOTimeout calls = %d, want 1", n)
	}
	// The lease is released: with a pool of one, another client is served.
	eventually(t, "upstream connection discarded", func() bool {
		rs := r.pobs.reasons()
		return contains(rs, "client_abandoned") || contains(rs, "error")
	})
	d := r.dial()
	d.Send(ping(7))
	m, err := d.ReadMessage(5 * time.Second)
	if err != nil || m.Header.ResponseTo != 7 {
		t.Fatalf("another client was not served after the slow reader was closed: %v", err)
	}
}
