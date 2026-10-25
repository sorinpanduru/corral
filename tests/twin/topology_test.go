//go:build twin

package twin

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func monitorChecks(s *mongosim.Server) int {
	n := 0
	for _, m := range s.Received() {
		if m.AppName == mongosim.MonitorAppName && strings.EqualFold(m.Command, "hello") || strings.EqualFold(m.Command, "ismaster") && m.AppName == mongosim.MonitorAppName {
			n++
		}
	}
	return n
}

func portOf(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }

func randHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// authCreds are random synthetic credentials for the upstream service identity:
// the twin knows them, corral gets them through the environment and a file.
type authCreds struct {
	user, pass string
	env        []string
}

func newAuth(t *testing.T) authCreds {
	t.Helper()
	a := authCreds{user: "svc-" + randHex(t, 6), pass: "pw-" + randHex(t, 12)}
	f := filepath.Join(t.TempDir(), "svc.pass")
	if err := os.WriteFile(f, []byte(a.pass), 0o600); err != nil {
		t.Fatal(err)
	}
	a.env = []string{"CORRAL_UPSTREAM_AUTH_USERNAME=" + a.user, "CORRAL_UPSTREAM_AUTH_PASSWORD_FILE=" + f}
	return a
}

func (a authCreds) users() map[string]string { return map[string]string{a.user: a.pass} }

func rsOpts(cl *mongosim.Cluster, extra map[string]string) opts {
	return opts{upstream: cl.Addrs(), rs: "rs0", set: extra}
}

func incompatReasons(c *corral) []string {
	var out []string
	for _, r := range c.records("upstream incompatible") {
		s, _ := r["reason"].(string)
		out = append(out, s)
	}
	return out
}

// AC-18 (twin, replica-set mode): incompatibility from the seed results.
func TestSeedIncompatibility_AC18(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hello  mongosim.HelloSpec
		reason string
	}{
		{"different setName", mongosim.HelloSpec{Role: mongosim.Primary, SetName: "other", ElectionID: make([]byte, 12)}, "replica_set_name_mismatch"},
		{"standalone", mongosim.HelloSpec{Role: mongosim.Standalone}, "standalone"},
		{"mongos", mongosim.HelloSpec{Role: mongosim.Mongos}, "topology_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := mongosim.Start(t, mongosim.Options{Hello: tc.hello})
			c := start(t, opts{upstream: []string{tw.Addr()}, rs: "rs0"})
			c.waitReadyz(503, "upstream_incompatible")
			if got := incompatReasons(c); len(got) != 1 || got[0] != tc.reason {
				t.Fatalf("records %v, want [%s]", got, tc.reason)
			}
			if m := rt(t, c.dial(), find(1, "x")); code(m) != 6 {
				t.Fatalf("%v", doc(m))
			}
		})
	}
}

// AC-18 (twin, single-mongos mode).
func TestSingleMongosModeIncompatibility_AC18(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hello  mongosim.HelloSpec
		reason string
	}{
		{"replica-set member", mongosim.HelloSpec{Role: mongosim.Primary, SetName: "rs0", ElectionID: make([]byte, 12)}, "topology_mismatch"},
		{"standalone", mongosim.HelloSpec{Role: mongosim.Standalone}, "standalone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := mongosim.Start(t, mongosim.Options{Hello: tc.hello})
			c := start(t, opts{upstream: []string{tw.Addr()}})
			c.waitReadyz(503, "upstream_incompatible")
			if got := incompatReasons(c); len(got) != 1 || got[0] != tc.reason {
				t.Fatalf("records %v, want [%s]", got, tc.reason)
			}
		})
	}
}

// AC-18 (twin, replica-set mode): a non-matching seed next to a matching member
// is ignored and the upstream stays compatible; it is still re-checked on the
// heartbeat schedule (AC-56).
func TestNonMatchingSeedIgnored_AC18(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
	foreign := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary, SetName: "other", ElectionID: make([]byte, 12)}})
	c := start(t, opts{upstream: append([]string{foreign.Addr()}, cl.Addrs()...), rs: "rs0"})
	c.waitReady()
	n := monitorChecks(foreign)
	time.Sleep(2500 * time.Millisecond)
	if monitorChecks(foreign) <= n {
		t.Fatal("the foreign seed is not re-checked")
	}
	if code, _ := c.readyz(); code != 200 || len(incompatReasons(c)) != 0 {
		t.Fatalf("readiness %d, incompatible records %v", code, incompatReasons(c))
	}
}

