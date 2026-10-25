//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-61 (FR-18, FR-19, FR-46, FR-49, FR-56; Amendment 5), twin, replica-set
// mode, seeds A then B in upstream.hosts order, upstream never reached:
// incompatibility from seed results is judged only once every seed has
// finished its first check.

func foreignSeed(t *testing.T) *mongosim.Server {
	return mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary, SetName: "other", ElectionID: make([]byte, 12)}})
}

func matchingSeed(t *testing.T) *mongosim.Server {
	s := mongosim.Start(t, mongosim.Options{})
	s.SetHello(mongosim.HelloSpec{Role: mongosim.Primary, SetName: "rs0", Hosts: []string{s.Addr()}, Primary: s.Addr(), ElectionID: make([]byte, 12), SetVersion: 1})
	return s
}

func assertUnavailableNotJudged(t *testing.T, c *corral, what string) {
	t.Helper()
	if code, body := c.readyz(); code != 503 || body != "upstream_unavailable" {
		t.Fatalf("%s: readyz %d %q, want 503 upstream_unavailable", what, code, body)
	}
	before := c.metric("corral_pool_checkout_failures_total", "error_detail", "upstream_unavailable")
	conn := c.dial()
	if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin")); code(m) != 6 {
		t.Fatalf("%s: handshake %v", what, doc(m))
	}
	if m := rt(t, conn, find(2, "x")); code(m) != 6 {
		t.Fatalf("%s: request %v", what, doc(m))
	}
	c.waitDelta(before, 1, "corral_pool_checkout_failures_total", "error_detail", "upstream_unavailable")
	if n := len(c.records("upstream incompatible")); n != 0 {
		t.Fatalf("%s: %d upstream incompatible records", what, n)
	}
}

// A foreign seed answers at once, the matching seed after a delay shorter than
// the connect timeout: unavailable until then, no incompatible record ever.
func TestForeignSeedFirstThenMatching_AC61(t *testing.T) {
	a, b := foreignSeed(t), matchingSeed(t)
	b.FailChecks(1, mongosim.CheckDelay(1500*time.Millisecond))
	c := start(t, opts{upstream: []string{a.Addr(), b.Addr()}, rs: "rs0", set: map[string]string{"upstream.connect_timeout": "5s"}})
	assertUnavailableNotJudged(t, c, "B pending")
	c.waitReady()
	if n := len(c.records("upstream incompatible")); n != 0 {
		t.Fatalf("%d upstream incompatible records", n)
	}
}

// The reason does not depend on the order the checks finish.
func TestReasonIndependentOfAnswerOrder_AC61(t *testing.T) {
	for _, slowA := range []bool{true, false} {
		name := "A answers later"
		if !slowA {
			name = "B answers later"
		}
		t.Run(name, func(t *testing.T) {
			a := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Standalone}})
			b := foreignSeed(t)
			slow := b
			if slowA {
				slow = a
			}
			slow.FailChecks(1, mongosim.CheckDelay(1500*time.Millisecond))
			begin := time.Now()
			c := start(t, opts{upstream: []string{a.Addr(), b.Addr()}, rs: "rs0", set: map[string]string{"upstream.connect_timeout": "5s"}})
			assertUnavailableNotJudged(t, c, "one seed pending")
			c.waitReadyz(503, "upstream_incompatible")
			recs := c.waitRecords("upstream incompatible", 1)
			if time.Since(begin) < 1400*time.Millisecond {
				t.Fatal("the incompatibility was judged before both checks finished")
			}
			time.Sleep(700 * time.Millisecond)
			recs = c.records("upstream incompatible")
			if len(recs) != 1 || recs[0]["reason"] != "standalone" {
				t.Fatalf("records %v, want exactly one with reason standalone", recs)
			}
			before := c.metric("corral_pool_checkout_failures_total", "error_detail", "upstream_incompatible")
			conn := c.dial()
			if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin")); code(m) != 6 {
				t.Fatalf("handshake %v", doc(m))
			}
			if m := rt(t, conn, find(2, "x")); code(m) != 6 {
				t.Fatalf("request %v", doc(m))
			}
			c.waitDelta(before, 1, "corral_pool_checkout_failures_total", "error_detail", "upstream_incompatible")
		})
	}
}

// An unreachable seed counts once its first check fails at the connect timeout.
func TestUnreachableSeedCountsAtConnectTimeout_AC61(t *testing.T) {
	a := foreignSeed(t)
	b := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	b.FailChecks(-1, mongosim.CheckNoReply)
	begin := time.Now()
	c := start(t, opts{upstream: []string{a.Addr(), b.Addr()}, rs: "rs0", set: map[string]string{"upstream.connect_timeout": "2s"}})
	assertUnavailableNotJudged(t, c, "B has not failed yet")
	recs := c.waitRecords("upstream incompatible", 1)
	if d := time.Since(begin); d < 1800*time.Millisecond {
		t.Fatalf("judged after %v, before B's first check could fail at the connect timeout", d)
	}
	time.Sleep(500 * time.Millisecond)
	recs = c.records("upstream incompatible")
	if len(recs) != 1 || recs[0]["reason"] != "replica_set_name_mismatch" {
		t.Fatalf("records %v", recs)
	}
}

// A ghost seed never causes incompatibility itself: with A foreign it is judged
// once both first checks have finished.
func TestGhostSeedWithForeignSeed_AC61(t *testing.T) {
	a := foreignSeed(t)
	b := mongosim.Start(t, mongosim.Options{Hello: ghost()})
	c := start(t, opts{upstream: []string{a.Addr(), b.Addr()}, rs: "rs0"})
	c.waitReadyz(503, "upstream_incompatible")
	recs := c.waitRecords("upstream incompatible", 1)
	if recs[0]["reason"] != "replica_set_name_mismatch" {
		t.Fatalf("record %v", recs[0])
	}
}

// FR-37: every seed's first check starts at startup: three seeds that never
// answer all see a connection before any of them reaches the connect timeout.
func TestFirstChecksAreConcurrent_AC61(t *testing.T) {
	var seeds []*mongosim.Server
	var addrs []string
	for i := 0; i < 3; i++ {
		s := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
		s.Blackhole(true)
		seeds = append(seeds, s)
		addrs = append(addrs, s.Addr())
	}
	c := start(t, opts{upstream: addrs, rs: "rs0", set: map[string]string{"upstream.connect_timeout": "6s"}})
	_ = c
	eventually(t, "a first-check connection on every seed", func() bool {
		for _, s := range seeds {
			if s.ConnAttempts() < 1 {
				return false
			}
		}
		return true
	})
	var first, last time.Time
	for _, s := range seeds {
		for _, ci := range s.Conns() {
			if first.IsZero() || ci.Opened.Before(first) {
				first = ci.Opened
			}
			if ci.Opened.After(last) {
				last = ci.Opened
			}
			break
		}
	}
	if d := last.Sub(first); d > 2*time.Second {
		t.Fatalf("the first checks arrived %v apart, want all within the connect timeout (6 s) and concurrently", d)
	}
}
