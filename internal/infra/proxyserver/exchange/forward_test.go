package exchange_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func u32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

const (
	flagChecksum = 1 << 0
	flagMore     = 1 << 1
	flagExhaust  = 1 << 16
)

func find(coll string, extra ...bson.E) bson.D {
	d := bson.D{{Key: "find", Value: coll}, {Key: "$db", Value: "shop"}}
	return append(d, extra...)
}

// expectedForwarded builds what the upstream must receive for a client
// OP_MSG: the same sections byte for byte, checksum stripped, exhaustAllowed
// cleared, the length adjusted. The requestID (bytes 4..8) is the engine's
// and is masked by the comparison.
func expectedForwarded(orig []byte) []byte {
	flags := u32(orig[16:])
	body := orig[20:]
	if flags&flagChecksum != 0 {
		body = body[:len(body)-4]
	}
	flags &^= flagChecksum | flagExhaust
	out := make([]byte, 0, len(orig))
	out = binary.LittleEndian.AppendUint32(out, uint32(20+len(body)))
	out = append(out, orig[4:16]...)
	out = binary.LittleEndian.AppendUint32(out, flags)
	return append(out, body...)
}

func sameModuloRequestID(a, b []byte) bool {
	if len(a) != len(b) || len(a) < 16 {
		return false
	}
	x, y := append([]byte(nil), a...), append([]byte(nil), b...)
	copy(x[4:8], []byte{0, 0, 0, 0})
	copy(y[4:8], []byte{0, 0, 0, 0})
	return bytes.Equal(x, y)
}

func lastReceived(t *testing.T, r *rig, cmd string) mongosim.Message {
	t.Helper()
	var ms []mongosim.Message
	eventually(t, "twin to receive "+cmd, func() bool { ms = r.twin.ReceivedCommand(cmd); return len(ms) > 0 })
	return ms[len(ms)-1]
}

// AC-6, FR-10, FR-30: forwarding is byte-faithful apart from the header IDs,
// the checksum, and exhaustAllowed. $readPreference is untouched (FR-36).
func TestForwardingByteFaithful_AC6(t *testing.T) {
	big := strings.Repeat("v", 150_000)
	cases := map[string][]byte{
		"plain find":           rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 11, Body: find("c", bson.E{Key: "filter", Value: bson.D{{Key: "a", Value: 1}}})}),
		"exhaustAllowed":       rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 12, Flags: flagExhaust | 1<<17, Body: find("c")}),
		"valid checksum":       rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 13, Checksum: rawwire.ChecksumValid, Body: find("c")}),
		"checksum+exhaust":     rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 14, Flags: flagExhaust | 1<<17, Checksum: rawwire.ChecksumValid, Body: find("c")}),
		"readPreference":       rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 15, Body: find("c", bson.E{Key: "$readPreference", Value: bson.D{{Key: "mode", Value: "secondary"}}})}),
		"kind-1 sequence":      rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 16, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "shop"}}, Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "_id", Value: 1}}, {{Key: "_id", Value: 2}}}}}}),
		"kind-1 first":         rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 17, SequencesFirst: true, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "shop"}}, Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "_id", Value: 1}}}}}}),
		"large body, checksum": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 18, Checksum: rawwire.ChecksumValid, Body: find("c", bson.E{Key: "comment", Value: big})}),
		"large kind-1":         rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 19, Body: bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "shop"}}, Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "blob", Value: big}}}}}}),
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			c := r.dial()
			m := roundTrip(t, c, msg)
			if m.Header.ResponseTo != int32(u32(msg[4:])) {
				t.Fatalf("responseTo = %d, want the client's requestID %d", m.Header.ResponseTo, u32(msg[4:]))
			}
			cmd := "find"
			if strings.Contains(name, "kind-1") {
				cmd = "insert"
			}
			got := lastReceived(t, r, cmd)
			want := expectedForwarded(msg)
			if !sameModuloRequestID(got.Raw, want) {
				t.Fatalf("forwarded bytes differ from the client's message (got %d bytes, want %d)", len(got.Raw), len(want))
			}
			if got.ResponseTo != 0 {
				t.Fatalf("responseTo upstream = %d, want 0", got.ResponseTo)
			}
			if got.Flags&flagChecksum != 0 || got.Flags&flagExhaust != 0 {
				t.Fatalf("forwarded flags %#x still carry checksum or exhaustAllowed", got.Flags)
			}
			if strings.Contains(name, "exhaust") && got.Flags&(1<<17) == 0 {
				t.Fatal("an unrelated optional flag bit was lost")
			}
			if m.Flags&flagMore != 0 {
				t.Fatal("a reply with moreToCome was sent to the client (FR-30)")
			}
		})
	}
}

// FR-10, FR-11: the reply's sections are forwarded byte for byte, with the
// client's requestID as responseTo, a fresh requestID, and the checksum
// removed or valid. The per-client reply requestIDs are unique.
func TestReplyByteFaithful_AC6(t *testing.T) {
	sections := cat1(bson.D{{Key: "ok", Value: 1.0}, {Key: "n", Value: 3}, {Key: "marker", Value: "reply-body"}, {Key: "writeErrors", Value: bson.A{bson.D{{Key: "index", Value: 0}, {Key: "code", Value: 10107}}}}})
	r := newRig(t, opts{})
	r.twin.AddRule(mongosim.Match{Command: "find"}, mongosim.ReplyRaw(sections))
	c := r.dial()
	seen := map[int32]bool{}
	for i := int32(1); i <= 3; i++ {
		m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 100 + i, Body: find("c")}))
		if m.Header.ResponseTo != 100+i {
			t.Fatalf("responseTo = %d", m.Header.ResponseTo)
		}
		if seen[m.Header.RequestID] {
			t.Fatalf("reply requestID %d repeated on one connection", m.Header.RequestID)
		}
		seen[m.Header.RequestID] = true
		if m.Flags&flagChecksum != 0 {
			want := crc32.Checksum(m.Raw[:len(m.Raw)-4], crc32.MakeTable(crc32.Castagnoli))
			if u32(m.Raw[len(m.Raw)-4:]) != want {
				t.Fatal("a reply carries a wrong checksum")
			}
			if !bytes.Equal(m.Raw[20:len(m.Raw)-4], sections) {
				t.Fatal("reply sections differ")
			}
		} else if !bytes.Equal(m.Raw[20:], sections) {
			t.Fatal("reply sections differ from the upstream's")
		}
	}
	// A checksummed upstream reply.
	r2 := newRig(t, opts{})
	r2.twin.AddRule(mongosim.Match{Command: "find"}, mongosim.ReplyChecksum(true))
	m := roundTrip(t, r2.dial(), rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 7, Body: find("c")}))
	if m.Header.ResponseTo != 7 {
		t.Fatalf("responseTo %d", m.Header.ResponseTo)
	}
}

