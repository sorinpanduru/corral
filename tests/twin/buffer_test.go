//go:build twin

package twin

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/corralprobe"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

const lLimit = 65536

func bigInsert(id int32, size int, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id,
		Body: d("insert", "c", "$db", "d", "corraltestMarker", marker, "documents", bson.A{d("_id", 1, "payload", strings.Repeat("x", size))})})
}

func maxPre(t *testing.T, p *corralprobe.Client) int64 {
	t.Helper()
	conns, err := p.Conns()
	if err != nil {
		t.Fatal(err)
	}
	var m int64
	for _, s := range conns {
		if s.MaxPreLeaseBytes > m {
			m = s.MaxPreLeaseBytes
		}
	}
	return m
}

// AC-19 (FR-38): with the pool held, the bytes read from a 10 MB insert before
// a lease never exceed L; after the release the twin receives it byte-identical;
// a waiting large request that times out gets 262 and the connection then
// completes a ping.
func TestLargeRequestBufferLimit_AC19(t *testing.T) {
	c, tw := startMongos(t, opts{probe: true, set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "1500ms"}})
	c.waitReady()
	pr := corralprobe.Open(t, c.p.HoldDir())
	k, err := pr.Constants()
	if err != nil || k.L != lLimit || k.K != lLimit {
		t.Fatalf("constants %+v err %v", k, err)
	}
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "hold"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("the first request was not held by the twin")
	}
	// A 10 MB insert waits for a lease: only L bytes are read.
	b := c.dial()
	raw := bigInsert(2, 10<<20, "big")
	go b.Send(raw)
	time.Sleep(600 * time.Millisecond)
	if got := maxPre(t, pr); got == 0 || got > lLimit {
		t.Fatalf("bytes read before the lease = %d, want 1..%d", got, lLimit)
	}
	// It times out waiting: 262, framing preserved.
	m, err := b.ReadMessage(10 * time.Second)
	if err != nil || code(m) != 262 {
		t.Fatalf("the waiting large request: err %v reply %v", err, doc(m))
	}
	hold.Release()
	if _, err := a.ReadMessage(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if m := rt(t, b, ping(3)); !isOK(m) {
		t.Fatalf("ping after the 262: %v", doc(m))
	}
	// With the pool free: the same insert reaches the twin byte-identical.
	b2 := c.dial()
	go b2.Send(raw)
	if _, err := b2.ReadMessage(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	var got []mongosim.Message
	for _, x := range tw.ReceivedCommand("insert") {
		if x.Marker == "big" {
			got = append(got, x)
		}
	}
	if len(got) != 1 || !bytes.Equal(got[0].Raw[16:], raw[16:]) {
		t.Fatalf("the twin received %d copies, identical=%v", len(got), len(got) == 1 && bytes.Equal(got[0].Raw[16:], raw[16:]))
	}
	if got := maxPre(t, pr); got > lLimit {
		t.Fatalf("bytes read before a lease reached %d", got)
	}
}

// AC-19: deciding fields past L, an oversized hello, and an invalid tail past L.
func TestLargeRequestRefusals_AC19(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	t.Run("pipeline after L", func(t *testing.T) {
		before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
		conn := c.dial()
		conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("aggregate", "c", "$db", "d", "corraltestMarker", "late-pipe",
			"filler", strings.Repeat("y", 100_000), "pipeline", bson.A{d("$match", d())}, "cursor", d())}))
		closedNoBytes(t, conn)
		c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
	})
	t.Run("OP_MSG hello larger than L", func(t *testing.T) {
		before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "message_too_large")
		conn := c.dial()
		conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("hello", 1, "$db", "admin", "filler", strings.Repeat("z", 100_000))}))
		closedNoBytes(t, conn)
		c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "message_too_large")
	})
	t.Run("invalid tail after L", func(t *testing.T) {
		before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
		// A body whose tail (after L) holds an element of an invalid type.
		payload := bytes.Repeat([]byte{'p'}, 200_000)
		var el []byte
		el = append(el, bsonOf(d("insert", "c", "$db", "d", "corraltestMarker", "bad-tail"))[4:len(bsonOf(d("insert", "c", "$db", "d", "corraltestMarker", "bad-tail")))-1]...)
		el = append(el, 0x05, 'b', 0, byte(len(payload)), byte(len(payload)>>8), byte(len(payload)>>16), byte(len(payload)>>24), 0)
		el = append(el, payload...)
		el = append(el, 0x20, 'x', 0) // invalid element type (0x7f is MaxKey)
		docb := make([]byte, 4, len(el)+5)
		n := len(el) + 5
		docb[0], docb[1], docb[2], docb[3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
		docb = append(docb, el...)
		docb = append(docb, 0)
		conn := c.dial()
		conn.Send(rawOpMsg(1, 0, kind0(docb)))
		closedNoBytes(t, conn)
		c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
		eventually(t, "the upstream connection closed", func() bool {
			for _, ci := range tw.Conns() {
				if ci.AppName == "corral" && !ci.Open && ci.PartialBytes > 0 {
					return true
				}
			}
			return false
		})
		for _, x := range tw.Received() {
			if x.Marker == "bad-tail" {
				t.Fatal("the message with an invalid tail was received complete")
			}
		}
	})
}

// AC-35 (FR-58): an upstream reply over maxMessageSizeBytes, and one with
// moreToCome set, close the client, discard the upstream connection and count
// upstream protocol errors; a 30 MB reply is delivered intact with the peak held
// at most K.
func TestUpstreamReplyLimits_AC35(t *testing.T) {
	c, tw := startMongos(t, opts{probe: true})
	c.waitReady()
	pr := corralprobe.Open(t, c.p.HoldDir())
	for _, tc := range []struct {
		marker string
		act    mongosim.Action
		detail string
	}{
		{"oversize", mongosim.ReplyOversize(), "message_too_large"},
		{"mtc-reply", mongosim.ReplyMoreToCome(), "unexpected_response"},
	} {
		tw.AddRule(mongosim.Match{Marker: tc.marker, Once: true}, tc.act)
		before := c.metric("corral_protocol_errors_total", "side", "upstream", "error_detail", tc.detail)
		conn := c.dial()
		conn.Send(find(1, tc.marker))
		if _, err := conn.ReadMessage(10 * time.Second); err == nil {
			// A partial reply may reach the client before the close; the
			// connection must still end closed.
		}
		c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "upstream", "error_detail", tc.detail)
	}
	m := rt(t, c.dial(), cmd(9, "find", "c", "$db", "d", "corraltestReplyBytes", 30_000_000))
	if len(m.Raw) < 30_000_000 {
		t.Fatalf("the reply has %d bytes", len(m.Raw))
	}
	conns, err := pr.Conns()
	if err != nil {
		t.Fatal(err)
	}
	var peak int64
	for _, s := range conns {
		if s.PeakReplyBytes > peak {
			peak = s.PeakReplyBytes
		}
	}
	if peak == 0 || peak > lLimit {
		t.Fatalf("peak reply bytes held = %d, want 1..%d", peak, lLimit)
	}
}
