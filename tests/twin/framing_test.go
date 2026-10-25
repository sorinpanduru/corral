//go:build twin

package twin

import (
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// SPEC-002 AC-4 (FR-5, FR-6, FR-7, FR-48) and SPEC-001 AC-34: each framing
// fault closes the connection with no reply and counts one client protocol
// error with the table's error_detail. The closed message counts as one
// proxy_error request with class protocol (FR-48 Requests closed by a client
// protocol error).
func TestFramingTable_AC4(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	validBody := bsonOf(d("ping", 1, "$db", "admin"))
	cases := []struct {
		name   string
		raw    []byte
		detail string
	}{
		{"length 15", rawwire.RawHeader(15, 1, 0, rawwire.OpCodeMsg, nil), "malformed_message"},
		{"negative length", rawwire.RawHeader(-1, 1, 0, rawwire.OpCodeMsg, nil), "malformed_message"},
		{"OP_MSG length 25", rawwire.RawHeader(25, 1, 0, rawwire.OpCodeMsg, make([]byte, 9)), "malformed_message"},
		{"OP_QUERY length 33", rawwire.RawHeader(33, 1, 0, rawwire.OpCodeQuery, make([]byte, 17)), "malformed_message"},
		{"length above the maximum", rawwire.RawHeader(60_000_000, 1, 0, rawwire.OpCodeMsg, nil), "message_too_large"},
		{"HTTP GET", rawwire.HTTPGet(), "message_too_large"},
		{"OP_COMPRESSED", rawwire.RawHeader(20, 1, 0, 2012, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_REPLY", rawwire.RawHeader(20, 1, 0, 1, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_UPDATE", rawwire.RawHeader(20, 1, 0, 2001, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_INSERT", rawwire.RawHeader(20, 1, 0, 2002, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_GET_MORE", rawwire.RawHeader(20, 1, 0, 2005, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_DELETE", rawwire.RawHeader(20, 1, 0, 2006, make([]byte, 4)), "unsupported_opcode"},
		{"legacy OP_KILL_CURSORS", rawwire.RawHeader(20, 1, 0, 2007, make([]byte, 4)), "unsupported_opcode"},
		{"unknown opcode", rawwire.RawHeader(20, 1, 0, 9999, make([]byte, 4)), "unsupported_opcode"},
		{"unknown opcode, length 16", rawwire.RawHeader(16, 1, 0, 9999, nil), "unsupported_opcode"},
		{"unknown required flag bit", rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: 1 << 2, Body: d("ping", 1, "$db", "admin")}), "malformed_message"},
		{"bad checksum", rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Checksum: rawwire.ChecksumInvalid, Body: d("ping", 1, "$db", "admin")}), "checksum_mismatch"},
		{"two kind-0 sections", rawOpMsg(1, 0, kind0(validBody), kind0(validBody)), "malformed_message"},
		{"unknown section kind", rawOpMsg(1, 0, append([]byte{5}, validBody...)), "malformed_message"},
		{"truncated section", rawOpMsg(1, 0, kind0(validBody[:len(validBody)-3])), "malformed_message"},
		{"invalid BSON body", rawOpMsg(1, 0, kind0([]byte{5, 0, 0, 0, 1})), "malformed_message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := c.with(t)
			proto := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", tc.detail)
			reqs := c.metric("corral_requests_total", "outcome", "proxy_error")
			errs := c.metric("corral_request_errors_total", "error_class", "protocol")
			conn := c.dial()
			if err := conn.Send(tc.raw); err != nil {
				t.Fatal(err)
			}
			closedNoBytes(t, conn)
			c.waitDelta(proto, 1, "corral_protocol_errors_total", "side", "client", "error_detail", tc.detail)
			c.waitDelta(reqs, 1, "corral_requests_total", "outcome", "proxy_error")
			c.waitDelta(errs, 1, "corral_request_errors_total", "error_class", "protocol")
		})
	}
	if n := len(userMessages(tw)); n != 0 {
		t.Fatalf("the twin received %d messages from refused framing cases", n)
	}
}

// AC-4: a valid OP_MSG with a correct checksum and a kind-1 section is proxied
// successfully (the twin sees the documents section).
func TestValidChecksummedKind1Proxied_AC4(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	m := rt(t, conn, rawwire.OpMsg(rawwire.OpMsgOpts{
		RequestID: 5, Checksum: rawwire.ChecksumValid,
		Body:      d("insert", "c", "ordered", true, "$db", "d", "corraltestMarker", "k1"),
		Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{d("_id", 1, "x", "a"), d("_id", 2, "x", "b")}}},
	}))
	if !isOK(m) || m.Header.ResponseTo != 5 {
		t.Fatalf("reply %v responseTo %d", doc(m), m.Header.ResponseTo)
	}
	got := tw.ReceivedCommand("insert")
	if len(got) != 1 || len(got[0].Sequences["documents"]) != 2 {
		t.Fatalf("the twin received %d inserts, sequences %v", len(got), got)
	}
}

// AC-4 / AC-37 end to end: when several faults combine, the detail is the one
// first in FR-5's order.
func TestFramingFaultOrder_AC37(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	cases := []struct {
		name   string
		raw    []byte
		detail string
	}{
		{"unknown opcode, length 10", rawwire.RawHeader(10, 1, 0, 9999, nil), "malformed_message"},
		{"unknown opcode, length 60,000,000", rawwire.RawHeader(60_000_000, 1, 0, 9999, nil), "message_too_large"},
		{"unknown flag and bad checksum", rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Flags: 1 << 3, Checksum: rawwire.ChecksumInvalid, Body: d("ping", 1, "$db", "admin")}), "malformed_message"},
		{"minimum length minus one", rawwire.RawHeader(25, 1, 0, rawwire.OpCodeMsg, make([]byte, 9)), "malformed_message"},
		{"INT32_MIN", rawwire.RawHeader(-2147483648, 1, 0, rawwire.OpCodeMsg, nil), "malformed_message"},
		{"zero", rawwire.RawHeader(0, 1, 0, rawwire.OpCodeMsg, nil), "malformed_message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := c.with(t)
			before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", tc.detail)
			conn := c.dial()
			conn.Send(tc.raw)
			closedNoBytes(t, conn)
			c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", tc.detail)
		})
	}
	_ = fmt.Sprint
}
