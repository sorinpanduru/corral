package faultproxy_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/twins/faultproxy"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func connect(t *testing.T, uri string, mod ...func(*options.ClientOptions)) *mongo.Client {
	t.Helper()
	o := options.Client().ApplyURI(uri).SetServerSelectionTimeout(4 * time.Second).SetHeartbeatInterval(500 * time.Millisecond)
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

// rawHello performs a hello over a raw connection and returns the reply frame.
func rawHello(t *testing.T, addr string) (frame []byte, doc bson.Raw) {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	d, _ := bson.Marshal(bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"},
		{Key: "client", Value: bson.D{{Key: "application", Value: bson.D{{Key: "name", Value: "rawtest"}}}}}})
	p := binary.LittleEndian.AppendUint32(nil, 0)
	p = append(p, 0)
	p = append(p, d...)
	b := make([]byte, 16+len(p))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint32(b[4:], 11)
	binary.LittleEndian.PutUint32(b[12:], 2013)
	copy(b[16:], p)
	nc.Write(b)
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	var l [4]byte
	if _, err := io.ReadFull(nc, l[:]); err != nil {
		t.Fatal(err)
	}
	n := binary.LittleEndian.Uint32(l[:])
	frame = make([]byte, n)
	copy(frame, l[:])
	if _, err := io.ReadFull(nc, frame[4:]); err != nil {
		t.Fatal(err)
	}
	flags := binary.LittleEndian.Uint32(frame[16:])
	end := len(frame)
	if flags&1 != 0 {
		end -= 4
	}
	return frame, bson.Raw(frame[21:end])
}

func hosts(t *testing.T, doc bson.Raw, key string) []string {
	t.Helper()
	vals, err := doc.Lookup(key).Array().Values()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range vals {
		out = append(out, v.StringValue())
	}
	return out
}

func TestRelayAndPerConnectionRecording(t *testing.T) {
	up := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary}})
	px := faultproxy.Start(t, up.Addr(), faultproxy.Options{})
	cl := connect(t, "mongodb://"+px.Addr()+"/?directConnection=true&appName=twin-app")
	ctx := context.Background()
	coll := cl.Database("db").Collection("c")
	for i := 0; i < 3; i++ {
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "i", Value: i}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(up.ReceivedCommand("insert")); n != 3 {
		t.Fatalf("upstream saw %d inserts", n)
	}
	reqs := px.RequestsBy("insert")
	if len(reqs) != 3 {
		t.Fatalf("proxy recorded %d inserts", len(reqs))
	}
	for _, r := range reqs {
		if r.DB != "db" || len(r.LsidID) != 16 || r.AppName != "twin-app" || r.Direction != faultproxy.ToUpstream {
			t.Fatalf("%+v", r)
		}
	}
	// Replies recorded and correlated to their request.
	var replies int
	for _, r := range px.Commands() {
		if r.Direction == faultproxy.ToClient && r.Command == "insert" && r.OK {
			replies++
		}
	}
	if replies != 3 {
		t.Fatalf("replies %d", replies)
	}
	if px.ConnCount() < 2 { // monitor + pool
		t.Fatalf("expected monitor and pool connections, got %d", px.ConnCount())
	}
	// Each proxy connection maps to exactly one upstream connection.
	if len(up.Conns()) != px.ConnCount() {
		t.Fatalf("upstream conns %d, proxy conns %d", len(up.Conns()), px.ConnCount())
	}
	// Connection identity of the commands matches upstream's view.
	poolConns := map[int]bool{}
	for _, r := range reqs {
		poolConns[r.ConnIndex] = true
	}
	if len(poolConns) != 1 {
		t.Fatalf("sequential inserts should reuse one connection: %v", poolConns)
	}
}

