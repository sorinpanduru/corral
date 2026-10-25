//go:build twin

package twin

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-17 (FR-33, FR-48, FR-49): blocked commands get 13, never reach the twin,
// increment the counter and log a rate-limited record; allowed ones pass.
func TestBlockedCommands_AC17(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	blocked := []struct {
		name string
		body []any
	}{
		{"currentOp", []any{"currentOp", 1, "$db", "admin"}},
		{"killOp", []any{"killOp", 1, "op", 1, "$db", "admin"}},
		{"killAllSessions", []any{"killAllSessions", bson.A{}, "$db", "admin"}},
		{"killAllSessionsByPattern", []any{"killAllSessionsByPattern", bson.A{}, "$db", "admin"}},
		{"aggregate $currentOp", []any{"aggregate", 1, "pipeline", bson.A{d("$currentOp", d())}, "cursor", d(), "$db", "admin"}},
		{"killSessions empty array", []any{"killSessions", bson.A{}, "$db", "admin"}},
		{"killSessions scalar", []any{"killSessions", 1, "$db", "admin"}},
		{"killSessions document", []any{"killSessions", d(), "$db", "admin"}},
	}
	for i, b := range blocked {
		before := c.metric("corral_commands_blocked_total")
		m := rt(t, conn, cmd(int32(i+1), append(b.body, "corraltestMarker", "blk")...))
		if isOK(m) || code(m) != 13 || strField(doc(m), "codeName") != "Unauthorized" {
			t.Fatalf("%s: %v", b.name, doc(m))
		}
		c.waitDelta(before, 1, "corral_commands_blocked_total")
	}
	if n := len(tw.Received()); n > 0 {
		for _, m := range tw.Received() {
			if m.Marker == "blk" {
				t.Fatalf("a blocked command reached the twin: %s", m.Command)
			}
		}
	}
	c.waitRecords("client command blocked", 1) // rate-limited: at least one
	// Allowed.
	m := rt(t, conn, mk(50, "ok-agg", "aggregate", "c", "pipeline", bson.A{d("$match", d())}, "cursor", d(), "$db", "d"))
	if !isOK(m) {
		t.Fatalf("aggregate $match: %v", doc(m))
	}
	m = rt(t, conn, mk(51, "ok-cs", "connectionStatus", 1, "$db", "admin"))
	if !isOK(m) {
		t.Fatalf("connectionStatus: %v", doc(m))
	}
	got := 0
	for _, x := range tw.Received() {
		if x.Marker == "ok-agg" || x.Marker == "ok-cs" {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("%d of the 2 allowed commands reached the twin", got)
	}
}

// AC-58 (FR-33, FR-9, FR-61): killSessions with explicit session ids is
// forwarded unchanged; over OP_QUERY it gets 352; with moreToCome it is dropped
// and counted; an argument after L is malformed_message.
func TestKillSessionsForms_AC58(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	sid := bson.Binary{Subtype: 4, Data: []byte("0123456789abcdef")}
	m := rt(t, conn, mk(1, "ks-fwd", "killSessions", bson.A{d("id", sid)}, "$db", "admin"))
	if !isOK(m) {
		t.Fatalf("explicit ids: %v", doc(m))
	}
	got := 0
	for _, x := range tw.Received() {
		if x.Marker == "ks-fwd" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d killSessions with ids reached the twin", got)
	}
	// OP_QUERY.
	qm := rt(t, c.dial(), rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 2, FullCollection: "admin.$cmd", NumberToReturn: 1, Query: d("killSessions", bson.A{})}))
	if code(qm) != 352 {
		t.Fatalf("OP_QUERY killSessions: %v", doc(qm))
	}
	// moreToCome.
	drop := c.metric("corral_requests_dropped_total")
	mc := c.dial()
	mc.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 3, Flags: mongosim.FlagMoreToCome, Body: d("killSessions", bson.A{}, "$db", "admin", "corraltestMarker", "ks-mtc")}))
	if _, err := mc.ReadMessage(500 * time.Millisecond); err == nil {
		t.Fatal("a moreToCome killSessions got a reply")
	}
	c.waitDelta(drop, 1, "corral_requests_dropped_total")
	for _, x := range tw.Received() {
		if x.Marker == "ks-mtc" {
			t.Fatal("the dropped message was forwarded")
		}
	}
	// A killSessions field that starts after L (here a second, empty one after
	// a 70 KB filler) is a deciding field that does not fit (FR-38): the
	// message is refused as malformed_message and nothing is forwarded.
	before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
	lc := c.dial()
	big := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 4, Body: d("killSessions", bson.A{d("id", sid)}, "$db", "admin",
		"corraltestMarker", "ks-late", "filler", string(make([]byte, 70000)), "killSessions", bson.A{})})
	lc.Send(big)
	closedNoBytes(t, lc)
	c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
	for _, x := range tw.Received() {
		if x.Marker == "ks-late" {
			t.Fatal("the late-argument message was forwarded")
		}
	}
}
