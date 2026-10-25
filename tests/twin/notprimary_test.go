//go:build twin

package twin

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-33 (FR-35, twin, replica-set mode): a not-primary reply is forwarded
// unchanged and marks the host; idle connections close as not_primary; ok:0
// code 11000 does not mark.
func TestNotPrimaryMarking_AC33(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  mongosim.Action
		code int64
	}{
		{"ok:0 10107", mongosim.ReplyError(10107, "NotWritablePrimary"), 10107},
		{"ok:1 writeConcernError 91", mongosim.ReplyWriteConcernError(91), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
			c := start(t, rsOpts(cl, map[string]string{"pool.min_size": "2", "upstream.heartbeat_interval": "5s"}))
			c.waitReady()
			pri := cl.Member(cl.PrimaryIndex())
			waitIdlePool(t, c, pri, 2)
			pri.AddRule(mongosim.Match{Marker: "np", Once: true}, tc.act)
			checks := monitorChecks(pri)
			closed := c.metric("corral_upstream_connections_closed_total", "kind", "pool", "reason", "not_primary")
			m := rt(t, c.dial(), mk(1, "np", "insert", "c", "$db", "d"))
			if got := code(m); got != tc.code {
				t.Fatalf("reply code %d, want %d (forwarded unchanged): %v", got, tc.code, doc(m))
			}
			recs := c.waitRecords("upstream target marked not primary", 1)
			if recs[0]["error"] != "server" || recs[0]["corral.error.detail"] != "other" {
				t.Fatalf("record %v", recs[0])
			}
			c.waitDelta(closed, 2, "corral_upstream_connections_closed_total", "kind", "pool", "reason", "not_primary")
			eventually(t, "an immediate check", func() bool { return monitorChecks(pri) > checks })
			// The host is used again once a check reports a primary.
			c.waitReady()
			if m := rt(t, c.dial(), find(2, "again")); !isOK(m) {
				t.Fatalf("%v", doc(m))
			}
		})
	}
	t.Run("ok:0 11000 does not mark", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, nil))
		c.waitReady()
		cl.Member(cl.PrimaryIndex()).AddRule(mongosim.Match{Marker: "dup"}, mongosim.ReplyError(11000, "DuplicateKey"))
		if m := rt(t, c.dial(), mk(1, "dup", "insert", "c", "$db", "d")); code(m) != 11000 {
			t.Fatalf("%v", doc(m))
		}
		time.Sleep(800 * time.Millisecond)
		if n := len(c.records("upstream target marked not primary")); n != 0 {
			t.Fatalf("%d marking records", n)
		}
	})
}

// AC-43 (FR-35, twin, single-mongos mode): the not-primary family does not mark
// a mongos except top-level 91 and 11600; in replica-set mode a code inside
// writeErrors[] does not mark the host.
func TestMongosMarkingRule_AC43(t *testing.T) {
	for _, tc := range []struct {
		name   string
		act    mongosim.Action
		marked bool
	}{
		{"10107", mongosim.ReplyError(10107, "NotWritablePrimary"), false},
		{"13435", mongosim.ReplyError(13435, "NotPrimaryNoSecondaryOk"), false},
		{"189", mongosim.ReplyError(189, "PrimarySteppedDown"), false},
		{"11602", mongosim.ReplyError(11602, "InterruptedDueToReplStateChange"), false},
		{"ok:1 writeConcernError 91", mongosim.ReplyWriteConcernError(91), false},
		{"ok:0 91", mongosim.ReplyError(91, "ShutdownInProgress"), true},
		{"ok:0 11600", mongosim.ReplyError(11600, "InterruptedAtShutdown"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, tw := startMongos(t, opts{})
			c.waitReady()
			tw.AddRule(mongosim.Match{Marker: "x", Once: true}, tc.act)
			m := rt(t, c.dial(), mk(1, "x", "insert", "c", "$db", "d"))
			if isOK(m) && !tc.marked {
				if _, err := doc(m).LookupErr("writeConcernError"); err != nil {
					t.Fatalf("not forwarded unchanged: %v", doc(m))
				}
			}
			if tc.marked {
				c.waitRecords("upstream target marked not primary", 1)
				return
			}
			time.Sleep(800 * time.Millisecond)
			if n := len(c.records("upstream target marked not primary")); n != 0 {
				t.Fatalf("the mongos was marked (%d records)", n)
			}
			if code, _ := c.readyz(); code != 200 {
				t.Fatalf("readiness %d", code)
			}
		})
	}
	t.Run("replica set: writeErrors 10107 does not mark", func(t *testing.T) {
		cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
		c := start(t, rsOpts(cl, nil))
		c.waitReady()
		cl.Member(cl.PrimaryIndex()).AddRule(mongosim.Match{Marker: "we", Once: true}, mongosim.ReplyWriteErrors(10107))
		m := rt(t, c.dial(), mk(1, "we", "insert", "c", "$db", "d", "documents", bson.A{d("_id", 1)}))
		if !isOK(m) {
			t.Fatalf("%v", doc(m))
		}
		time.Sleep(800 * time.Millisecond)
		if n := len(c.records("upstream target marked not primary")); n != 0 {
			t.Fatalf("%d marking records", n)
		}
	})
}