// AC-18 (twin, both modes): arbiters get no monitoring connection; only the
// target's monitoring connection authenticates; a secondary read goes to the
// primary.
func TestArbitersAuthAndReadPreference_AC18(t *testing.T) {
	a := newAuth(t)
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{Arbiters: 1, Users: a.users(), RequireAuth: true})
	c := start(t, opts{upstream: cl.Addrs(), rs: "rs0", env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256"}})
	c.waitReady()
	time.Sleep(1500 * time.Millisecond)
	if n := cl.Arbiter(0).ConnAttempts(); n != 0 {
		t.Fatalf("the arbiter received %d connection attempts", n)
	}
	for i := 0; i < cl.Len(); i++ {
		for _, ci := range cl.Member(i).Conns() {
			if ci.AppName == mongosim.MonitorAppName && ci.Authenticated != (i == cl.PrimaryIndex()) {
				t.Fatalf("member %d monitor connection authenticated=%v (primary is %d)", i, ci.Authenticated, cl.PrimaryIndex())
			}
		}
	}
	m := rt(t, c.dial(), mk(1, "secondary-read", "find", "c", "$db", "d", "$readPreference", d("mode", "secondary")))
	if !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	for i := 0; i < cl.Len(); i++ {
		n := 0
		for _, x := range cl.Member(i).ReceivedCommand("find") {
			if x.Marker == "secondary-read" {
				n++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[i == cl.PrimaryIndex()]; n != want {
			t.Fatalf("member %d received %d secondary reads, want %d", i, n, want)
		}
	}
}

func ghost(extra ...bson.E) mongosim.HelloSpec {
	return mongosim.HelloSpec{Role: mongosim.Other, Extra: append(bson.D{{Key: "isreplicaset", Value: true}}, extra...)}
}

// AC-56 (twin): ghost, standalone-after-match, unreachable and foreign members.
func TestMemberStates_AC56(t *testing.T) {
	t.Run("ghost", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, nil))
		c.waitReady()
		b := cl.Member(1)
		b.SetHello(ghost())
		n := monitorChecks(b)
		time.Sleep(2500 * time.Millisecond)
		if monitorChecks(b) < n+3 {
			t.Fatalf("the ghost member was checked %d times in 2.5s", monitorChecks(b)-n)
		}
		if code, _ := c.readyz(); code != 200 || len(c.records("upstream incompatible")) != 0 {
			t.Fatalf("readiness %d, records %v", code, incompatReasons(c))
		}
		// B reports the set again and, as primary, becomes the target.
		cl.SetPrimary(1)
		eventually(t, "B to become the target", func() bool {
			for _, r := range c.records("upstream primary changed") {
				if r["server.port"] == float64(mustPort(cl.Addrs()[1])) {
					return true
				}
			}
			return false
		})
	})
	t.Run("standalone and mongos after a match", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, nil))
		c.waitReady()
		cs := cl.Member(2)
		for _, h := range []mongosim.HelloSpec{{Role: mongosim.Standalone}, {Role: mongosim.Mongos}} {
			cs.SetHello(h)
			n := monitorChecks(cs)
			time.Sleep(1800 * time.Millisecond)
			if monitorChecks(cs) <= n {
				t.Fatal("C is not re-checked")
			}
			if code, _ := c.readyz(); code != 200 || len(c.records("upstream incompatible")) != 0 {
				t.Fatalf("readiness %d, records %v", code, incompatReasons(c))
			}
		}
	})
	t.Run("unreachable then used again", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, nil))
		c.waitReady()
		cs := cl.Member(2)
		cs.Stop()
		time.Sleep(1500 * time.Millisecond)
		if code, _ := c.readyz(); code != 200 {
			t.Fatalf("readiness %d with a secondary down", code)
		}
		cs.Restart()
		cl.SetPrimary(2)
		eventually(t, "C to become the target", func() bool {
			for _, r := range c.records("upstream primary changed") {
				if r["server.port"] == float64(mustPort(cl.Addrs()[2])) {
					return true
				}
			}
			return false
		})
	})
	t.Run("foreign member rechecked every heartbeat", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, map[string]string{"upstream.heartbeat_interval": "2s"}))
		c.waitReady()
		cl.NoPrimary() // the fast interval applies to the others
		b := cl.Member(1)
		b.SetHello(mongosim.HelloSpec{Role: mongosim.Secondary, SetName: "other", Hosts: []string{"203.0.113.9:27017"}})
		time.Sleep(300 * time.Millisecond)
		nb, nc := monitorChecks(b), monitorChecks(cl.Member(2))
		time.Sleep(6500 * time.Millisecond)
		db, dc := monitorChecks(b)-nb, monitorChecks(cl.Member(2))-nc
		if db > 5 || dc < 2*db {
			t.Fatalf("foreign member checked %d times, the others %d: the foreign rate must stay at the heartbeat", db, dc)
		}
		if len(c.records("upstream incompatible")) != 0 {
			t.Fatalf("incompatible records %v", incompatReasons(c))
		}
	})
	t.Run("all members lost after startup", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, map[string]string{"pool.checkout_timeout": "1500ms"}))
		c.waitReady()
		for i := 0; i < 3; i++ {
			cl.Member(i).Stop()
		}
		c.waitReadyz(503, "upstream_unavailable")
		m := rt(t, c.dial(), find(1, "x"))
		if code(m) != 262 {
			t.Fatalf("a request must wait then get 262, got %v", doc(m))
		}
		if len(c.records("upstream incompatible")) != 0 {
			t.Fatalf("incompatible records %v", incompatReasons(c))
		}
	})
	t.Run("seed rules at startup unchanged", func(t *testing.T) {
		g := mongosim.Start(t, mongosim.Options{Hello: ghost()})
		c := start(t, opts{upstream: []string{g.Addr()}, rs: "rs0"})
		c.waitReadyz(503, "upstream_unavailable")
		time.Sleep(800 * time.Millisecond)
		if len(c.records("upstream incompatible")) != 0 {
			t.Fatal("a ghost seed caused incompatibility")
		}
	})
}

