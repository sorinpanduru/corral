package mongosim_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

// ---- raw test client (independent framing) ----

type raw struct {
	t  *testing.T
	nc net.Conn
	id int32
	// short, when set, is the read timeout for the next read only.
	short time.Duration
}

func dial(t *testing.T, addr string) *raw {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &raw{t: t, nc: nc}
}

func frame(op int32, id, to int32, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint32(b[4:], uint32(id))
	binary.LittleEndian.PutUint32(b[8:], uint32(to))
	binary.LittleEndian.PutUint32(b[12:], uint32(op))
	copy(b[16:], payload)
	return b
}

func (r *raw) sendMsg(flags uint32, doc bson.D) int32 {
	r.id++
	d, _ := bson.Marshal(doc)
	p := binary.LittleEndian.AppendUint32(nil, flags)
	p = append(p, 0)
	p = append(p, d...)
	if _, err := r.nc.Write(frame(mongosim.OpMsg, r.id, 0, p)); err != nil {
		r.t.Fatal(err)
	}
	return r.id
}

type reply struct {
	hdr   []byte
	whole []byte
	doc   bson.Raw
	flags uint32
}

func (r *raw) read() (*reply, error) {
	to := 3 * time.Second
	if r.short > 0 {
		to, r.short = r.short, 0
	}
	r.nc.SetReadDeadline(time.Now().Add(to))
	var l [4]byte
	if _, err := io.ReadFull(r.nc, l[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(l[:]))
	if n > 64<<20 || n < 16 {
		return &reply{whole: l[:]}, nil
	}
	b := make([]byte, n)
	copy(b, l[:])
	if _, err := io.ReadFull(r.nc, b[4:]); err != nil {
		return nil, err
	}
	rp := &reply{hdr: b[:16], whole: b}
	switch int32(binary.LittleEndian.Uint32(b[12:])) {
	case mongosim.OpMsg:
		rp.flags = binary.LittleEndian.Uint32(b[16:])
		end := n
		if rp.flags&1 != 0 {
			end -= 4
		}
		rp.doc = bson.Raw(b[21:end])
	case mongosim.OpReply:
		rp.doc = bson.Raw(b[36:])
	}
	return rp, nil
}

func (r *raw) cmd(doc bson.D) *reply {
	r.t.Helper()
	r.sendMsg(0, doc)
	rp, err := r.read()
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	return rp
}

func (rp *reply) responseTo() int32 { return int32(binary.LittleEndian.Uint32(rp.hdr[8:])) }

func isClosed(r *raw) bool {
	r.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := r.read()
	if err == nil {
		return false
	}
	ne, ok := err.(net.Error)
	return !(ok && ne.Timeout())
}

func code(rp *reply) int32 { v, _ := rp.doc.Lookup("code").AsInt64OK(); return int32(v) }

func helloCmd(app string) bson.D {
	d := bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}
	if app != "" {
		d = append(d, bson.E{Key: "client", Value: bson.D{
			{Key: "application", Value: bson.D{{Key: "name", Value: app}}},
			{Key: "driver", Value: bson.D{{Key: "name", Value: "corral"}, {Key: "version", Value: "0.0.0-test"}}},
		}})
	}
	return d
}

// ---- tests ----

func TestPingEchoAndRecording(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	rp := c.cmd(helloCmd("corral"))
	if !rp.doc.Lookup("isWritablePrimary").Boolean() {
		t.Fatal("standalone should be writable")
	}
	if _, err := rp.doc.LookupErr("setName"); err == nil {
		t.Fatal("standalone has no setName")
	}
	if rp := c.cmd(bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}); rp.doc.Lookup("ok").AsFloat64() != 1 {
		t.Fatal("ping")
	}
	rp = c.cmd(bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "t"}, {Key: "corraltestMarker", Value: "m1"},
		{Key: "lsid", Value: bson.D{{Key: "id", Value: bson.Binary{Subtype: 4, Data: make([]byte, 16)}}}}, {Key: "txnNumber", Value: int64(1)}})
	if rp.doc.Lookup("corraltestEcho").StringValue() != "m1" || rp.doc.Lookup("n").Int32() != 1 {
		t.Fatalf("echo: %s", rp.doc)
	}
	if rp.responseTo() != c.id {
		t.Fatal("responseTo")
	}
	msgs := s.Received()
	if len(msgs) != 3 {
		t.Fatalf("recorded %d", len(msgs))
	}
	m := msgs[2]
	if m.Command != "insert" || m.DB != "t" || !m.HasLsid || !m.HasTxnNumber || m.Marker != "m1" || m.AppName != "corral" || m.ConnIndex != 0 || m.OpCode != mongosim.OpMsg {
		t.Fatalf("bad record %+v", m)
	}
	if len(m.Raw) == 0 || m.Time.IsZero() {
		t.Fatal("raw/time")
	}
	ci := s.Conns()[0]
	if ci.AppName != "corral" || ci.DriverName != "corral" || ci.DriverVersion != "0.0.0-test" || !ci.Open {
		t.Fatalf("%+v", ci)
	}
	if s.ConnAttempts() != 1 || s.ConnAttemptsBy("corral") != 1 || s.ConnAttemptsBy("corral-monitor") != 0 || s.OpenConns("corral") != 1 {
		t.Fatal("counters")
	}
	c.nc.Close()
	if !poll(func() bool { return s.OpenConns("corral") == 0 }) {
		t.Fatal("conn did not close")
	}
}

