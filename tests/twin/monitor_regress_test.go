//go:build twin

package twin

import (
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/corralprobe"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// FR-37 / AC-61, end to end: a silent seed with connect_timeout 2 s receives
// exactly one monitoring connection before the judgement, and its next check
// starts at least 500 ms after the first ended.
func TestSilentSeedSpacingFromEndOfCheck_FR37(t *testing.T) {
	a := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	a.FailChecks(-1, mongosim.CheckNoReply)
	b := foreignSeed(t)
	c := start(t, opts{upstream: []string{a.Addr(), b.Addr()}, rs: "rs0", set: map[string]string{"upstream.connect_timeout": "2s"}})
	time.Sleep(1500 * time.Millisecond)
	if n := a.ConnAttemptsBy(mongosim.MonitorAppName); n != 1 {
		t.Fatalf("the silent seed got %d monitoring connections before the judgement, want exactly 1", n)
	}
	c.waitRecords("upstream incompatible", 1)
	hellos := func() []time.Time {
		var out []time.Time
		for _, m := range a.Received() {
			if m.AppName == mongosim.MonitorAppName && strings.EqualFold(m.Command, "hello") {
				out = append(out, m.Time)
			}
		}
		return out
	}
	eventually(t, "a second check of the silent seed", func() bool { return len(hellos()) >= 2 })
	h := hellos()
	// The first check ended at its connect timeout (2 s after it started).
	if gap := h[1].Sub(h[0]); gap < 2*time.Second+400*time.Millisecond {
		t.Fatalf("the second check started %v after the first started; it must be at least the connect timeout plus 500 ms (with tolerance)", gap)
	}
}

// FR-64: the corraltest /conns probe lists open connections only; a connection
// is dropped from it once it closes.
func TestProbeDropsClosedConnections_FR64(t *testing.T) {
	c, _ := startMongos(t, opts{probe: true})
	c.waitReady()
	pr := corralprobe.Open(t, c.p.HoldDir())
	var conns []*rawwire.Conn
	for i := 0; i < 3; i++ {
		conn := c.dial()
		rt(t, conn, cmd(1, "hello", 1, "$db", "admin"))
		conns = append(conns, conn)
	}
	count := func() int {
		m, err := pr.Conns()
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}
	eventually(t, "three connections listed", func() bool { return count() == 3 })
	before, _ := pr.Conns()
	conns[0].Close()
	eventually(t, "the closed connection dropped", func() bool { return count() == 2 })
	after, _ := pr.Conns()
	gone := 0
	for id := range before {
		if _, ok := after[id]; !ok {
			gone++
		}
	}
	if gone != 1 {
		t.Fatalf("%d connections left the list, want exactly 1", gone)
	}
	conns[1].Close()
	conns[2].Close()
	eventually(t, "an empty list", func() bool { return count() == 0 })
}

// FR-22 / FR-46: when the target's authenticating monitoring check fails
// transiently (SCRAM stalls longer than connect_timeout), the target stays
// unusable until an authenticating check succeeds: /readyz reaches 503
// upstream_unavailable within heartbeat + 2 x connect_timeout, stays there
// while the stall lasts, and recovers to 200 once authentication works.
func TestAuthStallKeepsTargetUnusable_FR22(t *testing.T) {
	a := newAuth(t)
	tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: a.users(), RequireAuth: true})
	const connectTimeout = time.Second
	const heartbeat = 500 * time.Millisecond
	c := start(t, opts{upstream: []string{tw.Addr()}, env: a.env, set: map[string]string{
		"upstream.auth.mechanism": "SCRAM-SHA-256", "upstream.connect_timeout": connectTimeout.String(), "upstream.heartbeat_interval": heartbeat.String()}})
	c.waitReady()
	eventually(t, "an authenticated monitoring connection", func() bool {
		for _, ci := range tw.Conns() {
			if ci.AppName == mongosim.MonitorAppName && ci.Authenticated && ci.Open {
				return true
			}
		}
		return false
	})
	// SCRAM now stalls longer than the connect timeout; dropping the monitoring
	// connection makes the next check authenticate again.
	tw.SetAuthOutcome(mongosim.AuthDelay(10 * time.Second))
	begin := time.Now()
	tw.CloseConns()
	c.waitReadyz(503, "upstream_unavailable")
	if d := time.Since(begin); d > heartbeat+2*connectTimeout+time.Second {
		t.Fatalf("readiness took %v to reach 503, want within heartbeat + 2 x connect_timeout (%v)", d, heartbeat+2*connectTimeout)
	}
	// It stays there while the stall lasts, although a hello alone succeeds.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if code, body := c.readyz(); code != 503 || body != "upstream_unavailable" {
			t.Fatalf("readiness during the stall: %d %q", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Authentication works again: recovery to 200.
	tw.SetAuthOutcome(mongosim.Succeed)
	c.waitReady()
}
