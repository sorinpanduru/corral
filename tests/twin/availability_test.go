//go:build twin

package twin

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func authedMongos(t *testing.T, o opts, mod ...func(*mongosim.HelloSpec)) (*corral, *mongosim.Server, authCreds) {
	t.Helper()
	a := newAuth(t)
	h := mongosim.HelloSpec{Role: mongosim.Mongos}
	for _, f := range mod {
		f(&h)
	}
	tw := mongosim.Start(t, mongosim.Options{Hello: h, Users: a.users(), RequireAuth: true})
	if o.set == nil {
		o.set = map[string]string{}
	}
	o.set["upstream.auth.mechanism"] = "SCRAM-SHA-256"
	o.env = append(o.env, a.env...)
	o.upstream = []string{tw.Addr()}
	return start(t, o), tw, a
}

// AC-25 (FR-22, FR-46, twin): authentication failures give upstream_auth_failed
// and corral keeps running; transient errors do not.
func TestAuthFailureCodesAndReadiness_AC25(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    mongosim.AuthOutcome
	}{{"18", mongosim.Fail18}, {"334", mongosim.Fail334}, {"bad signature", mongosim.BadServerSignature}} {
		t.Run(tc.name, func(t *testing.T) {
			tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}, Users: map[string]string{"u": "p"}, RequireAuth: true})
			tw.SetAuthOutcome(tc.o)
			a := newAuth(t)
			c := start(t, opts{upstream: []string{tw.Addr()}, env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
			c.waitReadyz(503, "upstream_auth_failed")
			if m := rt(t, c.dial(), find(1, "x")); code(m) != 6 {
				t.Fatalf("%v", doc(m))
			}
			c.waitRecords("upstream authentication failed", 1)
			if c.metric("corral_upstream_auth_failures_total", "kind", "monitor") < 1 {
				t.Fatal("no monitor auth failure counted")
			}
		})
	}
	t.Run("transient errors do not set the failing state", func(t *testing.T) {
		for name, o := range map[string]mongosim.AuthOutcome{
			"10107": mongosim.ErrorCode(10107, "NotWritablePrimary"), "91": mongosim.ErrorCode(91, "ShutdownInProgress"),
			"11600": mongosim.ErrorCode(11600, "InterruptedAtShutdown"), "reset": mongosim.AuthReset,
		} {
			t.Run(name, func(t *testing.T) {
				c, tw, _ := authedMongos(t, opts{})
				_ = tw
				tw.SetAuthOutcome(o)
				time.Sleep(1500 * time.Millisecond)
				if _, body := c.readyz(); body == "upstream_auth_failed" {
					t.Fatalf("a transient authentication error gave upstream_auth_failed")
				}
			})
		}
	})
	t.Run("recovery gives 200", func(t *testing.T) {
		c, tw, _ := authedMongos(t, opts{})
		tw.SetAuthOutcome(mongosim.Fail18)
		tw.CloseConns() // force new authentications
		c.waitReadyz(503, "upstream_auth_failed")
		tw.SetAuthOutcome(mongosim.Succeed)
		c.waitReady()
	})
}

// AC-41 (FR-19, FR-46, twin): authentication rejected from startup with hello
// answered: handshakes succeed with the twin's parameters, requests get 6.
func TestAuthRejectedFromStartup_AC41(t *testing.T) {
	tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos, MaxWriteBatchSize: 31337}, Users: map[string]string{"u": "p"}, RequireAuth: true})
	tw.SetAuthOutcome(mongosim.Fail18)
	a := newAuth(t)
	c := start(t, opts{upstream: []string{tw.Addr()}, env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
	c.waitReadyz(503, "upstream_auth_failed")
	conn := c.dial()
	m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin"))
	if v, _ := intField(doc(m), "maxWriteBatchSize"); code(m) != 0 || v != 31337 {
		t.Fatalf("handshake: %v", doc(m))
	}
	before := c.metric("corral_pool_checkout_failures_total", "error_detail", "upstream_auth_failed")
	begin := time.Now()
	if m := rt(t, conn, find(2, "x")); code(m) != 6 || time.Since(begin) > time.Second {
		t.Fatalf("find: %v after %v", doc(m), time.Since(begin))
	}
	c.waitDelta(before, 1, "corral_pool_checkout_failures_total", "error_detail", "upstream_auth_failed")
}

// AC-41 (twin): pool connections refused while the checks succeed: readiness
// stays 200, requests wait and get 262; when the checks and their retries fail
// too, readiness becomes upstream_unavailable.
func TestPoolConnectionsRefusedChecksSucceed_AC41(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"pool.checkout_timeout": "1500ms"}})
	c.waitReady()
	tw.RefuseApp("corral", true)
	begin := time.Now()
	m := rt(t, c.dial(), find(1, "x"))
	if code(m) != 262 || time.Since(begin) < time.Second {
		t.Fatalf("%v after %v", doc(m), time.Since(begin))
	}
	if code, _ := c.readyz(); code != 200 {
		t.Fatalf("readiness %d while the checks succeed", code)
	}
	tw.FailChecks(-1, mongosim.CheckReset)
	c.waitReadyz(503, "upstream_unavailable")
	tw.ClearChecks()
	tw.RefuseApp("corral", false)
	c.waitReady()
}