func TestRewriteHelloDiscoversOnlyTheProxy(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 1, mongosim.ClusterOptions{})
	real := cl.Member(0).Addr()
	px := faultproxy.Start(t, real, faultproxy.Options{RewriteHelloSelf: true})

	frame, doc := rawHello(t, px.Addr())
	if h := hosts(t, doc, "hosts"); len(h) != 1 || h[0] != px.Addr() {
		t.Fatalf("hosts %v", h)
	}
	if doc.Lookup("primary").StringValue() != px.Addr() || doc.Lookup("me").StringValue() != px.Addr() {
		t.Fatalf("primary/me not rewritten: %s", doc)
	}
	if got := binary.LittleEndian.Uint32(frame); int(got) != len(frame) {
		t.Fatal("messageLength")
	}
	// Direct to the real member still advertises the real address.
	_, direct := rawHello(t, real)
	if h := hosts(t, direct, "hosts"); h[0] != real {
		t.Fatalf("direct hosts %v", h)
	}
	// A driver connecting by replica set name discovers only the proxy.
	c := connect(t, "mongodb://"+px.Addr()+"/?replicaSet=rs0")
	if _, err := c.Database("db").Collection("c").InsertOne(context.Background(), bson.D{}); err != nil {
		t.Fatal(err)
	}
	for _, ci := range px.Conns() {
		if ci.AppName == "rawtest" {
			continue
		}
	}
	if len(px.RequestsBy("insert")) != 1 {
		t.Fatal("insert should flow through the proxy")
	}
	if len(cl.Member(0).ReceivedCommand("insert")) != 1 {
		t.Fatal("insert should reach the member")
	}
}

func TestRewriteDropsChecksumAndKeepsLength(t *testing.T) {
	up := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary, SetName: "rs0", Hosts: []string{"x:1"}}})
	up.AddRule(mongosim.Match{Command: "hello", Once: true}, mongosim.ReplyChecksum(true))
	px := faultproxy.Start(t, up.Addr(), faultproxy.Options{RewriteHello: map[string]string{"x:1": "y:2"}})
	frame, doc := rawHello(t, px.Addr())
	if flags := binary.LittleEndian.Uint32(frame[16:]); flags&1 != 0 {
		t.Fatal("checksum flag should be dropped")
	}
	if int(binary.LittleEndian.Uint32(frame)) != len(frame) {
		t.Fatal("messageLength")
	}
	if h := hosts(t, doc, "hosts"); h[0] != "y:2" {
		t.Fatalf("%v", h)
	}
}

func TestOneShotResetRetriedByDriver(t *testing.T) {
	cl := mongosim.StartReplicaSet(t, 1, mongosim.ClusterOptions{})
	px := faultproxy.Start(t, cl.Member(0).Addr(), faultproxy.Options{RewriteHelloSelf: true})
	c := connect(t, "mongodb://"+px.Addr()+"/?replicaSet=rs0")
	ctx := context.Background()
	coll := c.Database("db").Collection("c")
	if _, err := coll.InsertOne(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	h := px.AddFault(faultproxy.Match{Command: "insert", Once: true}, faultproxy.ResetAfterRequest())
	if _, err := coll.InsertOne(ctx, bson.D{}); err != nil {
		t.Fatalf("retryable write should succeed after reset: %v", err)
	}
	if h.Hits() != 1 {
		t.Fatalf("hits %d", h.Hits())
	}
	// The member saw the original and the retry; the retry used another connection.
	ins := px.RequestsBy("insert")
	if len(ins) != 3 {
		t.Fatalf("proxy recorded %d inserts, want 3", len(ins))
	}
	if ins[1].ConnIndex == ins[2].ConnIndex {
		t.Fatal("retry should use a new connection")
	}
	if string(ins[1].LsidID) != string(ins[2].LsidID) || !ins[1].HasTxnNumber || ins[1].TxnNumber != ins[2].TxnNumber {
		t.Fatal("retry should repeat lsid and txnNumber")
	}

	// Without retries the same fault is a network error.
	c2 := connect(t, "mongodb://"+px.Addr()+"/?replicaSet=rs0&retryWrites=false")
	if _, err := c2.Database("db").Collection("c").InsertOne(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	px.AddFault(faultproxy.Match{Command: "insert", Once: true}, faultproxy.ResetAfterRequest())
	if _, err := c2.Database("db").Collection("c").InsertOne(ctx, bson.D{}); !mongo.IsNetworkError(err) {
		t.Fatalf("expected network error, got %v", err)
	}
}

func TestDelayAndNth(t *testing.T) {
	up := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary}})
	px := faultproxy.Start(t, up.Addr(), faultproxy.Options{})
	cl := connect(t, "mongodb://"+px.Addr()+"/?directConnection=true")
	db := cl.Database("db")
	ctx := context.Background()
	ping := func() time.Duration {
		st := time.Now()
		if err := db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); err != nil {
			t.Fatal(err)
		}
		return time.Since(st)
	}
	h := px.AddFault(faultproxy.Match{Command: "ping", Nth: 2}, faultproxy.Delay(250*time.Millisecond))
	h2 := px.AddFault(faultproxy.Match{Command: "ping", Nth: 3}, faultproxy.DelayReply(250*time.Millisecond))
	if d := ping(); d > 200*time.Millisecond {
		t.Fatalf("first ping slow: %v", d)
	}
	if d := ping(); d < 240*time.Millisecond {
		t.Fatalf("second ping not delayed: %v", d)
	}
	if d := ping(); d < 240*time.Millisecond {
		t.Fatalf("third ping reply not delayed: %v", d)
	}
	if d := ping(); d > 200*time.Millisecond {
		t.Fatalf("fourth ping slow: %v", d)
	}
	if h.Hits() != 1 || h2.Hits() != 1 {
		t.Fatal("hits")
	}
}