func cat1(d bson.D) []byte {
	b, _ := bson.Marshal(d)
	return append([]byte{0}, b...)
}

// AC-8: 50 concurrent clients sending the same requestID through a pool of two
// connections each get their own reply.
func TestConcurrentClientsSameRequestID_AC8(t *testing.T) {
	r := newRig(t, opts{poolMax: 2})
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := r.dial()
			marker := fmt.Sprintf("client-%d", i)
			m := roundTrip(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: find("c", bson.E{Key: "corraltestMarker", Value: marker})}))
			if m.Header.ResponseTo != 1 {
				errs <- fmt.Sprintf("client %d: responseTo %d", i, m.Header.ResponseTo)
				return
			}
			if v, err := m.Docs[0].LookupErr("corraltestEcho"); err != nil || v.StringValue() != marker {
				errs <- fmt.Sprintf("client %d got another client's reply", i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if r.twin.ConnAttemptsBy("corral") > 2 {
		t.Fatalf("pool opened %d connections, max 2", r.twin.ConnAttemptsBy("corral"))
	}
}

// AC-8, FR-11, FR-58: an upstream reply that does not match: the upstream
// connection is discarded with reason error, the client connection closes with
// no reply, and the request is a proxy_error.
func TestUpstreamProtocolErrors_AC8(t *testing.T) {
	cases := map[string]struct {
		action mongosim.Action
		detail string
	}{
		"unknown responseTo": {mongosim.ReplyUnknownResponseTo(), "unexpected_response"},
		"moreToCome reply":   {mongosim.ReplyMoreToCome(), "unexpected_response"},
		"oversize":           {mongosim.ReplyOversize(), "message_too_large"},
		"bad checksum":       {mongosim.ReplyChecksum(false), "checksum_mismatch"},
		"malformed":          {mongosim.ReplyMalformed(), "malformed_message"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts{})
			r.twin.AddRule(mongosim.Match{Command: "find"}, tc.action)
			c := r.dial()
			if err := c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Body: find("c")})); err != nil {
				t.Fatal(err)
			}
			if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
				t.Fatalf("client not closed without bytes: %v", err)
			}
			if reason := r.waitServed(); reason != proxy.CloseUpstreamFailure {
				t.Fatalf("close reason %q, want upstream_failure", reason)
			}
			res := r.waitResults(1)
			if res[0].Command != "find" || res[0].Outcome != proxy.OutcomeProxyError || detailOf(res[0].Err) != tc.detail {
				t.Fatalf("result %+v (detail %q), want find/proxy_error/%s", res[0], detailOf(res[0].Err), tc.detail)
			}
			eventually(t, "upstream connection discarded", func() bool { return contains(r.pobs.reasons(), "error") })
		})
	}
}

// AC-20, FR-40: the upstream resets after the request was sent.
func TestResetAfterRequestSent_AC20(t *testing.T) {
	r := newRig(t, opts{})
	r.twin.AddRule(mongosim.Match{Command: "find"}, mongosim.ResetAfterReceive())
	c := r.dial()
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Body: find("c")}))
	if err := c.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("client not closed without bytes: %v", err)
	}
	if reason := r.waitServed(); reason != proxy.CloseUpstreamFailure {
		t.Fatalf("close reason %q", reason)
	}
	res := r.waitResults(1)
	if res[0].Outcome != proxy.OutcomeProxyError || classOf(res[0].Err) != "network" {
		t.Fatalf("result %+v (class %q)", res[0], classOf(res[0].Err))
	}
	eventually(t, "upstream discarded", func() bool { return contains(r.pobs.reasons(), "error") })
}

// FR-12: a client has at most one exchange at a time, and messages are handled
// in arrival order: two pipelined requests are answered in order.
func TestPipelinedRequestsInOrder_FR12(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	var raw []byte
	for i := int32(1); i <= 5; i++ {
		raw = append(raw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: i, Body: find("c", bson.E{Key: "corraltestMarker", Value: fmt.Sprintf("m%d", i)})})...)
	}
	if err := c.Send(raw); err != nil {
		t.Fatal(err)
	}
	for i := int32(1); i <= 5; i++ {
		m, err := c.ReadMessage(10 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if m.Header.ResponseTo != i {
			t.Fatalf("reply %d has responseTo %d", i, m.Header.ResponseTo)
		}
	}
	if got := r.twin.ConnAttemptsBy("corral"); got != 1 {
		t.Fatalf("pipelined requests used %d upstream connections, want reuse of 1", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func detailOf(err error) string {
	var d interface{ CorralErrorDetail() string }
	if asErr(err, &d) {
		return d.CorralErrorDetail()
	}
	return ""
}

var _ = exchange.ReplyChunk