// AC-32 (FR-56, twin): wait versus fail fast.
func TestWaitVersusFailFast_AC32(t *testing.T) {
	t.Run("no primary: wait, then 262; a primary within the timeout succeeds", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, map[string]string{"pool.checkout_timeout": "2500ms"}))
		c.waitReady()
		cl.NoPrimary()
		c.waitReadyz(503, "upstream_unavailable")
		begin := time.Now()
		if m := rt(t, c.dial(), find(1, "waits")); code(m) != 262 || time.Since(begin) < 2*time.Second {
			t.Fatalf("%v after %v", doc(m), time.Since(begin))
		}
		conn := c.dial()
		conn.Send(find(2, "waits-then-ok"))
		time.Sleep(700 * time.Millisecond)
		cl.SetPrimary(0)
		m, err := conn.ReadMessage(10 * time.Second)
		if err != nil || !isOK(m) {
			t.Fatalf("a primary reappeared within the timeout: %v %v", err, m)
		}
	})
	t.Run("no pool connection attempts while no primary", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, map[string]string{"pool.min_size": "1"}))
		c.waitReady()
		cl.NoPrimary()
		c.waitReadyz(503, "upstream_unavailable")
		time.Sleep(300 * time.Millisecond)
		count := func() int {
			n := 0
			for i := 0; i < 3; i++ {
				n += cl.Member(i).ConnAttemptsBy("corral")
			}
			return n
		}
		base := count()
		time.Sleep(2 * time.Second)
		if got := count(); got != base {
			t.Fatalf("%d pool connection attempts while no primary is known", got-base)
		}
	})
	t.Run("never reached: 6 at once", func(t *testing.T) {
		c := start(t, opts{})
		begin := time.Now()
		if m := rt(t, c.dial(), find(1, "x")); code(m) != 6 || time.Since(begin) > time.Second {
			t.Fatalf("%v after %v", doc(m), time.Since(begin))
		}
	})
	t.Run("requests queued when auth starts failing get 6 at once", func(t *testing.T) {
		c, tw, _ := authedMongos(t, opts{set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "30s"}})
		c.waitReady()
		hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
		a := c.dial()
		a.Send(find(1, "hold"))
		if !hold.WaitHeld(1, 10*time.Second) {
			t.Fatal("not held")
		}
		b := c.dial()
		b.Send(find(1, "queued"))
		time.Sleep(300 * time.Millisecond)
		tw.SetAuthOutcome(mongosim.Fail18)
		tw.CloseConns()
		m, err := b.ReadMessage(10 * time.Second)
		if err != nil || code(m) != 6 {
			t.Fatalf("queued request: %v %v", err, m)
		}
	})
	t.Run("check and retry both fail: unavailable, requests wait", func(t *testing.T) {
		c, tw := startMongos(t, opts{set: map[string]string{"pool.checkout_timeout": "1500ms"}})
		c.waitReady()
		tw.FailChecks(-1, mongosim.CheckReset)
		c.waitReadyz(503, "upstream_unavailable")
		n0 := len(tw.ReceivedCommand("find"))
		m := rt(t, c.dial(), find(1, "waiting"))
		if code(m) != 262 || len(tw.ReceivedCommand("find")) != n0 {
			t.Fatalf("%v (finds received: %d)", doc(m), len(tw.ReceivedCommand("find"))-n0)
		}
	})
	t.Run("single failed check with a successful retry keeps readiness", func(t *testing.T) {
		c, tw := startMongos(t, opts{})
		c.waitReady()
		tw.FailChecks(1, mongosim.CheckReset)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if code, body := c.readyz(); code != 200 {
				t.Fatalf("readiness dropped: %d %s", code, body)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if m := rt(t, c.dial(), find(1, "ok")); !isOK(m) {
			t.Fatalf("%v", doc(m))
		}
	})
	t.Run("never reached with a standalone seed: incompatible, not unavailable", func(t *testing.T) {
		tw := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Standalone}})
		c := start(t, opts{upstream: []string{tw.Addr()}})
		c.waitReadyz(503, "upstream_incompatible")
		before := c.metric("corral_pool_checkout_failures_total", "error_detail", "upstream_incompatible")
		conn := c.dial()
		if m := rt(t, conn, cmd(1, "hello", 1, "$db", "admin")); code(m) != 6 {
			t.Fatalf("handshake: %v", doc(m))
		}
		if m := rt(t, conn, find(2, "x")); code(m) != 6 {
			t.Fatalf("request: %v", doc(m))
		}
		c.waitDelta(before, 1, "corral_pool_checkout_failures_total", "error_detail", "upstream_incompatible")
		if c.metric("corral_pool_checkout_failures_total", "error_detail", "upstream_unavailable") != 0 {
			t.Fatal("counted as upstream_unavailable")
		}
	})
}