func mustPort(addr string) int {
	n, _ := strconv.Atoi(portOf(addr))
	return n
}

// AC-57 (FR-35): which check failures are retried once.
func TestRetryOnlyTransientTargetChecks_AC57(t *testing.T) {
	// A hello error reply is applied at once, with no retry: the next check of
	// the target is the scheduled one, not an immediate retry.
	t.Run("error reply is not retried", func(t *testing.T) {
		c, tw := startMongos(t, opts{set: map[string]string{"upstream.heartbeat_interval": "2s"}})
		c.waitReady()
		tw.ResetRecording()
		tw.FailChecks(1, mongosim.CheckError(91, "ShutdownInProgress"))
		c.waitReadyz(503, "upstream_unavailable")
		time.Sleep(900 * time.Millisecond)
		var at []time.Time
		for _, m := range tw.Received() {
			if m.AppName == mongosim.MonitorAppName && strings.EqualFold(m.Command, "hello") {
				at = append(at, m.Time)
			}
		}
		if len(at) < 2 {
			t.Fatalf("%d monitor checks recorded", len(at))
		}
		// at[0] is the failing check. A retry would follow at once; an applied
		// error follows only the 500 ms re-check of an unusable upstream.
		if gap := at[1].Sub(at[0]); gap < 400*time.Millisecond {
			t.Fatalf("the check after the failing one came %v later: an error reply must not be retried", gap)
		}
	})
	// A timed-out check of the target is retried at once on a new connection.
	t.Run("timeout is retried", func(t *testing.T) {
		c, tw := startMongos(t, opts{set: map[string]string{"upstream.heartbeat_interval": "2s", "upstream.connect_timeout": "700ms"}})
		c.waitReady()
		attempts := tw.ConnAttemptsBy(mongosim.MonitorAppName)
		tw.FailChecks(1, mongosim.CheckNoReply)
		eventually(t, "a retry on a new monitoring connection", func() bool {
			return tw.ConnAttemptsBy(mongosim.MonitorAppName) > attempts
		})
		time.Sleep(300 * time.Millisecond)
		tw.ReleaseChecks()
		if code, _ := c.readyz(); code != 200 {
			t.Fatalf("readiness %d after a retried timeout", code)
		}
	})
}

// AC-57: writeConcernError counts only in ok:1; in ok:0 only the top-level code.
func TestWriteConcernErrorOnlyInOk1_AC57(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
	c := start(t, rsOpts(cl, nil))
	c.waitReady()
	pri := cl.Member(cl.PrimaryIndex())
	mixed := bsonOf(d("ok", float64(0), "code", int32(11000), "codeName", "DuplicateKey", "errmsg", "dup", "writeConcernError", d("code", int32(91), "errmsg", "x")))
	pri.AddRule(mongosim.Match{Marker: "ok0"}, mongosim.ReplyRaw(append([]byte{0}, mixed...)))
	m := rt(t, c.dial(), mk(1, "ok0", "insert", "c", "$db", "d"))
	if code(m) != 11000 {
		t.Fatalf("%v", doc(m))
	}
	time.Sleep(800 * time.Millisecond)
	if n := len(c.records("upstream target marked not primary")); n != 0 {
		t.Fatalf("an ok:0 reply with writeConcernError 91 marked the host (%d records)", n)
	}
	pri.AddRule(mongosim.Match{Marker: "ok1", Once: true}, mongosim.ReplyWriteConcernError(91))
	rt(t, c.dial(), mk(2, "ok1", "insert", "c", "$db", "d"))
	c.waitRecords("upstream target marked not primary", 1)
}

// AC-57: when B becomes the target, its next check runs at once, authenticates
// on B's monitoring connection, and before that its checks carried no auth.
func TestAuthenticationOnTargetChange_AC57(t *testing.T) {
	a := newAuth(t)
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{Users: a.users(), RequireAuth: true})
	c := start(t, opts{upstream: cl.Addrs(), rs: "rs0", env: a.env, set: map[string]string{"upstream.auth.mechanism": "SCRAM-SHA-256", "upstream.heartbeat_interval": "5s"}})
	c.waitReady()
	b := cl.Member(1)
	if n := len(b.ReceivedCommand("saslStart")); n != 0 {
		t.Fatalf("B received %d authentications before it became the target", n)
	}
	cl.SetPrimary(1)
	eventually(t, "B's monitoring connection to authenticate", func() bool { return len(b.ReceivedCommand("saslStart")) >= 1 })
	eventually(t, "an authenticated monitoring connection on B", func() bool {
		for _, ci := range b.Conns() {
			if ci.AppName == mongosim.MonitorAppName && ci.Authenticated {
				return true
			}
		}
		return false
	})
}