func poll(f func() bool) bool {
	for i := 0; i < 500; i++ {
		if f() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestOpQueryHello(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	c := dial(t, s.Addr())
	d, _ := bson.Marshal(bson.D{{Key: "ismaster", Value: 1}, {Key: "helloOk", Value: true}})
	p := binary.LittleEndian.AppendUint32(nil, 0)
	p = append(p, "admin.$cmd\x00"...)
	p = binary.LittleEndian.AppendUint32(p, 0)
	p = binary.LittleEndian.AppendUint32(p, 0xffffffff)
	p = append(p, d...)
	c.nc.Write(frame(mongosim.OpQuery, 7, 0, p))
	rp, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if int32(binary.LittleEndian.Uint32(rp.hdr[12:])) != mongosim.OpReply || rp.responseTo() != 7 {
		t.Fatal("expected OP_REPLY to 7")
	}
	if rp.doc.Lookup("msg").StringValue() != "isdbgrid" || !rp.doc.Lookup("helloOk").Boolean() {
		t.Fatalf("%s", rp.doc)
	}
	m := s.Received()[0]
	if m.OpCode != mongosim.OpQuery || m.Command != "ismaster" || m.DB != "admin" {
		t.Fatalf("%+v", m)
	}
}

func TestSetHelloLiveAndAwaitable(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	rp := c.cmd(helloCmd(""))
	tv := rp.doc.Lookup("topologyVersion").Document()
	pid := tv.Lookup("processId").ObjectID()
	cnt := tv.Lookup("counter").Int64()

	// Awaitable: blocks until SetHello.
	done := make(chan *reply, 1)
	c2 := dial(t, s.Addr())
	go func() {
		c2.sendMsg(0, bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}, {Key: "maxAwaitTimeMS", Value: 10000},
			{Key: "topologyVersion", Value: bson.D{{Key: "processId", Value: pid}, {Key: "counter", Value: cnt}}}})
		r, _ := c2.read()
		done <- r
	}()
	select {
	case <-done:
		t.Fatal("awaitable hello returned early")
	case <-time.After(150 * time.Millisecond):
	}
	eid := bytes.Repeat([]byte{1}, 12)
	s.SetHello(mongosim.HelloSpec{Role: mongosim.Primary, SetName: "rs9", Hosts: []string{"a:1", "b:2"}, Passives: []string{"p:3"},
		Arbiters: []string{"x:4"}, Primary: "a:1", ElectionID: eid, SetVersion: 5, MinWire: 3, MaxWire: 9, MaxMessageSizeBytes: 1000})
	select {
	case r := <-done:
		if r.doc.Lookup("topologyVersion", "counter").Int64() != cnt+1 || r.doc.Lookup("setName").StringValue() != "rs9" {
			t.Fatalf("%s", r.doc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitable hello not woken")
	}
	rp = c.cmd(helloCmd(""))
	d := rp.doc
	if d.Lookup("setVersion").Int32() != 5 || d.Lookup("minWireVersion").Int32() != 3 || d.Lookup("maxWireVersion").Int32() != 9 ||
		d.Lookup("maxMessageSizeBytes").Int32() != 1000 || d.Lookup("primary").StringValue() != "a:1" ||
		d.Lookup("me").StringValue() != s.Addr() || arrLen(d.Lookup("passives")) == 0 {
		t.Fatalf("%s", d)
	}
	if _, ok := d.Lookup("electionId").ObjectIDOK(); !ok {
		t.Fatal("electionId")
	}
	// Awaitable with a stale counter answers at once.
	start := time.Now()
	c.cmd(bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}, {Key: "maxAwaitTimeMS", Value: 10000},
		{Key: "topologyVersion", Value: bson.D{{Key: "processId", Value: pid}, {Key: "counter", Value: cnt}}}})
	if time.Since(start) > time.Second {
		t.Fatal("stale awaitable should answer at once")
	}
}

