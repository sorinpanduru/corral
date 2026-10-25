package exchange_test

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func insertMTC(reqID int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: reqID, Flags: flagMore,
		Body:      bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "shop"}, {Key: "corraltestMarker", Value: marker}},
		Sequences: []rawwire.Sequence{{ID: "documents", Docs: []bson.D{{{Key: "_id", Value: 1}}}}}})
}

func findM(reqID int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: reqID, Body: find("c", bson.E{Key: "corraltestMarker", Value: marker})})
}

func noMsg(t *testing.T, c *rawwire.Conn, wait time.Duration) {
	t.Helper()
	if m, err := c.ReadMessage(wait); err == nil {
		t.Fatalf("an unexpected reply arrived: opcode %d", m.Header.OpCode)
	}
}

// AC-14, FR-29: a moreToCome request gets no reply; the barrier ping goes on
// the same upstream connection right after it; the lease returns after the
// ping reply.
func TestBarrierPingSameConnection_AC14(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	if err := c.Send(insertMTC(5, "mtc-1")); err != nil {
		t.Fatal(err)
	}
	noMsg(t, c, 300*time.Millisecond)
	ins := lastReceived(t, r, "insert")
	var pg mongosim.Message
	eventually(t, "the barrier ping", func() bool {
		ms := r.twin.ReceivedCommand("ping")
		if len(ms) > 0 {
			pg = ms[0]
		}
		return len(ms) > 0
	})
	if pg.ConnIndex != ins.ConnIndex || pg.Seq <= ins.Seq {
		t.Fatalf("ping on conn %d seq %d, insert on conn %d seq %d: the ping must follow on the same connection", pg.ConnIndex, pg.Seq, ins.ConnIndex, ins.Seq)
	}
	if pg.DB != "admin" || pg.HasLsid {
		t.Fatalf("ping db=%q lsid=%v", pg.DB, pg.HasLsid)
	}
	eventually(t, "the lease to return", func() bool { return r.p.Stats().InUse == 0 && r.p.Stats().Idle == 1 })
	// The next message is served on the same connection.
	roundTrip(t, c, findM(6, "after"))
	if r.twin.ConnAttemptsBy("corral") != 1 {
		t.Fatalf("pool connections = %d, want the same one reused", r.twin.ConnAttemptsBy("corral"))
	}
	res := r.waitResults(2)
	if res[0].Command != "insert" || res[0].Outcome != 0 /* OK */ {
		t.Logf("first result: %+v", res[0])
	}
}

// AC-14: the connection is not released, and the client's next message is not
// read, while the ping is held.
func TestBarrierHoldsConnectionAndOrdering_AC14(t *testing.T) {
	r := newRig(t, opts{})
	h := r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	c := r.dial()
	c.Send(append(insertMTC(1, "a"), findM(2, "b")...))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("the ping never arrived")
	}
	time.Sleep(200 * time.Millisecond)
	if r.p.Stats().InUse != 1 {
		t.Fatalf("lease released while the ping is held: %+v", r.p.Stats())
	}
	if n := len(r.twin.ReceivedCommand("find")); n != 0 {
		t.Fatalf("the client's next message was forwarded (%d find) while the ping is held", n)
	}
	h.Release()
	m, err := c.ReadMessage(10 * time.Second)
	if err != nil || m.Header.ResponseTo != 2 {
		t.Fatalf("next message not answered after the ping: %v", err)
	}
}

// FR-29: a ping reset closes the upstream connection (reason error) but keeps
// the client connection open.
func TestBarrierResetKeepsClient_AC14(t *testing.T) {
	r := newRig(t, opts{})
	r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.ResetAfterReceive())
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	eventually(t, "upstream discarded", func() bool { return contains(r.pobs.reasons(), "error") })
	// The client connection is still open: a find works (on a new connection).
	m := roundTrip(t, c, findM(2, "y"))
	if m.Header.ResponseTo != 2 {
		t.Fatal("client connection broken by a barrier failure")
	}
}

// Amendment 1 (G-12), AC-14: a client that disconnects during the ping does
// not abort it. The barrier completes, the connection returns to the pool, and
// the next lease reuses it.
func TestBarrierSurvivesClientDisconnect_AC42(t *testing.T) {
	r := newRig(t, opts{})
	h := r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("ping not held")
	}
	c.Close()
	time.Sleep(300 * time.Millisecond)
	if r.p.Stats().InUse != 1 {
		t.Fatalf("the client's disconnect aborted the barrier: %+v", r.p.Stats())
	}
	if contains(r.pobs.reasons(), "client_abandoned") {
		t.Fatal("the connection was discarded as client_abandoned")
	}
	h.Release()
	eventually(t, "connection returned to the pool", func() bool { return r.p.Stats().Idle == 1 && r.p.Stats().InUse == 0 })
	// The next client reuses it.
	m := roundTrip(t, r.dial(), findM(1, "next"))
	if m.Header.ResponseTo != 1 || r.twin.ConnAttemptsBy("corral") != 1 {
		t.Fatalf("connection not reused (attempts %d)", r.twin.ConnAttemptsBy("corral"))
	}
	if contains(r.pobs.reasons(), "client_abandoned") {
		t.Fatal("client_abandoned reported")
	}
}

// FR-29: the barrier has no timeout of its own: a ping delayed three times the
// I/O and connect timeouts still completes.
func TestBarrierHasNoTimeout_AC14(t *testing.T) {
	r := newRig(t, opts{ioTimeout: 250 * time.Millisecond, connectTO: 250 * time.Millisecond})
	r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Delay(900*time.Millisecond))
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	time.Sleep(1200 * time.Millisecond)
	eventually(t, "the barrier to complete", func() bool { return r.p.Stats().Idle == 1 && r.p.Stats().InUse == 0 })
	if contains(r.pobs.reasons(), "error") {
		t.Fatal("the delayed ping failed the barrier")
	}
	roundTrip(t, c, findM(2, "later"))
}

