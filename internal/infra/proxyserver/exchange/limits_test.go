package exchange_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func bigInsert(reqID int32, size int, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: reqID,
		Body:      bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "shop"}, {Key: "corraltestMarker", Value: marker}},
		Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "blob", Value: strings.Repeat("b", size)}}}}}})
}

func TestChunkConstants_AC35(t *testing.T) {
	if exchange.ReplyChunk != 65536 || exchange.ForwardChunk != 65536 || proxy.RequestBufferLimit != 65536 {
		t.Fatalf("L=%d K=%d forward=%d", proxy.RequestBufferLimit, exchange.ReplyChunk, exchange.ForwardChunk)
	}
}

// AC-19, FR-38: a large request that waits for a lease is held in memory only
// up to L (the probe sees the bytes read before the lease); it is forwarded in
// full once a lease arrives.
func TestLargeRequestBufferBound_AC19(t *testing.T) {
	r := newRig(t, opts{poolMax: 1})
	h := r.twin.AddRule(mongosim.Match{Marker: "blocker"}, mongosim.Hold())
	a := r.dial()
	a.Send(findM(1, "blocker"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("blocker not held")
	}
	b := r.dial()
	msg := bigInsert(2, 600_000, "big")
	if err := b.Send(msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	pre, _ := r.probe.get()
	if pre == 0 || pre > proxy.RequestBufferLimit {
		t.Fatalf("bytes read before the lease = %d, want 1..%d", pre, proxy.RequestBufferLimit)
	}
	if len(r.twin.ReceivedCommand("insert")) != 0 {
		t.Fatal("the request went upstream without a lease")
	}
	h.Release()
	if m, err := b.ReadMessage(10 * time.Second); err != nil || m.Header.ResponseTo != 2 {
		t.Fatalf("large request not answered: %v", err)
	}
	got := lastReceived(t, r, "insert")
	if !sameModuloRequestID(got.Raw, expectedForwarded(msg)) {
		t.Fatalf("forwarded %d bytes, want %d identical", len(got.Raw), len(msg))
	}
}

// AC-19: a waiting large request that times out in the queue still keeps the
// connection framed: the rest is read and discarded, the 262 reply comes, and
// the next message works.
func TestLargeRequestQueueTimeoutKeepsFraming_AC19(t *testing.T) {
	r := newRig(t, opts{poolMax: 1, checkout: 300 * time.Millisecond})
	h := r.twin.AddRule(mongosim.Match{Marker: "blocker"}, mongosim.Hold())
	a := r.dial()
	a.Send(findM(1, "blocker"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("blocker not held")
	}
	b := r.dial()
	b.Send(bigInsert(2, 400_000, "big"))
	b.Send(ping(3))
	m, err := b.ReadMessage(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.ResponseTo != 2 || m.Docs[0].Lookup("code").AsInt64() != 262 {
		t.Fatalf("reply: responseTo=%d code=%v", m.Header.ResponseTo, m.Docs[0].Lookup("code"))
	}
	h.Release()
	m, err = b.ReadMessage(10 * time.Second)
	if err != nil || m.Header.ResponseTo != 3 {
		t.Fatalf("the next message after the refusal was not served: %v", err)
	}
	if len(r.twin.ReceivedCommand("insert")) != 0 {
		t.Fatal("the refused request reached the upstream")
	}
}

// AC-19: an aggregate whose pipeline lies beyond the buffer limit, and an
// oversized hello, close the connection with the right details.
func TestDecidingFieldsMustFit_AC19(t *testing.T) {
	pad := strings.Repeat("p", 100_000)
	cases := map[string]struct {
		msg    []byte
		detail string
	}{
		"aggregate pipeline beyond L": {rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "aggregate", Value: "c"}, {Key: "pad", Value: pad}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}}, {Key: "$db", Value: "d"}}}), "malformed_message"},
		"oversized hello":             {rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "hello", Value: 1}, {Key: "pad", Value: pad}, {Key: "$db", Value: "admin"}}}), "message_too_large"},
		"oversized saslStart":         {rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "saslStart", Value: 1}, {Key: "pad", Value: pad}, {Key: "$db", Value: "admin"}}}), "message_too_large"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			c := r.dial()
			c.Send(tc.msg)
			if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
				t.Fatalf("not closed without bytes: %v", err)
			}
			if reason := r.waitServed(); reason != proxy.CloseProtocolError {
				t.Fatalf("close reason %q", reason)
			}
			res := r.waitResults(1)
			if res[0].Outcome != proxy.OutcomeProxyError || detailOf(res[0].Err) != tc.detail {
				t.Fatalf("result %+v detail %q, want %s", res[0], detailOf(res[0].Err), tc.detail)
			}
			if r.twin.ConnAttempts() != 0 {
				t.Fatal("the upstream was contacted")
			}
		})
	}
}