func TestPauseBlackholeKill(t *testing.T) {
	up := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Primary}})
	px := faultproxy.Start(t, up.Addr(), faultproxy.Options{})
	cl := connect(t, "mongodb://"+px.Addr()+"/?directConnection=true")
	db := cl.Database("db")
	ctx := context.Background()
	if err := db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); err != nil {
		t.Fatal(err)
	}

	px.PauseAll()
	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err := db.RunCommand(tctx, bson.D{{Key: "ping", Value: 1}}).Err()
	cancel()
	if err == nil {
		t.Fatal("expected timeout while paused")
	}
	px.ResumeAll()
	// The cancelled operation closed its connection; a fresh one works after resume.
	if !eventually(func() bool { return db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err() == nil }) {
		t.Fatal("no recovery after resume")
	}

	n := px.ConnCount()
	old := map[int]bool{}
	for _, ci := range px.Conns() {
		if ci.Open {
			old[ci.Index] = true
		}
	}
	px.KillAll()
	if !eventually(func() bool {
		for _, ci := range px.Conns() {
			if old[ci.Index] && ci.Open {
				return false
			}
		}
		return true
	}) {
		t.Fatal("KillAll did not close connections")
	}
	if !eventually(func() bool { return db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err() == nil }) {
		t.Fatal("no recovery after KillAll")
	}
	if px.ConnCount() <= n {
		t.Fatal("driver should have reconnected")
	}

	// Blackhole a raw connection: it stays open but never answers.
	nc, _ := net.Dial("tcp", px.Addr())
	defer nc.Close()
	idx := -1
	if !eventually(func() bool {
		for _, ci := range px.Conns() {
			if ci.Client == nc.LocalAddr().String() {
				idx = ci.Index
			}
		}
		return idx >= 0
	}) {
		t.Fatal("raw connection not accepted")
	}
	px.Blackhole(idx)
	d, _ := bson.Marshal(bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}})
	b := make([]byte, 16+5+len(d))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint32(b[12:], 2013)
	copy(b[21:], d)
	nc.Write(b)
	nc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("blackholed connection answered")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("blackholed connection closed: %v", err)
	}
}

func eventually(f func() bool) bool {
	for i := 0; i < 100; i++ {
		if f() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestStopRacesAccept(t *testing.T) {
	up := mongosim.Start(t, mongosim.Options{})
	for i := 0; i < 200; i++ {
		px := faultproxy.Start(t, up.Addr(), faultproxy.Options{})
		go func() {
			if nc, err := net.Dial("tcp", px.Addr()); err == nil {
				nc.Close()
			}
		}()
		px.Stop()
		if px.OpenConns() != 0 {
			t.Fatal("connection left open after Stop")
		}
	}
}
