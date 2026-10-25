package exchange_test

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// M1 (review): the reply-held probe is measured at the true peak: a large
// reply holds exactly one full chunk at its peak, never more; a small reply
// holds no more than its own size.
func TestReplyPeakMeasuredAtTruePeak_AC35(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 20 * time.Second})
	c := r.dial()
	m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c", bson.E{Key: "corraltestReplyBytes", Value: 3 << 20})}))
	if len(m.Raw) < 3<<20 {
		t.Fatalf("reply is %d bytes", len(m.Raw))
	}
	_, held := r.probe.get()
	if held != exchange.ReplyChunk {
		t.Fatalf("peak reply bytes held for a %d byte reply = %d, want exactly one chunk (%d)", len(m.Raw), held, exchange.ReplyChunk)
	}
	r2 := newRig(t, opts{})
	small := roundTrip(t, r2.dial(), ping(1))
	_, held = r2.probe.get()
	if held == 0 || held > len(small.Raw) {
		t.Fatalf("peak for a %d byte reply = %d", len(small.Raw), held)
	}
}

// FR-5 order (review): an unknown required flag bit on a large local message is
// malformed_message, not message_too_large.
func TestFlagBitOnLargeLocalMessage_FR5(t *testing.T) {
	big := strings.Repeat("p", 100_000)
	for name, tc := range map[string]struct {
		flags  uint32
		detail string
	}{"required flag bit": {1 << 2, "malformed_message"}, "no flag bit": {0, "message_too_large"}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			c := r.dial()
			c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: tc.flags, Body: bson.D{{Key: "hello", Value: 1}, {Key: "pad", Value: big}, {Key: "$db", Value: "admin"}}}))
			if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
				t.Fatal(err)
			}
			res := r.waitResults(1)
			if detailOf(res[0].Err) != tc.detail {
				t.Fatalf("detail %q, want %s", detailOf(res[0].Err), tc.detail)
			}
		})
	}
}

// FR-59 (review): a moreToCome handshake refusal (loadBalanced, 72) gets no
// reply and counts as dropped; a moreToCome handshake that succeeds is answered
// locally, not dropped.
func TestMoreToComeHandshakeRefusalDropped_FR59(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: flagMore, Body: bson.D{{Key: "hello", Value: 1}, {Key: "loadBalanced", Value: true}, {Key: "$db", Value: "admin"}}}))
	noMsg(t, c, 200*time.Millisecond)
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Flags: flagMore, Body: bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}}))
	noMsg(t, c, 200*time.Millisecond)
	roundTrip(t, c, ping(3))
	res := r.waitResults(3)
	if !res[0].Dropped || res[0].Outcome != proxy.OutcomeProxyError {
		t.Fatalf("refused moreToCome handshake: %+v, want dropped proxy_error", res[0])
	}
	if res[1].Dropped || res[1].Outcome != proxy.OutcomeOK {
		t.Fatalf("answered moreToCome handshake: %+v, want ok and not dropped", res[1])
	}
}

// Spec: a moreToCome request's duration runs to the end of the barrier.
func TestMoreToComeDurationEndsWithBarrier_FR48(t *testing.T) {
	r := newRig(t, opts{})
	h := r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("ping not held")
	}
	time.Sleep(400 * time.Millisecond)
	h.Release()
	res := r.waitResults(1)
	if res[0].Command != "insert" || res[0].Dur < 400*time.Millisecond {
		t.Fatalf("duration %v for a request whose barrier took 400 ms", res[0].Dur)
	}
}

// Review: a slow upstream (delay, then a large reply) does not trip the client
// I/O timeout: the upstream wait is not charged, and the write deadline starts
// with the first reply byte.
func TestSlowUpstreamLargeReplyNoClientTimeout_FR57(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 600 * time.Millisecond})
	r.twin.AddRule(mongosim.Match{Marker: "slow-big"}, mongosim.Delay(1200*time.Millisecond))
	c := r.dial()
	m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c", bson.E{Key: "corraltestMarker", Value: "slow-big"}, bson.E{Key: "corraltestReplyBytes", Value: 6 << 20})}))
	if len(m.Raw) < 6<<20 {
		t.Fatalf("reply %d bytes", len(m.Raw))
	}
	if r.obs.nTimeouts() != 0 {
		t.Fatal("the client I/O timeout fired")
	}
}