// AC-19, FR-38: a large forwarded request whose tail (after L) is invalid BSON:
// the upstream never receives the complete message, the connection is
// discarded and the client is closed.
func TestInvalidTailAfterL_AC19(t *testing.T) {
	r := newRig(t, opts{})
	roundTrip(t, r.dial(), ping(1)) // opens a pool connection
	idx := r.twin.IdleConns("corral")
	if len(idx) != 1 {
		t.Fatalf("idle conns %v", idx)
	}
	before := r.twin.Conns()[idx[0]].BytesReceived
	msg := bigInsert(9, 300_000, "x")
	bad := append([]byte(nil), msg...)
	bad[len(bad)-3] = 0x14 // corrupt a byte near the end: inside the last document
	bad[len(bad)-2] = 0xFF
	c := r.dial()
	c.Send(bad)
	if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("client not closed: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseProtocolError {
		t.Fatalf("close reason %q", reason)
	}
	if len(r.twin.ReceivedCommand("insert")) != 0 {
		t.Fatal("a failing message arrived complete upstream")
	}
	got := r.twin.Conns()[idx[0]].BytesReceived - before
	if got <= 0 || got >= int64(len(expectedForwarded(msg))) {
		t.Fatalf("upstream received %d of %d bytes: must be a partial message", got, len(expectedForwarded(msg)))
	}
	eventually(t, "upstream discarded", func() bool { return contains(r.pobs.reasons(), "client_abandoned") })
	res := r.waitResults(2)
	if last := res[len(res)-1]; last.Command != "insert" || last.Outcome != proxy.OutcomeProxyError || detailOf(last.Err) != "malformed_message" {
		t.Fatalf("result %+v", last)
	}
}

// AC-35, FR-58: a 30 MB reply streams within the chunk bound.
func TestLargeReplyStreamsInChunks_AC35(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 20 * time.Second})
	c := r.dial()
	m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 4, Body: find("c", bson.E{Key: "corraltestReplyBytes", Value: 30 << 20})}))
	if len(m.Raw) < 30<<20 {
		t.Fatalf("reply is %d bytes, want >= 30 MiB", len(m.Raw))
	}
	_, held := r.probe.get()
	if held == 0 || held > exchange.ReplyChunk {
		t.Fatalf("peak reply bytes held = %d, want 1..%d", held, exchange.ReplyChunk)
	}
	if m.Header.ResponseTo != 4 {
		t.Fatalf("responseTo %d", m.Header.ResponseTo)
	}
}

// AC-34, FR-57: a client that sends a header and stalls is closed after the
// timeout.
func TestIOTimeoutStalledHeader_AC34(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 300 * time.Millisecond})
	c := r.dial()
	begin := time.Now()
	c.Send([]byte{0x20, 0, 0, 0, 1, 0, 0}) // seven bytes of a header
	if err := c.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("not closed: %v", err)
	}
	if d := time.Since(begin); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("closed after %v", d)
	}
	if reason := r.waitServed(); reason != proxy.CloseIOTimeout {
		t.Fatalf("close reason %q", reason)
	}
	res := r.waitResults(1)
	if res[0].Command != "other" && res[0].Command != "" {
		t.Fatalf("command %q for an unread command name", res[0].Command)
	}
	if res[0].Outcome != proxy.OutcomeProxyError || classOf(res[0].Err) != "timeout" {
		t.Fatalf("result %+v class %q", res[0], classOf(res[0].Err))
	}
	if r.obs.nTimeouts() != 1 {
		t.Fatalf("ClientIOTimeout calls = %d", r.obs.nTimeouts())
	}
}

// AC-34: a client that stops reading a large reply is closed after the timeout
// and its upstream connection is discarded.
func TestIOTimeoutSlowReader_AC34(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 400 * time.Millisecond})
	c := r.dial()
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c", bson.E{Key: "corraltestReplyBytes", Value: 40 << 20})}))
	// Never read: the socket buffers fill and the write times out.
	if reason := r.waitServed(); reason != proxy.CloseIOTimeout {
		t.Fatalf("close reason %q, want io_timeout", reason)
	}
	eventually(t, "upstream connection discarded", func() bool {
		rs := r.pobs.reasons()
		return contains(rs, "client_abandoned") || contains(rs, "error")
	})
}

