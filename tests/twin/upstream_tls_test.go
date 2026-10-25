//go:build twin

package twin

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

// SPEC-002 FR-45 and AC-24 (upstream TLS), and the Behavior on failure rows:
// corral verifies the upstream certificate chain, host name and protocol
// version; a failed handshake makes the connection attempt fail with class
// network and detail tls_handshake; readiness is upstream_unavailable; no
// request reaches the upstream; no certificate identity or path is emitted.
// The twin is mongosim's upstream TLS mode.

func tlsTwin(t *testing.T, mode mongosim.TLSMode, auth *mongosim.Authority) *mongosim.Server {
	t.Helper()
	return mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, TLS: mode, Authority: auth})
}

func tlsOptsFor(tw *mongosim.Server, caFile string, extra map[string]string) opts {
	set := map[string]string{"upstream.tls.enabled": "true", "upstream.tls.ca_path": caFile}
	for k, v := range extra {
		set[k] = v
	}
	return opts{upstream: []string{tw.Addr()}, set: set}
}

// The control: a valid certificate with the right CA works end to end.
func TestUpstreamTLSValidControl_FR45(t *testing.T) {
	tw := tlsTwin(t, mongosim.TLSValid, nil)
	c := start(t, tlsOptsFor(tw, tw.CAFile(), nil))
	c.waitReady()
	if m := rt(t, c.dial(), find(1, "tls-ok")); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	if n := countCommand(tw, "find"); n != 1 {
		t.Fatalf("the twin received %d finds", n)
	}
	if n := tw.TLSHandshakeFailures(); n != 0 {
		t.Fatalf("%d failed handshakes with a valid certificate", n)
	}
	if got := c.metric("corral_upstream_connect_errors_total", "error_class", "network"); got != 0 {
		t.Fatalf("connect errors with a valid upstream: %v", got)
	}
}

// Expired certificate, host name mismatch, a TLS 1.1-only upstream and a wrong
// CA each fail the handshake.
func TestUpstreamTLSFailures_FR45(t *testing.T) {
	cases := []struct {
		name  string
		mode  mongosim.TLSMode
		wrong bool // the client trusts a different CA
		// leaks are strings that identify the certificate and must not be emitted.
		leaks []string
	}{
		{"expired certificate", mongosim.TLSExpired, false, []string{"corral-test-ca"}},
		{"host name mismatch", mongosim.TLSHostnameMismatch, false, []string{"mismatch.corral-test.invalid", "corral-test-ca"}},
		{"TLS 1.1 only", mongosim.TLSLegacyVersion, false, []string{"corral-test-ca"}},
		{"wrong CA", mongosim.TLSValid, true, []string{"corral-test-ca"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := mongosim.NewAuthority(t)
			tw := tlsTwin(t, tc.mode, auth)
			ca := auth.CAFile()
			if tc.wrong {
				ca = mongosim.NewAuthority(t).CAFile()
			}
			c := start(t, tlsOptsFor(tw, ca, nil))

			// Readiness: never reached, upstream_unavailable (not incompatible,
			// not auth failed), and it stays there.
			c.waitReadyz(503, "upstream_unavailable")
			time.Sleep(1200 * time.Millisecond)
			if code, body := c.readyz(); code != 503 || body != "upstream_unavailable" {
				t.Fatalf("readiness %d %q", code, body)
			}
			if c.metric("corral_upstream_available") != 0 {
				t.Fatal("corral_upstream_available is not 0")
			}
			// Class and detail: a network failure of the monitoring connection,
			// logged with detail tls_handshake and only the FR-49 keys.
			c.waitMetricAtLeastTwin(1, "corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "network")
			rec := c.waitRecords("upstream connection failed", 1)[0]
			if rec["error"] != "network" || rec["corral.error.detail"] != "tls_handshake" || rec.Component() != "upstream" || rec["level"] != "WARN" {
				t.Fatalf("record %v", rec)
			}
			allowed := map[string]bool{"time": true, "level": true, "msg": true, "component": true, "error": true,
				"corral.error.detail": true, "server.address": true, "server.port": true, "trace_id": true, "span_id": true, "suppressed_count": true}
			for k := range rec {
				if !allowed[k] {
					t.Fatalf("the record carries %q: %v", k, rec)
				}
			}
			// No request is forwarded: handshakes and requests get 6 and the twin
			// sees no command at all (the handshake never completed).
			conn := c.dial()
			if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin")); code(m) != 6 {
				t.Fatalf("handshake: %v", doc(m))
			}
			if m := rt(t, conn, find(2, "tls-fail")); code(m) != 6 {
				t.Fatalf("request: %v", doc(m))
			}
			if n := len(tw.Received()); n != 0 {
				t.Fatalf("the twin received %d messages over a failed TLS handshake", n)
			}
			if tw.ConnAttemptsBy("corral") != 0 {
				t.Fatal("a pool connection was opened to an upstream that fails TLS verification")
			}
			// The handshake failed on the server side too, and no authentication
			// failure was counted.
			eventually(t, "a failed server-side handshake", func() bool { return tw.TLSHandshakeFailures() >= 1 })
			if got := c.metric("corral_upstream_auth_failures_total"); got != 0 {
				t.Fatalf("auth_failures_total = %v for a TLS failure", got)
			}
			// No certificate identity, subject or path in any output.
			for name, out := range map[string][]byte{"stdout": c.p.Stdout(), "stderr": c.p.Stderr()} {
				for _, l := range append(tc.leaks, ca) {
					if bytes.Contains(out, []byte(l)) {
						t.Fatalf("%s contains %q", name, l)
					}
				}
			}
			if txt, _ := c.p.Metrics(); strings.Contains(txt, "corral-test-ca") || strings.Contains(txt, ca) {
				t.Fatal("/metrics contains certificate identity or a path")
			}
		})
	}
}