func TestRolesShape(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	for role, want := range map[mongosim.Role][3]bool{ // writable, secondary, arbiterOnly
		mongosim.Primary: {true, false, false}, mongosim.Secondary: {false, true, false},
		mongosim.Arbiter: {false, false, true}, mongosim.Other: {false, false, false},
	} {
		s.SetHello(mongosim.HelloSpec{Role: role, SetName: "rs"})
		d := c.cmd(helloCmd("")).doc
		got := [3]bool{d.Lookup("isWritablePrimary").Boolean(), d.Lookup("secondary").Boolean(), false}
		if v, ok := d.Lookup("arbiterOnly").BooleanOK(); ok {
			got[2] = v
		}
		if got != want {
			t.Fatalf("role %v: got %v want %v", role, got, want)
		}
	}
}

func TestFailChecks(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	// Reset on monitor only.
	s.FailChecks(1, mongosim.CheckReset)
	app := dial(t, s.Addr())
	if app.cmd(helloCmd("corral")).doc.Lookup("ok").AsFloat64() != 1 {
		t.Fatal("non-monitor connection must not be affected")
	}
	mon := dial(t, s.Addr())
	mon.sendMsg(0, helloCmd("corral-monitor"))
	if _, err := mon.read(); err == nil {
		t.Fatal("expected reset")
	}
	// Consumed: next monitor check succeeds.
	mon2 := dial(t, s.Addr())
	if mon2.cmd(helloCmd("corral-monitor")).doc.Lookup("ok").AsFloat64() != 1 {
		t.Fatal("fault should be consumed")
	}

	// NoReply until released.
	s.FailChecks(1, mongosim.CheckNoReply)
	m3 := dial(t, s.Addr())
	m3.sendMsg(0, helloCmd("corral-monitor"))
	m3.short = 150 * time.Millisecond
	if _, err := m3.read(); err == nil {
		t.Fatal("expected no reply")
	}
	s.ReleaseChecks()
	if rp, err := m3.read(); err != nil || rp.doc.Lookup("ok").AsFloat64() != 1 {
		t.Fatalf("expected reply after release: %v", err)
	}

	// Delay, error, malformed on all connections.
	s.FailChecksAll(1, mongosim.CheckDelay(200*time.Millisecond))
	st := time.Now()
	app.cmd(helloCmd("corral"))
	if time.Since(st) < 190*time.Millisecond {
		t.Fatal("delay not applied")
	}
	s.FailChecksAll(1, mongosim.CheckError(91, "ShutdownInProgress"))
	if rp := app.cmd(helloCmd("corral")); code(rp) != 91 {
		t.Fatalf("%s", rp.doc)
	}
	s.FailChecksAll(1, mongosim.CheckMalformed)
	rp := app.cmd(helloCmd("corral"))
	if rp.doc.Validate() == nil {
		t.Fatal("expected malformed body")
	}
}

func TestLifecycle(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	addr := s.Addr()
	c := dial(t, addr)
	c.cmd(helloCmd("a"))
	s.Stop()
	if !isClosed(c) {
		t.Fatal("conn should be closed by Stop")
	}
	if nc, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		nc.Close()
		t.Fatal("address should be unreachable")
	}
	s.Restart()
	if s.Addr() != addr {
		t.Fatal("address changed")
	}
	c = dial(t, addr)
	c.cmd(helloCmd("a"))

	s.RefuseNew(true)
	r := dial(t, addr)
	if !isClosed(r) {
		t.Fatal("refused conn should close")
	}
	s.RefuseNew(false)
	s.Blackhole(true)
	b := dial(t, addr)
	b.sendMsg(0, helloCmd("b"))
	b.short = 200 * time.Millisecond
	if _, err := b.read(); err == nil {
		t.Fatal("blackholed conn answered")
	}
	s.Blackhole(false)
	if n := s.ConnAttempts(); n != 4 {
		t.Fatalf("attempts %d", n)
	}
	s.CloseConns()
	if !isClosed(c) {
		t.Fatal("CloseConns")
	}
	// Stop is idempotent and cleanup-safe.
	s.Stop()
	s.Stop()
}

