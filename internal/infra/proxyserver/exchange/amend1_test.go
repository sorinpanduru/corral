package exchange_test

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func aggregateWithSecondPipeline(size int) []byte {
	pipe := bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 21, Body: bson.D{
		{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: pipe}, {Key: "pad", Value: strings.Repeat("x", size)},
		{Key: "$db", Value: "d"}, {Key: "pipeline", Value: pipe}}})
}

// AC-49 (unit): a 5 MB aggregate with a second pipeline near the end, sent with
// a free lease. The upstream records fewer bytes than the rewritten length and
// no complete message; the client is closed; one RequestCompleted for
// aggregate with proxy_error.
func TestLateDuplicatePipelineNeverCompleteUpstream_AC49(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 30 * time.Second})
	roundTrip(t, r.dial(), ping(1))
	idx := r.twin.IdleConns("corral")
	if len(idx) != 1 {
		t.Fatalf("idle conns %v", idx)
	}
	before := r.twin.Conns()[idx[0]].BytesReceived
	msg := aggregateWithSecondPipeline(5 << 20)
	c := r.dial()
	c.Send(msg)
	if err := c.ExpectClosedNoBytes(20 * time.Second); err != nil {
		t.Fatalf("client not closed without a reply: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseProtocolError {
		t.Fatalf("close reason %q", reason)
	}
	if n := len(r.twin.ReceivedCommand("aggregate")); n != 0 {
		t.Fatalf("the upstream received %d complete aggregate messages", n)
	}
	got := r.twin.Conns()[idx[0]].BytesReceived - before
	want := int64(len(expectedForwarded(msg)))
	if got <= 0 || got >= want {
		t.Fatalf("upstream received %d of %d bytes: want a strict, non-empty prefix", got, want)
	}
	eventually(t, "the upstream connection discarded", func() bool { return contains(r.pobs.reasons(), "client_abandoned") })
	res := r.waitResults(2)
	var agg []result
	for _, x := range res {
		if x.Command == "aggregate" {
			agg = append(agg, x)
		}
	}
	if len(agg) != 1 || agg[0].Outcome != proxy.OutcomeProxyError || detailOf(agg[0].Err) != "malformed_message" {
		t.Fatalf("aggregate results: %+v", agg)
	}
}

// AC-49 (unit, checksum): a valid body with a wrong checksum: the upstream never
// receives the last body byte, the client is closed with checksum_mismatch.
func TestWrongChecksumHoldsBackLastByte_AC49(t *testing.T) {
	r := newRig(t, opts{})
	roundTrip(t, r.dial(), ping(1))
	idx := r.twin.IdleConns("corral")
	before := r.twin.Conns()[idx[0]].BytesReceived
	msg := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 31, Checksum: rawwire.ChecksumInvalid,
		Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "pad", Value: strings.Repeat("z", 200_000)}}})
	c := r.dial()
	c.Send(msg)
	if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("client not closed: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseProtocolError {
		t.Fatalf("close reason %q", reason)
	}
	if len(r.twin.ReceivedCommand("insert")) != 0 {
		t.Fatal("the message arrived complete upstream")
	}
	forwardedLen := int64(len(expectedForwarded(msg)))
	got := r.twin.Conns()[idx[0]].BytesReceived - before
	if got >= forwardedLen {
		t.Fatalf("upstream received %d bytes of a %d byte message: the last byte must be held back", got, forwardedLen)
	}
	res := r.waitResults(2)
	last := res[len(res)-1]
	if last.Command != "insert" || detailOf(last.Err) != "checksum_mismatch" {
		t.Fatalf("result %+v (detail %q)", last, detailOf(last.Err))
	}
	eventually(t, "upstream discarded", func() bool { return contains(r.pobs.reasons(), "client_abandoned") })
}

// A valid checksummed large message passes (the held byte is released only
// after every check).
func TestValidChecksumReleasesHeldByte_AC49(t *testing.T) {
	r := newRig(t, opts{})
	msg := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 32, Checksum: rawwire.ChecksumValid,
		Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "pad", Value: strings.Repeat("z", 200_000)}}})
	c := r.dial()
	m := roundTrip(t, c, msg)
	if m.Header.ResponseTo != 32 {
		t.Fatal("not answered")
	}
	got := lastReceived(t, r, "insert")
	if !sameModuloRequestID(got.Raw, expectedForwarded(msg)) {
		t.Fatal("forwarded message differs")
	}
}

