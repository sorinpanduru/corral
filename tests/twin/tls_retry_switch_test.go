//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

// FR-35 / AC-57 with the TLS-switching twin: a usable target's check stalls past
// connect_timeout on its existing connection while the server switches to a
// failing certificate. That timeout is transient, so it is retried exactly once,
// on a new connection, whose handshake fails; no further attempt happens before
// the target is marked unknown; readiness becomes upstream_unavailable; the
// failures are counted (timeout for the stall, network for the handshake); and
// switching back to a valid certificate restores readiness.
func TestStalledCheckThenFailingTLSRetriedOnce_AC57(t *testing.T) {
	auth := mongosim.NewAuthority(t)
	tw := tlsTwin(t, mongosim.TLSValid, auth)
	const connectTimeout = time.Second
	c := start(t, tlsOptsFor(tw, auth.CAFile(), map[string]string{
		"upstream.connect_timeout": connectTimeout.String(), "upstream.heartbeat_interval": "2s"}))
	c.waitReady()
	monitorConn := -1
	eventually(t, "the monitoring connection", func() bool {
		for _, ci := range tw.Conns() {
			if ci.AppName == mongosim.MonitorAppName && ci.Open {
				monitorConn = ci.Index
				return true
			}
		}
		return false
	})
	baseline := len(tw.TLSHandshakes())

	// Stall the next hello on the existing connection past connect_timeout, and
	// make every new handshake fail.
	tw.SetTLS(mongosim.TLSExpired)
	if err := tw.StallNextHello(monitorConn, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// Sample readiness tightly so the number of attempts at the moment of the
	// drop is exact (later checks are at least 500 ms apart).
	var attemptsAtDrop int
	eventuallyWithin(t, 30*time.Second, "readiness to drop", func() bool {
		if code, _ := c.readyz(); code != 200 {
			attemptsAtDrop = len(tw.TLSHandshakes()) - baseline
			return true
		}
		time.Sleep(10 * time.Millisecond)
		return false
	})
	hs := tw.TLSHandshakes()[baseline:]
	if attemptsAtDrop != 1 {
		t.Fatalf("%d handshake attempts before the target was marked unknown, want exactly 1 (the retry): %+v", attemptsAtDrop, hs)
	}
	if hs[0].OK {
		t.Fatalf("the retry's handshake did not fail: %+v", hs)
	}
	if hs[0].ConnIndex == monitorConn {
		t.Fatal("the retry reused the stalled connection")
	}
	c.waitReadyz(503, "upstream_unavailable")
	c.waitMetricAtLeastTwin(1, "corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "timeout")
	c.waitMetricAtLeastTwin(1, "corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "network")
	// "upstream connection failed" is rate-limited to one record per 30 s and
	// shared by both failures: the first (the stall's timeout) is the one logged.
	rec := c.waitRecords("upstream connection failed", 1)[0]
	if d := rec["corral.error.detail"]; d != "deadline_exceeded" && d != "tls_handshake" {
		t.Fatalf("record %v", rec)
	}
	// Back to a valid certificate: readiness is restored.
	tw.SetTLS(mongosim.TLSValid)
	c.waitReady()
}