// FR-29, FR-35: an abort through Lease.Aborted (the host is marked) discards
// the upstream connection and keeps the client open.
func TestBarrierAbort_AC14(t *testing.T) {
	r := newRig(t, opts{})
	h := r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	if !h.WaitHeld(1, 5*time.Second) {
		t.Fatal("ping not held")
	}
	r.p.AbortBarriers(r.twin.Addr())
	eventually(t, "the connection discarded", func() bool { return contains(r.pobs.reasons(), "error") })
	if st := r.p.Stats(); st.InUse != 0 || st.Idle != 0 {
		t.Fatalf("stats after abort: %+v", st)
	}
	h.Release()
	m := roundTrip(t, c, findM(2, "after"))
	if m.Header.ResponseTo != 2 {
		t.Fatal("client broken by the abort")
	}
}

// AC-42 (unit): a ping reply with 10107 or 91 in replica-set mode fails the
// barrier (connection discarded) and goes to ReplyCodes with the Barrier
// source.
func TestBarrierPingMarkingReplicaSet_AC42(t *testing.T) {
	for _, tc := range []struct {
		code int32
		name string
	}{{10107, "NotWritablePrimary"}, {91, "ShutdownInProgress"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, opts{rule: replicaRule})
			r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.ReplyError(tc.code, tc.name))
			c := r.dial()
			c.Send(insertMTC(1, "x"))
			eventually(t, "the connection discarded", func() bool { return contains(r.pobs.reasons(), "error") })
			var call *rcall
			eventually(t, "ReplyCodes for the barrier", func() bool {
				for _, k := range r.codes.all() {
					if k.Src == upstream.Barrier {
						k := k
						call = &k
						return true
					}
				}
				return false
			})
			if call.Host != r.twin.Addr() || !call.Info.HasTop || !call.Info.OKZero || call.Info.Top != tc.code || call.Info.TopName != tc.name {
				t.Fatalf("ReplyCodes call: %+v", call)
			}
			if call.Info.HasWCE {
				t.Fatal("a barrier call carries a writeConcernError")
			}
			// The client connection stays open.
			if m := roundTrip(t, c, findM(2, "y")); m.Header.ResponseTo != 2 {
				t.Fatal("client broken")
			}
		})
	}
}

// AC-42 (unit): in single-mongos mode a 10107 ping reply does not mark the
// target, and the barrier ... still ends with the connection decided by the
// ok:0 reply (an error reply fails the barrier); 91 marks and fails.
func TestBarrierPingMongosMode_AC42(t *testing.T) {
	r := newRig(t, opts{rule: mongosRule})
	r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.ReplyError(10107, "NotWritablePrimary"))
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	eventually(t, "ReplyCodes called", func() bool {
		for _, k := range r.codes.all() {
			if k.Src == upstream.Barrier {
				return true
			}
		}
		return false
	})
	for _, k := range r.codes.all() {
		if k.Src == upstream.Barrier && mongosRule(k.Src, k.Info) {
			t.Fatal("10107 would mark a mongos")
		}
	}
	// An ok:0 reply without a marking code still fails the barrier.
	eventually(t, "upstream connection discarded", func() bool { return contains(r.pobs.reasons(), "error") })
}

// FR-29 (Amendment 1): any ok:0 ping reply fails the barrier, marking or not.
func TestBarrierOkZeroFails_AC42(t *testing.T) {
	r := newRig(t, opts{rule: replicaRule})
	r.twin.AddRule(mongosim.Match{Command: "ping"}, mongosim.ReplyError(13, "Unauthorized"))
	c := r.dial()
	c.Send(insertMTC(1, "x"))
	eventually(t, "connection discarded", func() bool { return contains(r.pobs.reasons(), "error") })
	for _, k := range r.codes.all() {
		if k.Src == upstream.Barrier && replicaRule(k.Src, k.Info) {
			t.Fatal("code 13 must not mark")
		}
	}
}

// AC-36: moreToCome messages refused locally get no reply and the connection
// stays usable; they count as dropped.
func TestMoreToComeRefusalsNoReply_AC36(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	mtc := func(id int32, body bson.D) []byte {
		return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Flags: flagMore, Body: body})
	}
	refused := []bson.D{
		{{Key: "saslStart", Value: 1}, {Key: "$db", Value: "admin"}},
		{{Key: "currentOp", Value: 1}, {Key: "$db", Value: "admin"}},
		{{Key: "logout", Value: 1}, {Key: "$db", Value: "admin"}},
	}
	for i, b := range refused {
		c.Send(mtc(int32(10+i), b))
		noMsg(t, c, 150*time.Millisecond)
	}
	// A handshake with moreToCome: answered locally, no reply.
	c.Send(mtc(20, bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}))
	noMsg(t, c, 150*time.Millisecond)
	// The connection still answers an ordinary ping.
	if m := roundTrip(t, c, ping(30)); m.Header.ResponseTo != 30 {
		t.Fatal("ping not answered after the refusals")
	}
	res := r.waitResults(5)
	dropped := 0
	for _, x := range res {
		if x.Dropped {
			dropped++
		}
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (the refusals; the handshake is answered locally)", dropped)
	}
	// Nothing reached the upstream except the ping.
	if len(r.twin.ReceivedCommand("saslStart"))+len(r.twin.ReceivedCommand("currentOp"))+len(r.twin.ReceivedCommand("logout")) != 0 {
		t.Fatal("a refused command was forwarded")
	}
}

var _ = pool.CloseReason("")