// AC-44 (unit): every AC-44 malformed message within L is refused before any
// checkout, and the upstream receives nothing.
func TestMalformedWithinLNeverForwarded_AC44(t *testing.T) {
	pipe := bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}
	nest := func(n int) bson.D {
		d := bson.D{{Key: "leaf", Value: 1}}
		for i := 1; i < n; i++ {
			d = bson.D{{Key: "d", Value: d}}
		}
		return d
	}
	bodyAgg := bson.D{{Key: "aggregate", Value: "c"}, {Key: "$db", Value: "d"}}
	cases := map[string][]byte{
		"kind-1 pipeline after body":  rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bodyAgg, Sequences: []rawwire.Sequence{{ID: "pipeline", Docs: []bson.D{{{Key: "$match", Value: bson.D{}}}}}}}),
		"kind-1 pipeline before body": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, SequencesFirst: true, Body: bodyAgg, Sequences: []rawwire.Sequence{{ID: "pipeline", Docs: []bson.D{{{Key: "$match", Value: bson.D{}}}}}}}),
		"two pipelines":               rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: pipe}, {Key: "pipeline", Value: pipe}, {Key: "$db", Value: "d"}}}),
		"two $db":                     rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "a"}, {Key: "$db", Value: "b"}}}),
		"repeated command name":       rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "find", Value: "a"}, {Key: "$db", Value: "d"}, {Key: "find", Value: "b"}}}),
		"1025 levels":                 rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "find", Value: "c"}, {Key: "n", Value: nest(1024)}, {Key: "$db", Value: "d"}}}),
	}
	// An overrunning kind-1 inner length, built by hand.
	good := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}}, Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "_id", Value: 1}}}}}})
	over := append([]byte(nil), good...)
	over[len(over)-12] = 0x7F // inner document length byte near the end
	over[len(over)-11] = 0x7F
	cases["kind-1 inner overrun"] = over
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			c := r.dial()
			c.Send(msg)
			if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
				t.Fatalf("not closed without a reply: %v", err)
			}
			if reason := r.waitServed(); reason != proxy.CloseProtocolError {
				t.Fatalf("close reason %q", reason)
			}
			if r.twin.ConnAttempts() != 0 || len(r.twin.Received()) != 0 {
				t.Fatal("the upstream was contacted for a malformed message")
			}
			res := r.waitResults(1)
			if res[0].Outcome != proxy.OutcomeProxyError || detailOf(res[0].Err) != "malformed_message" {
				t.Fatalf("result %+v detail %q", res[0], detailOf(res[0].Err))
			}
		})
	}
	// Accepted: 1024 levels and an insert with kind-1 documents.
	r := newRig(t, opts{})
	c := r.dial()
	if m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "find", Value: "c"}, {Key: "n", Value: nest(1023)}, {Key: "$db", Value: "d"}}})); m.Header.ResponseTo != 1 {
		t.Fatal("1024 levels refused")
	}
	if m := roundTrip(t, c, good); m.Header.ResponseTo != 1 {
		t.Fatal("an insert with kind-1 documents refused")
	}
}

// FR-35, G-20: after a forwarded reply, ReplyCodes gets the top-level and
// writeConcernError codes, after the lease was returned. writeErrors are never
// reported and the reply is forwarded byte-identical.
func TestReplyCodesAfterForwardedReplies_FR35(t *testing.T) {
	cases := map[string]struct {
		sections []byte
		check    func(t *testing.T, f upstream.ReplyCodeInfo)
		none     bool
	}{
		"top-level 10107": {cat1(bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: 10107}, {Key: "codeName", Value: "NotWritablePrimary"}, {Key: "errmsg", Value: "x"}}), func(t *testing.T, f upstream.ReplyCodeInfo) {
			if !f.HasTop || !f.OKZero || f.Top != 10107 || f.TopName != "NotWritablePrimary" || f.HasWCE {
				t.Fatalf("info %+v", f)
			}
		}, false},
		"writeConcernError 91": {cat1(bson.D{{Key: "ok", Value: 1.0}, {Key: "n", Value: 1}, {Key: "writeConcernError", Value: bson.D{{Key: "code", Value: 91}, {Key: "errmsg", Value: "x"}}}}), func(t *testing.T, f upstream.ReplyCodeInfo) {
			if !f.HasWCE || f.WCE != 91 || f.OKZero || f.HasTop {
				t.Fatalf("info %+v", f)
			}
		}, false},
		"duplicate key 11000": {cat1(bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: 11000}, {Key: "codeName", Value: "DuplicateKey"}}), func(t *testing.T, f upstream.ReplyCodeInfo) {
			if !f.HasTop || f.Top != 11000 {
				t.Fatalf("info %+v", f)
			}
		}, false},
		"writeErrors only": {cat1(bson.D{{Key: "ok", Value: 1.0}, {Key: "n", Value: 0}, {Key: "writeErrors", Value: bson.A{bson.D{{Key: "index", Value: 0}, {Key: "code", Value: 10107}, {Key: "errmsg", Value: "e"}}}}}), nil, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			r.twin.AddRule(mongosim.Match{Command: "insert"}, mongosim.ReplyRaw(tc.sections))
			c := r.dial()
			m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 5, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "d"}}}))
			// The reply is identical.
			body := m.Raw[20:]
			if m.Flags&flagChecksum != 0 {
				body = body[:len(body)-4]
			}
			if string(body) != string(tc.sections) {
				t.Fatal("reply sections differ from the upstream's")
			}
			time.Sleep(100 * time.Millisecond)
			calls := r.codes.all()
			if tc.none {
				for _, k := range calls {
					if k.Src == upstream.Forwarded && (k.Info.HasTop || k.Info.HasWCE) {
						t.Fatalf("a code from writeErrors was reported: %+v", k.Info)
					}
				}
				return
			}
			var fwd []rcall
			for _, k := range calls {
				if k.Src == upstream.Forwarded {
					fwd = append(fwd, k)
				}
			}
			if len(fwd) != 1 || fwd[0].Host != r.twin.Addr() {
				t.Fatalf("Forwarded ReplyCodes calls: %+v", fwd)
			}
			if fwd[0].InUseAt != 0 {
				t.Fatalf("ReplyCodes ran with %d leases still held; the lease must be returned first", fwd[0].InUseAt)
			}
			tc.check(t, fwd[0].Info)
		})
	}
}