// FR-35 / AC-57: a TLS failure of a usable target's check is retried once, at
// once, on a new connection, before the target is marked unknown (unlike an
// error reply, which is applied at once). The heartbeat here is 5 s, so two
// failed handshakes that are a few hundred milliseconds apart are the check and
// its retry.
func TestUpstreamTLSCheckFailureRetriedOnce_AC57(t *testing.T) {
	auth := mongosim.NewAuthority(t)
	good := tlsTwin(t, mongosim.TLSValid, auth)
	addr := good.Addr()
	c := start(t, tlsOptsFor(good, auth.CAFile(), map[string]string{"upstream.heartbeat_interval": "5s"}))
	c.waitReady()
	// The upstream now presents an expired certificate on the same address.
	good.Stop()
	bad := mongosim.Start(t, mongosim.Options{Addr: addr, Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, TLS: mongosim.TLSExpired, Authority: auth})
	var first, second time.Time
	eventually(t, "the failed check and its retry", func() bool {
		n := bad.TLSHandshakeFailures()
		now := time.Now()
		if n >= 1 && first.IsZero() {
			first = now
		}
		if n >= 2 && second.IsZero() {
			second = now
		}
		return !second.IsZero()
	})
	if gap := second.Sub(first); gap > 1500*time.Millisecond {
		t.Fatalf("the second failed handshake came %v after the first: a TLS check failure is retried at once, not at the next heartbeat", gap)
	}
	// After the retry fails too, the target is unknown: readiness drops.
	c.waitReadyz(503, "upstream_unavailable")
	if m := rt(t, c.dial(), find(1, "after-tls-failure")); code(m) != 262 && code(m) != 6 {
		t.Fatalf("a request must wait or fail fast, got %v", doc(m))
	}
	if n := len(bad.Received()); n != 0 {
		t.Fatalf("the failing upstream received %d messages", n)
	}
}

// waitMetricAtLeastTwin waits until the metric has reached at least want.
func (c *corral) waitMetricAtLeastTwin(want float64, name string, kv ...string) {
	c.t.Helper()
	eventually(c.t, name, func() bool { return c.metric(name, kv...) >= want })
}