func TestRules(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	c.cmd(helloCmd("corral"))
	ping := bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}
	ins := func(marker string) bson.D {
		return bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "t"}, {Key: "corraltestMarker", Value: marker}}
	}

	t.Run("hold and release", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Command: "insert", Marker: "hold"}, mongosim.Hold())
		c.sendMsg(0, ins("hold"))
		if !h.WaitHeld(1, 2*time.Second) {
			t.Fatal("not held")
		}
		c.short = 100 * time.Millisecond
		if _, err := c.read(); err == nil {
			t.Fatal("answered while held")
		}
		h.Release()
		rp, err := c.read()
		if err != nil || rp.doc.Lookup("ok").AsFloat64() != 1 {
			t.Fatalf("after release: %v", err)
		}
		if h.Hits() != 1 || h.Held() != 0 {
			t.Fatal("counters")
		}
		h.Remove()
	})
	t.Run("delay once on ping", func(t *testing.T) {
		s.AddRule(mongosim.Match{Command: "ping", Once: true}, mongosim.Delay(200*time.Millisecond))
		st := time.Now()
		c.cmd(ping)
		if time.Since(st) < 190*time.Millisecond {
			t.Fatal("not delayed")
		}
		st = time.Now()
		c.cmd(ping)
		if time.Since(st) > 150*time.Millisecond {
			t.Fatal("once rule applied twice")
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			c    int32
			name string
		}{{10107, "NotWritablePrimary"}, {13435, "NotPrimaryNoSecondaryOk"}, {11602, "InterruptedDueToReplStateChange"},
			{13436, "NotPrimaryOrSecondary"}, {189, "PrimarySteppedDown"}, {91, "ShutdownInProgress"}, {11600, "InterruptedAtShutdown"}} {
			h := s.AddRule(mongosim.Match{Command: "insert", Marker: "err"}, mongosim.ReplyError(tc.c, tc.name))
			rp := c.cmd(ins("err"))
			if rp.doc.Lookup("ok").AsFloat64() != 0 || code(rp) != tc.c || rp.doc.Lookup("codeName").StringValue() != tc.name ||
				rp.doc.Lookup("errmsg").StringValue() == "" {
				t.Fatalf("%s", rp.doc)
			}
			h.Remove()
		}
		h := s.AddRule(mongosim.Match{Marker: "legacy"}, mongosim.ReplyErrorMessage("not master"))
		rp := c.cmd(ins("legacy"))
		if _, err := rp.doc.LookupErr("code"); err == nil || rp.doc.Lookup("errmsg").StringValue() != "not master" {
			t.Fatalf("%s", rp.doc)
		}
		h.Remove()
	})
	t.Run("writeConcernError", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "wce"}, mongosim.ReplyWriteConcernError(64))
		rp := c.cmd(ins("wce"))
		if rp.doc.Lookup("ok").AsFloat64() != 1 || rp.doc.Lookup("writeConcernError", "code").Int32() != 64 {
			t.Fatalf("%s", rp.doc)
		}
		h.Remove()
	})
	t.Run("nth", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "nth", Nth: 2}, mongosim.ReplyError(2, ""))
		a, b, d := c.cmd(ins("nth")), c.cmd(ins("nth")), c.cmd(ins("nth"))
		if code(a) != 0 || code(b) != 2 || code(d) != 0 {
			t.Fatal("nth")
		}
		h.Remove()
	})
	t.Run("unknown responseTo", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "u"}, mongosim.ReplyUnknownResponseTo())
		id := c.sendMsg(0, ins("u"))
		rp, _ := c.read()
		if rp.responseTo() == id {
			t.Fatal("responseTo should not match")
		}
		h.Remove()
	})
	t.Run("moreToCome reply", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "m"}, mongosim.ReplyMoreToCome())
		rp := c.cmd(ins("m"))
		if rp.flags&mongosim.FlagMoreToCome == 0 {
			t.Fatal("flag not set")
		}
		h.Remove()
	})
	t.Run("checksum", func(t *testing.T) {
		for _, valid := range []bool{true, false} {
			h := s.AddRule(mongosim.Match{Marker: "k"}, mongosim.ReplyChecksum(valid))
			rp := c.cmd(ins("k"))
			if rp.flags&1 == 0 {
				t.Fatal("no checksum flag")
			}
			n := len(rp.whole)
			got := binary.LittleEndian.Uint32(rp.whole[n-4:])
			want := crc32c(rp.whole[:n-4])
			if (got == want) != valid {
				t.Fatalf("valid=%v got=%x want=%x", valid, got, want)
			}
			h.Remove()
		}
	})
	t.Run("raw", func(t *testing.T) {
		d, _ := bson.Marshal(bson.D{{Key: "ok", Value: 1.0}, {Key: "raw", Value: "yes"}})
		h := s.AddRule(mongosim.Match{Marker: "r"}, mongosim.ReplyRaw(append([]byte{0}, d...)))
		rp := c.cmd(ins("r"))
		if rp.doc.Lookup("raw").StringValue() != "yes" || rp.flags != 0 {
			t.Fatal("raw")
		}
		h.Remove()
	})
	t.Run("oversize", func(t *testing.T) {
		o := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{MaxMessageSizeBytes: 5000}})
		oc := dial(t, o.Addr())
		o.AddRule(mongosim.Match{Marker: "o"}, mongosim.ReplyOversize())
		oc.sendMsg(0, ins("o"))
		var hdr [16]byte
		oc.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(oc.nc, hdr[:]); err != nil {
			t.Fatal(err)
		}
		if got := binary.LittleEndian.Uint32(hdr[:]); got != 5001 {
			t.Fatalf("messageLength %d", got)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "bad"}, mongosim.ReplyMalformed())
		rp := c.cmd(ins("bad"))
		if rp.doc.Validate() == nil {
			t.Fatal("body valid")
		}
		h.Remove()
	})
	t.Run("no reply", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "nr"}, mongosim.NoReply())
		c.sendMsg(0, ins("nr"))
		c.short = 100 * time.Millisecond
		if _, err := c.read(); err == nil {
			t.Fatal("reply")
		}
		h.Remove()
		// the connection is still usable (reset the deadline-induced state)
		if c.cmd(ping).doc.Lookup("ok").AsFloat64() != 1 {
			t.Fatal("conn broken")
		}
	})
	t.Run("reset after receive", func(t *testing.T) {
		h := s.AddRule(mongosim.Match{Marker: "rst"}, mongosim.ResetAfterReceive())
		c.sendMsg(0, ins("rst"))
		if _, err := c.read(); err == nil {
			t.Fatal("expected reset")
		}
		if h.Hits() != 1 {
			t.Fatal("hits")
		}
	})
}

