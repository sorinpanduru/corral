//go:build twin

package twin

import (
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func rawDoc(elems ...[]byte) []byte {
	n := 5
	for _, e := range elems {
		n += len(e)
	}
	out := []byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	for _, e := range elems {
		out = append(out, e...)
	}
	return append(out, 0)
}

func strEl(key, val string) []byte {
	b := []byte{0x02}
	b = append(b, key...)
	b = append(b, 0)
	n := len(val) + 1
	b = append(b, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	b = append(b, val...)
	return append(b, 0)
}

func int32El(key string, v int32) []byte {
	b := []byte{0x10}
	b = append(b, key...)
	b = append(b, 0)
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// refused asserts a message closes the connection with detail, and that the
// twin received nothing carrying marker.
func refused(t *testing.T, c *corral, tw *mongosim.Server, raw []byte, detail, marker string) {
	t.Helper()
	c = c.with(t)
	before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", detail)
	n0 := len(userMessages(tw))
	conn := c.dial()
	if err := conn.Send(raw); err != nil {
		t.Fatal(err)
	}
	closedNoBytes(t, conn)
	c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", detail)
	if n := len(userMessages(tw)); n != n0 {
		t.Fatalf("a refused message reached the twin (%d new messages)", n-n0)
	}
	_ = marker
}

// AC-44 (FR-60, FR-33): messages smaller than L that are fully checked before a
// lease: each is refused with malformed_message and the twin sees nothing.
func TestInspectedFieldBypassesRefused_AC44(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	t.Run("pipeline only in a kind-1 section", func(t *testing.T) {
		refused(t, c, tw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1,
			Body:      d("aggregate", "c", "$db", "d", "cursor", d(), "corraltestMarker", "m-k1pipe"),
			Sequences: []rawwire.Sequence{{ID: "pipeline", Docs: []bson.D{d("$currentOp", d())}}}}), "malformed_message", "m-k1pipe")
	})
	t.Run("two pipeline fields", func(t *testing.T) {
		refused(t, c, tw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("aggregate", "c", "$db", "d", "corraltestMarker", "m-dup",
			"pipeline", bson.A{d("$match", d())}, "cursor", d(), "pipeline", bson.A{d("$currentOp", d())})}), "malformed_message", "m-dup")
	})
	t.Run("two $db fields", func(t *testing.T) {
		refused(t, c, tw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: d("find", "c", "$db", "d", "$db", "e", "corraltestMarker", "m-db")}), "malformed_message", "m-db")
	})
	t.Run("kind-1 inner length overruns", func(t *testing.T) {
		bad := []byte{0xff, 0xff, 0, 0, 0x10, 'x', 0, 1, 0, 0, 0, 0} // inner length 65535 over a 12-byte document
		refused(t, c, tw, rawOpMsg(1, 0, kind0(bsonOf(d("insert", "c", "$db", "d", "corraltestMarker", "m-k1len"))), kind1("documents", bad)), "malformed_message", "m-k1len")
	})
	t.Run("nested 1025 levels", func(t *testing.T) {
		refused(t, c, tw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, BodyRaw: rawwire.NestedBody(1025, true)}), "malformed_message", "")
	})
	t.Run("nested 1024 levels is forwarded", func(t *testing.T) {
		before := len(tw.Received())
		conn := c.dial()
		conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, BodyRaw: rawwire.NestedBody(1024, true)}))
		conn.ReadMessage(5 * time.Second) // whatever the server answers
		if len(tw.Received()) <= before {
			t.Fatal("a body nested exactly 1024 levels did not reach the twin")
		}
	})
	t.Run("forwarded unchanged", func(t *testing.T) {
		cases := map[string][]byte{
			"insert with a kind-1 documents section": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1,
				Body:      d("insert", "c", "$db", "d", "corraltestMarker", "f-ins"),
				Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{d("_id", 1)}}}}),
			"non-UTF-8 string in an uninspected field": rawOpMsg(2, 0, kind0(rawDoc(strEl("find", "c"), strEl("$db", "d"), strEl("corraltestMarker", "f-utf8"), strEl("comment", "\xff\xfe")))),
			"duplicate keys in an uninspected field": rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3,
				Body: d("find", "c", "$db", "d", "corraltestMarker", "f-dupkey", "filter", d("a", 1, "a", 2), "foo", 1, "foo", 2)}),
		}
		for name, raw := range cases {
			conn := c.dial()
			m := rt(t, conn, raw)
			if !isOK(m) {
				t.Errorf("%s: %v", name, doc(m))
			}
		}
		seen := map[string]bool{}
		for _, m := range tw.Received() {
			seen[m.Marker] = true
		}
		for _, k := range []string{"f-ins", "f-utf8", "f-dupkey"} {
			if !seen[k] {
				t.Errorf("%s never reached the twin", k)
			}
		}
	})
}

