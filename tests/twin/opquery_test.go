//go:build twin

package twin

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

// AC-38 (FR-8, FR-48): malformed OP_QUERY variants close with
// malformed_message; a valid non-handshake OP_QUERY gets 352 and leaves
// protocol_errors_total unchanged while requests_total{proxy_error} and
// request_errors_total{protocol} each grow by 1.
func TestOpQueryMalformedVariants_AC38(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	base := rawwire.OpQueryOpts{RequestID: 1, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: d("isMaster", 1)}
	variants := map[string]rawwire.OpQueryOpts{
		"unterminated namespace":   {RequestID: 1, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: d("isMaster", 1), UnterminatedNamespace: true},
		"missing numberToReturn":   {RequestID: 1, FullCollection: "admin.$cmd", Query: d("isMaster", 1), OmitNumberToReturn: true},
		"invalid query document":   {RequestID: 1, FullCollection: "admin.$cmd", NumberToReturn: -1, OmitQuery: true, Extra: []byte{5, 0, 0, 0, 1}},
		"bytes after the selector": {RequestID: 1, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: d("isMaster", 1), Extra: []byte{0, 0, 0, 0, 0, 9}},
	}
	_ = base
	for name, o := range variants {
		t.Run(name, func(t *testing.T) {
			before := c.metric("corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
			conn := c.dial()
			conn.Send(rawwire.OpQuery(o))
			closedNoBytes(t, conn)
			c.waitDelta(before, 1, "corral_protocol_errors_total", "side", "client", "error_detail", "malformed_message")
		})
	}
	_ = tw
	t.Run("valid non-handshake 352", func(t *testing.T) {
		proto := c.metric("corral_protocol_errors_total")
		reqs := c.metric("corral_requests_total", "outcome", "proxy_error")
		errs := c.metric("corral_request_errors_total", "error_class", "protocol")
		m := rt(t, c.dial(), rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 2, FullCollection: "d.c", NumberToReturn: 1, Query: d("find", "c")}))
		if code(m) != 352 {
			t.Fatalf("%v", doc(m))
		}
		c.waitDelta(reqs, 1, "corral_requests_total", "outcome", "proxy_error")
		c.waitDelta(errs, 1, "corral_request_errors_total", "error_class", "protocol")
		if got := c.metric("corral_protocol_errors_total"); got != proto {
			t.Fatalf("protocol_errors_total changed from %v to %v", proto, got)
		}
	})
}

// AC-50 (FR-8, FR-23, FR-33, FR-61; R-1): auth and blocked commands over
// OP_QUERY get the 352 reply, never reach the twin, keep the connection open,
// and are labelled as specified; the auth-rejected and blocked counters are
// unchanged.
func TestOpQueryAuthAndBlockedGet352_AC50(t *testing.T) {
	c, tw := startMongos(t, opts{})
	c.waitReady()
	cases := []struct {
		name    string
		query   any
		command string
	}{
		{"saslStart", d("saslStart", 1, "mechanism", "SCRAM-SHA-256"), "saslStart"},
		{"getnonce", d("getnonce", 1), "other"},
		{"currentOp", d("currentOp", 1), "other"},
		{"$query killOp", d("$query", d("killOp", 1, "op", 1)), "other"},
		{"$query saslStart", d("$query", d("saslStart", 1)), "saslStart"},
	}
	conn := c.dial()
	n0 := len(userMessages(tw))
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := c.metric("corral_client_auth_rejected_total")
			blk := c.metric("corral_commands_blocked_total")
			req := c.metric("corral_requests_total", "command", tc.command, "outcome", "proxy_error")
			all := c.metric("corral_requests_total", "outcome", "proxy_error")
			errs := c.metric("corral_request_errors_total", "error_class", "protocol")
			m := rt(t, conn, rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: int32(i + 1), FullCollection: "admin.$cmd", NumberToReturn: 1, Query: tc.query}))
			if m.Flags&2 == 0 || code(m) != 352 {
				t.Fatalf("flags %d %v", m.Flags, doc(m))
			}
			c.waitDelta(req, 1, "corral_requests_total", "command", tc.command, "outcome", "proxy_error")
			c.waitDelta(all, 1, "corral_requests_total", "outcome", "proxy_error")
			c.waitDelta(errs, 1, "corral_request_errors_total", "error_class", "protocol")
			c.steady(auth, "corral_client_auth_rejected_total")
			c.steady(blk, "corral_commands_blocked_total")
		})
	}
	if n := len(userMessages(tw)); n != n0 {
		t.Fatalf("%d refused OP_QUERY messages reached the twin", n-n0)
	}
	if m := rt(t, conn, ping(99)); !isOK(m) {
		t.Fatal("the connection did not stay usable")
	}
}