// AC-34: an upstream reply delayed for twice the timeout is not cut off, and
// time spent waiting for a lease is not charged to the client.
func TestUpstreamDelayAndQueueNotCharged_AC34(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 300 * time.Millisecond, poolMax: 1})
	r.twin.AddRule(mongosim.Match{Marker: "slow"}, mongosim.Delay(700*time.Millisecond))
	a := r.dial()
	a.Send(findM(1, "slow"))
	time.Sleep(50 * time.Millisecond)
	// b queues behind a for ~650 ms (more than the I/O timeout) with a large,
	// partly read message.
	b := r.dial()
	b.Send(bigInsert(2, 300_000, "queued"))
	ma, err := a.ReadMessage(10 * time.Second)
	if err != nil || ma.Header.ResponseTo != 1 {
		t.Fatalf("delayed reply cut off: %v", err)
	}
	mb, err := b.ReadMessage(10 * time.Second)
	if err != nil || mb.Header.ResponseTo != 2 {
		t.Fatalf("queued request failed: %v", err)
	}
	if r.obs.nTimeouts() != 0 {
		t.Fatalf("ClientIOTimeout calls = %d, want 0", r.obs.nTimeouts())
	}
}

// FR-4: an idle connection is closed after the idle timeout, with no request
// accounting.
func TestIdleTimeout_FR4(t *testing.T) {
	r := newRig(t, opts{idleTimeout: 250 * time.Millisecond})
	c := r.dial()
	if err := c.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("not closed: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseIdleTimeout {
		t.Fatalf("close reason %q", reason)
	}
	if n := len(r.obs.snapshot()); n != 0 {
		t.Fatalf("%d request results for an idle close", n)
	}
	// Not charged while an exchange is active, nor when the timeout is 0.
	r2 := newRig(t, opts{})
	c2 := r2.dial()
	time.Sleep(300 * time.Millisecond)
	if m := roundTrip(t, c2, ping(1)); m.Header.ResponseTo != 1 {
		t.Fatal("connection closed with idle timeout disabled")
	}
}

// AC-21 (Linux), FR-41: a client that disconnects while its request is in
// flight upstream gets its upstream connection discarded (client_abandoned).
func TestDisconnectInFlight_AC21(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("liveness detection is Linux-only")
	}
	r := newRig(t, opts{})
	h := r.twin.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	c := r.dial()
	c.Send(findM(1, "stuck"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("request not held upstream")
	}
	c.Close()
	eventually(t, "the upstream connection discarded", func() bool { return contains(r.pobs.reasons(), "client_abandoned") })
	res := r.waitResults(1)
	if res[0].Outcome != proxy.OutcomeProxyError || classOf(res[0].Err) != "network" {
		t.Fatalf("result %+v class %q", res[0], classOf(res[0].Err))
	}
}

// AC-21 (Linux), FR-41: a client that disconnects while queued leaves the
// queue without any Open on its behalf.
func TestDisconnectWhileQueued_AC21(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("liveness detection is Linux-only")
	}
	r := newRig(t, opts{poolMax: 1})
	h := r.twin.AddRule(mongosim.Match{Marker: "blocker"}, mongosim.Hold())
	a := r.dial()
	a.Send(findM(1, "blocker"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("blocker not held")
	}
	b := r.dial()
	b.Send(findM(2, "queued"))
	eventually(t, "b queued", func() bool { return r.p.Stats().Waiters == 1 })
	attempts := r.twin.ConnAttempts()
	b.Close()
	eventually(t, "b removed from the queue", func() bool { return r.p.Stats().Waiters == 0 })
	if got := r.twin.ConnAttempts(); got != attempts {
		t.Fatalf("a connection was opened for a departed client (%d -> %d)", attempts, got)
	}
	h.Release()
	eventually(t, "a answered", func() bool { _, err := a.ReadMessage(2 * time.Second); return err == nil })
}

// FR-41: a client that closes in the middle of a message: one proxy_error
// request, class network, and the connection ends as client_closed.
func TestClientClosesMidMessage_R3(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	msg := bigInsert(3, 100_000, "x")
	c.Send(msg[:len(msg)/2])
	c.Close()
	if reason := r.waitServed(); reason != proxy.CloseClientClosed {
		t.Fatalf("close reason %q, want client_closed", reason)
	}
	res := r.waitResults(1)
	if len(res) != 1 || res[0].Outcome != proxy.OutcomeProxyError || classOf(res[0].Err) != "network" {
		t.Fatalf("results %+v", res)
	}
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	if len(r.obs.protoErrs) != 0 {
		t.Fatal("a client closing mid-message was reported as a protocol error")
	}
	if res[0].Dur <= 0 {
		t.Fatalf("duration %v not measured to the close", res[0].Dur)
	}
}