// AC-55 (FR-60): kind-1 nesting counts from the document itself; a duplicated
// command name is refused; a kind-1 section named like the command is not.
func TestKind1AndCommandNameRules_AC55(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	head := func(marker string) []byte {
		return kind0(bsonOf(d("insert", "c", "$db", "d", "corraltestMarker", marker)))
	}
	t.Run("kind-1 document nested 1025", func(t *testing.T) {
		refused(t, c, tw, rawOpMsg(1, 0, head("n1025"), kind1("documents", rawwire.NestedBody(1025, false))), "malformed_message", "n1025")
	})
	t.Run("kind-1 document nested 1024 forwarded", func(t *testing.T) {
		conn := c.dial()
		conn.Send(rawOpMsg(2, 0, head("n1024"), kind1("documents", rawwire.NestedBody(1024, false))))
		conn.ReadMessage(5 * time.Second)
		found := false
		for _, m := range tw.Received() {
			found = found || m.Marker == "n1024"
		}
		if !found {
			t.Fatal("a kind-1 document nested exactly 1024 levels was not forwarded")
		}
	})
	t.Run("duplicate command name", func(t *testing.T) {
		refused(t, c, tw, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Body: d("find", "a", "filter", d(), "find", "b", "$db", "d", "corraltestMarker", "dupcmd")}), "malformed_message", "dupcmd")
	})
	t.Run("kind-1 named like the command", func(t *testing.T) {
		conn := c.dial()
		m := rt(t, conn, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 4,
			Body:      d("find", "c", "$db", "d", "corraltestMarker", "k1named"),
			Sequences: []rawwire.Sequence{{ID: "find", Docs: []bson.D{d("x", 1)}}}}))
		if !isOK(m) {
			t.Fatalf("reply %v", doc(m))
		}
	})
}

func bigAggregate(id int32, size int, pre, post bson.A, cs rawwire.ChecksumMode, marker string) []byte {
	body := d("aggregate", "c", "$db", "d", "corraltestMarker", marker, "pipeline", pre, "cursor", d(), "filler", strings.Repeat("a", size))
	if post != nil {
		body = append(body, bson.E{Key: "pipeline", Value: post})
	}
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Checksum: cs, Body: body})
}

// AC-49 (FR-38, FR-60, FR-48): with a pool connection free, a 5 MB aggregate
// whose second pipeline field (a $currentOp) sits past L: the twin receives
// some bytes and the connection is then closed, never a complete message; a
// wrong checksum on a correct body: the twin never receives the last 4 bytes.
func TestLateValidationNeverCompletesUpstream_AC49(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	m := rt(t, c.dial(), find(1, "warm")) // opens a pool connection
	_ = m
	run := func(name string, raw []byte, detail string) {
		t.Run(name, func(t *testing.T) {
			before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", detail)
			reqs := c.metric("corral_requests_total", "command", "aggregate", "outcome", "proxy_error")
			conn := c.dial()
			go conn.Send(raw)
			closed := false
			if _, err := conn.ReadMessage(15 * time.Second); err != nil {
				closed = true
			}
			if !closed {
				t.Fatal("the client got a reply")
			}
			c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", detail)
			c.waitDelta(reqs, 1, "corral_requests_total", "command", "aggregate", "outcome", "proxy_error")
			for _, x := range tw.ReceivedCommand("aggregate") {
				if x.Marker == "late-"+name {
					t.Fatal("the twin received a complete message")
				}
			}
			eventually(t, "a partial upstream frame and a closed connection", func() bool {
				for _, ci := range tw.Conns() {
					if ci.AppName == "corral" && !ci.Open && ci.PartialBytes > 0 && ci.PartialBytes < len(raw) {
						if name == "checksum" && int(ci.PartialBytes) > len(raw)-4 {
							return false
						}
						return true
					}
				}
				return false
			})
		})
	}
	run("pipeline", bigAggregate(2, 5<<20, bson.A{d("$match", d())}, bson.A{d("$currentOp", d())}, rawwire.ChecksumNone, "late-pipeline"), "malformed_message")
	run("checksum", bigAggregate(3, 5<<20, bson.A{d("$match", d())}, nil, rawwire.ChecksumInvalid, "late-checksum"), "checksum_mismatch")
	// An HTTP GET counts as one request with command "other".
	before := c.metric("corral_requests_total", "command", "other", "outcome", "proxy_error")
	conn := c.dial()
	conn.Send(rawwire.HTTPGet())
	closedNoBytes(t, conn)
	c.waitDelta(before, 1, "corral_requests_total", "command", "other", "outcome", "proxy_error")
}
