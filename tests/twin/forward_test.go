//go:build twin

package twin

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-6 (FR-9, FR-10, FR-58): forwarded request sections and reply sections are
// byte-identical; the only header differences are requestID, responseTo,
// messageLength (checksum removal only, exactly 4), the checksum flag and
// exhaustAllowed.
func TestForwardedBytesIdentical_AC6(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	reply := append([]byte{0}, bsonOf(d("ok", float64(1), "n", int32(1), "knownReplyBytes", "synthetic-reply-bytes-5521"))...)
	tw.AddRule(mongosim.Match{Marker: "raw-reply"}, mongosim.ReplyRaw(reply))
	for _, cs := range []rawwire.ChecksumMode{rawwire.ChecksumNone, rawwire.ChecksumValid} {
		t.Run(fmt.Sprintf("checksum=%d", cs), func(t *testing.T) {
			tw.ResetRecording()
			conn := c.dial()
			sent := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 77, Checksum: cs,
				Body:      d("insert", "c", "$db", "d", "corraltestMarker", "raw-reply", "weird", d("a", int32(1), "b", "\x01\x02")),
				Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{d("_id", 1), d("_id", 2)}}}})
			var sentFlags uint32
			if cs != rawwire.ChecksumNone {
				sentFlags = 1
			}
			m := rt(t, conn, sent)
			got := tw.ReceivedCommand("insert")
			if len(got) != 1 {
				t.Fatalf("twin received %d inserts", len(got))
			}
			r := got[0]
			if !bytes.Equal(sections(r.Raw, r.Flags), sections(sent, sentFlags)) {
				t.Fatal("forwarded sections differ from what the client sent")
			}
			if diff := r.Flags &^ (mongosim.FlagChecksumPresent | mongosim.FlagExhaustAllowed); diff != 0 {
				t.Fatalf("unexpected flag bits %#x", diff)
			}
			if d := len(sent) - len(r.Raw); d != 0 && !(sentFlags == 1 && d == 4) {
				t.Fatalf("messageLength differs by %d", d)
			}
			if m.Header.ResponseTo != 77 {
				t.Fatalf("responseTo %d", m.Header.ResponseTo)
			}
			if !bytes.Equal(sections(m.Raw, m.Flags), reply) {
				t.Fatal("reply sections differ from what the twin sent")
			}
		})
	}
	// An upstream reply with a checksum (valid) reaches the client intact in
	// content, and the client's reply is a well-formed OP_MSG.
	tw.AddRule(mongosim.Match{Marker: "cksum-reply"}, mongosim.ReplyChecksum(true))
	m := rt(t, c.dial(), find(5, "cksum-reply"))
	if !isOK(m) || m.Header.ResponseTo != 5 {
		t.Fatalf("%v", doc(m))
	}
}

// AC-8 (FR-11, FR-12): 50 raw clients, all with requestID 1, through a pool of
// 2: each gets exactly its own reply with responseTo 1; an upstream reply with
// an unknown responseTo discards the upstream connection and closes the client
// connection.
func TestCorrelation_AC8(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.max_size": "2"}})
	c.waitReady()
	tw.AddRule(mongosim.Match{Command: "find"}, mongosim.Delay(20*time.Millisecond))
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("client-%02d", i)
			conn := rawwire.Dial(t, c.proxy)
			conn.Send(find(1, marker))
			m, err := conn.ReadMessage(20 * time.Second)
			if err != nil {
				errs <- fmt.Sprintf("%s: %v", marker, err)
				return
			}
			if m.Header.ResponseTo != 1 || strField(doc(m), "corraltestEcho") != marker {
				errs <- fmt.Sprintf("%s: responseTo %d echo %q", marker, m.Header.ResponseTo, strField(doc(m), "corraltestEcho"))
			}
			if _, err := conn.ReadMessage(100 * time.Millisecond); err == nil {
				errs <- marker + ": a second reply"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := tw.OpenConns("corral"); n > 2 {
		t.Fatalf("%d pool connections open, want <= 2", n)
	}
	// Unknown responseTo.
	tw.ClearRules()
	tw.AddRule(mongosim.Match{Marker: "bad-resp", Once: true}, mongosim.ReplyUnknownResponseTo())
	base := c.metric("corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response")
	conn := c.dial()
	conn.Send(find(1, "bad-resp"))
	closedNoBytes(t, conn)
	c.waitDelta(base, 1, "corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response")
	eventually(t, "the upstream connection discarded", func() bool {
		return c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "error") >= 1
	})
}

// AC-15 (FR-30): a client getMore with exhaustAllowed reaches the twin with the
// flag cleared, and the client receives one reply without moreToCome.
func TestExhaustAllowedCleared_AC15(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	m := rt(t, c.dial(), rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Flags: mongosim.FlagExhaustAllowed,
		Body: d("getMore", int64(1), "collection", "c", "$db", "d")}))
	got := tw.ReceivedCommand("getMore")
	if len(got) != 1 || got[0].Flags&mongosim.FlagExhaustAllowed != 0 {
		t.Fatalf("twin received %d getMores, flags %#x", len(got), flagsOf(got))
	}
	if m.Flags&mongosim.FlagMoreToCome != 0 {
		t.Fatal("the reply carries moreToCome")
	}
}

func flagsOf(ms []mongosim.Message) uint32 {
	if len(ms) == 0 {
		return 0
	}
	return ms[0].Flags
}

// AC-36 (FR-59): a hello with moreToCome gets no reply and the connection then
// completes a ping.
func TestMoreToComeHelloNoReply_AC36(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: mongosim.FlagMoreToCome, Body: d("hello", 1, "$db", "admin")}))
	if _, err := conn.ReadMessage(500 * time.Millisecond); err == nil {
		t.Fatal("a moreToCome hello got a reply")
	}
	if m := rt(t, conn, ping(2)); !isOK(m) || m.Header.ResponseTo != 2 {
		t.Fatalf("ping after the dropped hello: %v", doc(m))
	}
}
