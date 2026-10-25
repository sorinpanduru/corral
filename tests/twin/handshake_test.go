//go:build twin

package twin

import (
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func clientMeta() bson.D {
	return d("application", d("name", "twin-test-app"), "driver", d("name", "twin-driver", "version", "1.0"), "os", d("type", "Linux"))
}

func isMasterQuery(id int32, wrapped bool, extra ...any) []byte {
	q := d(append([]any{"isMaster", 1}, extra...)...)
	var query any = q
	if wrapped {
		query = d("$query", q, "$readPreference", d("mode", "primaryPreferred"))
	}
	return rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: id, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: query})
}

func keys(r bson.Raw) []string {
	els, _ := r.Elements()
	var out []string
	for _, e := range els {
		out = append(out, e.Key())
	}
	sort.Strings(out)
	return out
}

// AC-5 (FR-8, FR-19): OP_QUERY handshakes and refusals.
func TestOpQueryHandshake_AC5(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	for _, wrapped := range []bool{false, true} {
		m := rt(t, conn, isMasterQuery(1, wrapped))
		if m.Header.OpCode != rawwire.OpCodeReply || m.Flags != 0 {
			t.Fatalf("wrapped=%v: opcode %d responseFlags %d", wrapped, m.Header.OpCode, m.Flags)
		}
		if strField(doc(m), "msg") != "isdbgrid" || !isOK(m) {
			t.Fatalf("wrapped=%v: not the handshake document: %v", wrapped, doc(m))
		}
	}
	// A non-handshake OP_QUERY: QueryFailure, $err, code 352; then OP_MSG works.
	m := rt(t, c.dial(), rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 2, FullCollection: "d.c", NumberToReturn: 1, Query: d("find", "c")}))
	if m.Header.OpCode != rawwire.OpCodeReply || m.Flags&2 == 0 {
		t.Fatalf("352 reply: opcode %d flags %d", m.Header.OpCode, m.Flags)
	}
	if code(m) != 352 || strField(doc(m), "$err") == "" {
		t.Fatalf("352 reply doc: %v", doc(m))
	}
	same := c.dial()
	if m := rt(t, same, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 3, FullCollection: "d.c", NumberToReturn: 1, Query: d("find", "c")})); code(m) != 352 {
		t.Fatalf("second refusal: %v", doc(m))
	}
	if m := rt(t, same, ping(4)); !isOK(m) {
		t.Fatalf("ping after the refusal: %v", doc(m))
	}
	// A second OP_QUERY isMaster with client gets 186.
	conn2 := c.dial()
	if m := rt(t, conn2, isMasterQuery(5, false, "client", clientMeta())); code(m) != 0 || !isOK(m) {
		t.Fatalf("first handshake with client: %v", doc(m))
	}
	m = rt(t, conn2, isMasterQuery(6, false, "client", clientMeta()))
	if m.Flags != 0 || code(m) != 186 {
		t.Fatalf("second handshake with client: flags %d %v", m.Flags, doc(m))
	}
	for _, x := range userMessages(tw) {
		if x.Command == "find" && x.OpCode == rawwire.OpCodeQuery {
			t.Fatal("the refused OP_QUERY find reached the twin")
		}
	}
}

// AC-5: with the upstream never reached, an OP_QUERY isMaster gets {ok:0, code:6}.
func TestOpQueryHandshakeNeverReached_AC5(t *testing.T) {
	c := start(t, opts{})
	conn := c.dial()
	m := rt(t, conn, isMasterQuery(1, false))
	if m.Flags != 0 || isOK(m) || code(m) != 6 {
		t.Fatalf("flags %d %v", m.Flags, doc(m))
	}
}