func crc32c(b []byte) uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i)
		for j := 0; j < 8; j++ {
			if c&1 == 1 {
				c = c>>1 ^ 0x82f63b78
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	c := ^uint32(0)
	for _, x := range b {
		c = t[byte(c)^x] ^ c>>8
	}
	return ^c
}

func TestMoreToComeRequestGetsNoReply(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	c.sendMsg(mongosim.FlagMoreToCome, bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "t"}})
	c.sendMsg(0, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}})
	rp, err := c.read()
	if err != nil || rp.doc.Lookup("ok").AsFloat64() != 1 || rp.responseTo() != c.id {
		t.Fatal("first reply must be the ping's")
	}
	if got := s.Received(); len(got) != 2 || got[0].Flags&mongosim.FlagMoreToCome == 0 {
		t.Fatal("recording")
	}
}

func TestLargeReply(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	c := dial(t, s.Addr())
	const n = 30 << 20
	rp := c.cmd(bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "t"}, {Key: "corraltestReplyBytes", Value: int64(n)}})
	if len(rp.whole) < n || len(rp.whole) > n+1<<20 {
		t.Fatalf("reply %d bytes", len(rp.whole))
	}
	sum1 := sha256.Sum256(rp.whole[16:])
	rp2 := c.cmd(bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "t"}, {Key: "corraltestReplyBytes", Value: int64(n)}})
	if sha256.Sum256(rp2.whole[16:]) != sum1 {
		// request ids differ only in the header; payload must be deterministic
		t.Fatal("pattern not deterministic")
	}
}

// ---- SCRAM using the official driver ----

func connect(t *testing.T, uri string, mod ...func(*options.ClientOptions)) *mongo.Client {
	t.Helper()
	o := options.Client().ApplyURI(uri).SetServerSelectionTimeout(3 * time.Second)
	for _, m := range mod {
		m(o)
	}
	cl, err := mongo.Connect(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Disconnect(context.Background()) })
	return cl
}

