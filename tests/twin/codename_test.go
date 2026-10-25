//go:build twin

package twin

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-62 (FR-35, FR-9, FR-29, FR-49; Amendment 6), twin, replica-set mode. Each
// test starts its own corral, so each has a fresh rate limiter. The code name in
// the "upstream target marked not primary" record comes from the fixed table,
// never from the reply; the reply itself reaches the client unchanged.

func markedRecord(t *testing.T, c *corral) map[string]any {
	t.Helper()
	return c.waitRecords("upstream target marked not primary", 1)[0]
}

func rawReply(d bson.D) []byte { return append([]byte{0}, bsonOf(d)...) }

// An ok:1 reply with writeConcernError 91 and a different codeName (a marker),
// and the same with no codeName at all.
func TestWriteConcernErrorCodeNameFromTable_AC62(t *testing.T) {
	for name, wce := range map[string]func(marker string) bson.D{
		"with a marker codeName": func(m string) bson.D { return d("code", int32(91), "codeName", m, "errmsg", "synthetic") },
		"with no codeName":       func(string) bson.D { return d("code", int32(91), "errmsg", "synthetic") },
	} {
		t.Run(name, func(t *testing.T) {
			marker := "mk-codename-" + randHex(t, 8)
			cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
			c := start(t, rsOpts(cl, nil))
			c.waitReady()
			reply := rawReply(d("ok", float64(1), "n", int32(1), "writeConcernError", wce(marker)))
			cl.Member(cl.PrimaryIndex()).AddRule(mongosim.Match{Marker: "wce", Once: true}, mongosim.ReplyRaw(reply))
			m := rt(t, c.dial(), mk(1, "wce", "insert", "c", "$db", "d"))
			r := markedRecord(t, c)
			if r["db.response.status_code"] != "91" || r["corral.db.response.code_name"] != "ShutdownInProgress" {
				t.Fatalf("record %v", r)
			}
			// The reply reaches the client byte-identical (FR-10).
			if !bytes.Equal(sections(m.Raw, m.Flags), reply) {
				t.Fatal("the forwarded reply differs from the twin's")
			}
			// The marker (a codeName of the reply) appears in no output.
			sc := markerscan.New()
			sc.AddMarker(t, marker, markerscan.SourceStdout, markerscan.SourceStderr, markerscan.SourceMetrics)
			sc.Add(markerscan.SourceStdout, c.p.Stdout())
			sc.Add(markerscan.SourceStderr, c.p.Stderr())
			if txt, err := c.p.Metrics(); err == nil {
				sc.Add(markerscan.SourceMetrics, []byte(txt))
			}
			sc.AssertClean(t)
		})
	}
}

// An ok:0 reply with code 10107 and the legacy codeName NotMaster is recorded as
// NotWritablePrimary, and the reply reaches the client unchanged.
func TestLegacyCodeNameMapped_AC62(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
	c := start(t, rsOpts(cl, nil))
	c.waitReady()
	reply := rawReply(d("ok", float64(0), "code", int32(10107), "codeName", "NotMaster", "errmsg", "not master"))
	cl.Member(cl.PrimaryIndex()).AddRule(mongosim.Match{Marker: "nm", Once: true}, mongosim.ReplyRaw(reply))
	m := rt(t, c.dial(), mk(1, "nm", "insert", "c", "$db", "d"))
	if r := markedRecord(t, c); r["db.response.status_code"] != "10107" || r["corral.db.response.code_name"] != "NotWritablePrimary" {
		t.Fatalf("record %v", r)
	}
	if !bytes.Equal(sections(m.Raw, m.Flags), reply) {
		t.Fatal("the forwarded ok:0 reply differs from the twin's")
	}
}

// A barrier ping reply with code 91 and a different codeName is recorded as
// ShutdownInProgress.
func TestBarrierPingCodeNameFromTable_AC62(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{})
	c := start(t, rsOpts(cl, nil))
	c.waitReady()
	reply := rawReply(d("ok", float64(0), "code", int32(91), "codeName", "SomeOtherName", "errmsg", "shutting down"))
	cl.Member(cl.PrimaryIndex()).AddRule(mongosim.Match{Command: "ping", AppName: "corral", Once: true}, mongosim.ReplyRaw(reply))
	a := c.dial()
	a.Send(mtcInsert(1, "mtc-codename"))
	if r := markedRecord(t, c); r["db.response.status_code"] != "91" || r["corral.db.response.code_name"] != "ShutdownInProgress" {
		t.Fatalf("record %v", r)
	}
}