// AC-9 (FR-13 to FR-17, FR-20, FR-21): local handshake.
func TestHandshakeLocal_AC9(t *testing.T) {
	c, tw := startMongos(t, opts{}, func(h *mongosim.HelloSpec) {
		h.MinWire, h.MaxWire = 6, 27
		h.MaxWriteBatchSize, h.MaxMessageSizeBytes, h.LogicalSessionTimeoutMinutes = 54321, 40_000_000, 17
	})
	c.waitReady()
	conn := c.dial()
	for _, name := range []string{"hello", "isMaster", "ismaster"} {
		m := rt(t, conn, mk(1, "hs-"+name, name, 1, "$db", "admin", "helloOk", true))
		r := doc(m)
		if !isOK(m) || strField(r, "msg") != "isdbgrid" {
			t.Fatalf("%s: %v", name, r)
		}
		want := []string{"helloOk", "localTime", "logicalSessionTimeoutMinutes", "maxBsonObjectSize", "maxMessageSizeBytes",
			"maxWireVersion", "maxWriteBatchSize", "minWireVersion", "msg", "ok", "readOnly", "connectionId"}
		if name == "hello" {
			want = append(want, "isWritablePrimary")
		} else {
			want = append(want, "ismaster")
		}
		sort.Strings(want)
		if got := keys(r); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: fields %v, want %v", name, got, want)
		}
		if v, _ := intField(r, "maxWireVersion"); v != 25 {
			t.Fatalf("maxWireVersion %d, want 25 (capped)", v)
		}
		if v, _ := intField(r, "minWireVersion"); v != 6 {
			t.Fatalf("minWireVersion %d, want 6 (unchanged)", v)
		}
		if v, _ := intField(r, "maxWriteBatchSize"); v != 54321 {
			t.Fatalf("maxWriteBatchSize %d", v)
		}
		if v, _ := intField(r, "maxMessageSizeBytes"); v != 40_000_000 {
			t.Fatalf("maxMessageSizeBytes %d", v)
		}
		if v, _ := intField(r, "logicalSessionTimeoutMinutes"); v != 17 {
			t.Fatalf("logicalSessionTimeoutMinutes %d", v)
		}
		if id, _ := intField(r, "connectionId"); id <= 0 {
			t.Fatalf("connectionId %d", id)
		}
	}
	// No handshake reached the twin from a client.
	for _, m := range tw.Received() {
		if strings.HasPrefix(m.Marker, "hs-") {
			t.Fatalf("a client handshake reached the twin: %s", m.Command)
		}
	}
	if tw.ConnAttemptsBy("corral") != 0 {
		t.Fatal("a pool connection was opened for handshakes")
	}
	// helloOk only when requested.
	m := rt(t, conn, cmd(2, "hello", 1, "$db", "admin"))
	if _, err := doc(m).LookupErr("helloOk"); err == nil {
		t.Fatal("helloOk present when not requested")
	}
}

// AC-9 (FR-15): topologyVersion with maxAwaitTimeMS is answered at once.
func TestHelloWithTopologyVersionAnsweredAtOnce_AC9(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	begin := time.Now()
	m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin",
		"topologyVersion", d("processId", bson.NewObjectID(), "counter", int64(1)), "maxAwaitTimeMS", int64(10000)))
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("answered after %v", d)
	}
	if _, err := doc(m).LookupErr("topologyVersion"); err == nil {
		t.Fatal("topologyVersion in the reply")
	}
}

// AC-9 (FR-16, FR-17, FR-20, FR-21): 186, 72, and no compression or
// authentication fields.
func TestHandshakeRefusalsAndStrippedFields_AC9(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin", "client", clientMeta(),
		"compression", bson.A{"zstd"}, "saslSupportedMechs", "twin-user-marker-Wq83Nd.admin",
		"speculativeAuthenticate", d("saslStart", 1, "mechanism", "SCRAM-SHA-256", "db", "admin")))
	r := doc(m)
	if !isOK(m) {
		t.Fatalf("%v", r)
	}
	for _, k := range []string{"compression", "speculativeAuthenticate", "saslSupportedMechs"} {
		if _, err := r.LookupErr(k); err == nil {
			t.Fatalf("reply carries %s", k)
		}
	}
	if m := rt(t, conn, cmd(2, "hello", 1, "$db", "admin", "client", clientMeta())); code(m) != 186 {
		t.Fatalf("second client: %v", doc(m))
	}
	// 72 on a fresh connection (loadBalanced).
	lb := c.dial()
	if m := rt(t, lb, cmd(1, "hello", 1, "$db", "admin", "loadBalanced", true)); code(m) != 72 {
		t.Fatalf("loadBalanced: %v", doc(m))
	}
	// The connection stays open.
	if m := rt(t, lb, ping(2)); !isOK(m) {
		t.Fatalf("connection after 72: %v", doc(m))
	}
}