func TestDriverCRUDPrimaryAndMongos(t *testing.T) {
	for name, spec := range map[string]mongosim.HelloSpec{
		"primary": {Role: mongosim.Primary}, "mongos": {Role: mongosim.Mongos}, "standalone": {},
	} {
		t.Run(name, func(t *testing.T) {
			s := mongosim.Start(t, mongosim.Options{Hello: spec})
			cl := connect(t, "mongodb://"+s.Addr()+"/?directConnection=true")
			coll := cl.Database("db").Collection("c")
			ctx := context.Background()
			if _, err := coll.InsertOne(ctx, bson.D{{Key: "x", Value: 1}}); err != nil {
				t.Fatal(err)
			}
			if _, err := coll.UpdateOne(ctx, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 2}}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := coll.DeleteOne(ctx, bson.D{}); err != nil {
				t.Fatal(err)
			}
			if err := coll.FindOne(ctx, bson.D{}).Err(); err != mongo.ErrNoDocuments {
				t.Fatalf("find: %v", err)
			}
			if err := cl.Ping(ctx, nil); err != nil {
				t.Fatal(err)
			}
			var withLsid bool
			for _, m := range s.ReceivedCommand("insert") {
				withLsid = m.HasLsid
			}
			if !withLsid {
				t.Fatal("driver session lsid not recorded")
			}
		})
	}
}

