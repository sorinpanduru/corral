//go:build twin

package twin

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

// SPEC-002 FR-44 (TLS 1.2 or later on the wire listener), FR-49 and the
// Behavior on failure row "Client TLS handshake fails": a client that offers
// only TLS 1.0 to 1.1 is refused. The connection is closed with reason
// tls_failure, one rate-limited warn record with class network and detail
// tls_handshake (and no certificate identity), and no request is processed.
func TestClientOffersOnlyLegacyTLSIsClosed_FR44(t *testing.T) {
	cs := genCerts(t)
	c, tw := startMongos(t, tlsOpts(cs, nil))
	c.waitReady()
	closed := c.metric("corral_client_connections_closed_total", "reason", "tls_failure")
	accepted := c.metric("corral_client_connections_accepted_total")
	requests := c.metric("corral_requests_total")
	n0 := len(userMessages(tw))

	cfg := &tls.Config{RootCAs: cs.pool, ServerName: "localhost", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(&d, "tcp", c.proxy, cfg)
	if err == nil {
		conn.Close()
		t.Fatal("a TLS 1.0-1.1 client completed a handshake with the wire listener")
	}
	c.waitDelta(closed, 1, "corral_client_connections_closed_total", "reason", "tls_failure")
	c.waitDelta(accepted, 1, "corral_client_connections_accepted_total")
	rec := c.waitRecords("client TLS handshake failed", 1)[0]
	if rec.Component() != "proxy" || rec["level"] != "WARN" || rec["error"] != "network" || rec["corral.error.detail"] != "tls_handshake" {
		t.Fatalf("record %v", rec)
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "component": true, "error": true,
		"corral.error.detail": true, "trace_id": true, "span_id": true, "suppressed_count": true}
	for k := range rec {
		if !allowed[k] {
			t.Fatalf("the record carries %q: %v", k, rec)
		}
	}
	// No request was processed.
	c.steady(requests, "corral_requests_total")
	if n := len(userMessages(tw)); n != n0 {
		t.Fatalf("%d messages reached the upstream", n-n0)
	}
	// No certificate identity in the output.
	for _, out := range [][]byte{c.p.Stdout(), c.p.Stderr()} {
		for _, leak := range []string{"corral-twin-server.invalid", "corral-twin-ca.invalid", cs.certPath, cs.keyPath} {
			if containsBytes(out, leak) {
				t.Fatalf("the output contains %q", leak)
			}
		}
	}
	// A client that offers TLS 1.2 still works afterwards.
	ok := tlsDialOK(t, c.proxy, cs)
	if !ok {
		t.Fatal("a TLS 1.2 client was refused after the legacy one")
	}
}

func containsBytes(b []byte, s string) bool { return len(s) > 0 && strings.Contains(string(b), s) }

func tlsDialOK(t *testing.T, addr string, cs certs) bool {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{RootCAs: cs.pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