func TestDriverDiscoversReplicaSetAndFailover(t *testing.T) {
	c := mongosim.StartReplicaSet(t, 3, mongosim.ClusterOptions{Arbiters: 1})
	cl := connect(t, c.URI(), func(o *options.ClientOptions) { o.SetHeartbeatInterval(500 * time.Millisecond) })
	ctx := context.Background()
	if _, err := cl.Database("db").Collection("c").InsertOne(ctx, bson.D{{Key: "x", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if len(c.Member(0).ReceivedCommand("insert")) != 1 {
		t.Fatal("write should reach member 0")
	}
	c.SetPrimary(2)
	if !poll(func() bool {
		_, err := cl.Database("db").Collection("c").InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
		return err == nil && len(c.Member(2).ReceivedCommand("insert")) > 0
	}) {
		t.Fatal("driver did not fail over to member 2")
	}
	// arbiter advertised but never contacted for writes; its address is in the hello of every data member.
	d := dial(t, c.Member(1).Addr()).cmd(helloCmd("")).doc
	if arrLen(d.Lookup("arbiters")) != 1 || d.Lookup("setName").StringValue() != "rs0" || d.Lookup("secondary").Boolean() != true {
		t.Fatalf("%s", d)
	}
	if _, ok := d.Lookup("electionId").ObjectIDOK(); ok {
		t.Fatal("secondary must not send electionId")
	}
	pe := dial(t, c.Member(2).Addr()).cmd(helloCmd("")).doc.Lookup("electionId").ObjectID()
	c.SetPrimary(0)
	if ne := dial(t, c.Member(0).Addr()).cmd(helloCmd("")).doc.Lookup("electionId").ObjectID(); bytes.Compare(ne[:], pe[:]) <= 0 {
		t.Fatal("electionId must increase")
	}
	c.NoPrimary()
	d = dial(t, c.Member(0).Addr()).cmd(helloCmd("")).doc
	if _, err := d.LookupErr("primary"); err == nil || d.Lookup("isWritablePrimary").Boolean() {
		t.Fatalf("NoPrimary: %s", d)
	}
	c.SetPrimary(1)
	c.StepDown(1)
	if c.PrimaryIndex() != -1 {
		t.Fatal("StepDown")
	}
}

func TestDriverSCRAM(t *testing.T) {
	users := map[string]string{"synthuser": "synth-password-1"}
	cred := func(user, pw string) func(*options.ClientOptions) {
		return func(o *options.ClientOptions) {
			o.SetAuth(options.Credential{AuthMechanism: "SCRAM-SHA-256", Username: user, Password: pw, AuthSource: "admin"})
		}
	}
	try := func(s *mongosim.Server, user, pw string) error {
		cl := connect(t, "mongodb://"+s.Addr()+"/?directConnection=true", cred(user, pw))
		return cl.Ping(context.Background(), nil)
	}
	s := mongosim.Start(t, mongosim.Options{Users: users, RequireAuth: true})
	if err := try(s, "synthuser", "synth-password-1"); err != nil {
		t.Fatalf("success: %v", err)
	}
	var authed bool
	for _, ci := range s.Conns() {
		if ci.Authenticated && ci.AuthMechanism == "SCRAM-SHA-256" && ci.AuthUser == "synthuser" {
			authed = true
		}
	}
	if !authed {
		t.Fatalf("auth not recorded: %+v", s.Conns())
	}
	// Unauthenticated command on RequireAuth is refused with 13.
	rp := dial(t, s.Addr()).cmd(bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "t"}})
	if code(rp) != 13 {
		t.Fatalf("%s", rp.doc)
	}
	// Genuine verification: a wrong password fails with 18.
	if err := try(s, "synthuser", "wrong"); err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("wrong password: %v", err)
	}

	cases := []struct {
		name string
		o    mongosim.AuthOutcome
		want string
	}{
		{"18", mongosim.Fail18, "AuthenticationFailed"},
		{"18 at continue", mongosim.Fail18.AtContinue(), "AuthenticationFailed"},
		{"334", mongosim.Fail334, "MechanismUnavailable"},
		{"10107", mongosim.ErrorCode(10107, "NotWritablePrimary"), "NotWritablePrimary"},
		{"badsig", mongosim.BadServerSignature, "server validation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mongosim.Start(t, mongosim.Options{Users: users})
			s.SetAuthOutcome(tc.o)
			err := try(s, "synthuser", "synth-password-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q got %v", tc.want, err)
			}
		})
	}
	t.Run("reset", func(t *testing.T) {
		s := mongosim.Start(t, mongosim.Options{Users: users})
		s.SetAuthOutcome(mongosim.AuthReset)
		if err := try(s, "synthuser", "synth-password-1"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("delay then success, queue", func(t *testing.T) {
		s := mongosim.Start(t, mongosim.Options{Users: users})
		s.QueueAuthOutcomes(mongosim.Fail18, mongosim.AuthDelay(200*time.Millisecond))
		if err := try(s, "synthuser", "synth-password-1"); err == nil {
			t.Fatal("first conversation should fail")
		}
		st := time.Now()
		if err := try(s, "synthuser", "synth-password-1"); err != nil || time.Since(st) < 190*time.Millisecond {
			t.Fatalf("second: %v (%v)", err, time.Since(st))
		}
		if err := try(s, "synthuser", "synth-password-1"); err != nil {
			t.Fatalf("third (sticky Succeed): %v", err)
		}
	})
}

func TestDriverObservesFaults(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary}})
	cl := connect(t, "mongodb://"+s.Addr()+"/?directConnection=true", func(o *options.ClientOptions) {
		o.SetRetryWrites(false).SetRetryReads(false).SetTimeout(0)
	})
	coll := cl.Database("db").Collection("c")
	ctx := context.Background()
	if _, err := coll.InsertOne(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	h := s.AddRule(mongosim.Match{Command: "insert", Once: true}, mongosim.ResetAfterReceive())
	if _, err := coll.InsertOne(ctx, bson.D{}); !mongo.IsNetworkError(err) {
		t.Fatalf("expected network error, got %v", err)
	}
	h.Remove()
	s.AddRule(mongosim.Match{Command: "insert", Once: true}, mongosim.ReplyError(50, "MaxTimeMSExpired"))
	_, err := coll.InsertOne(ctx, bson.D{})
	var ce mongo.CommandError
	if !asCmdErr(err, &ce) || ce.Code != 50 {
		t.Fatalf("got %v", err)
	}
	s.AddRule(mongosim.Match{Command: "insert", Once: true}, mongosim.ReplyWriteConcernError(64))
	_, err = coll.InsertOne(ctx, bson.D{})
	var we mongo.WriteException
	if !asWriteErr(err, &we) || we.WriteConcernError == nil || we.WriteConcernError.Code != 64 {
		t.Fatalf("got %v", err)
	}
}

func asCmdErr(err error, t *mongo.CommandError) bool {
	ce, ok := err.(mongo.CommandError)
	if ok {
		*t = ce
	}
	return ok
}

func asWriteErr(err error, t *mongo.WriteException) bool {
	we, ok := err.(mongo.WriteException)
	if ok {
		*t = we
	}
	return ok
}

func arrLen(v bson.RawValue) int {
	vals, _ := v.Array().Values()
	return len(vals)
}
