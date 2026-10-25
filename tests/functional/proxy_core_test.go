//go:build functional

package functional

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/faultproxy"
)

func pxHelloRaw(t *testing.T, addr string, id int32) bson.Raw {
	t.Helper()
	conn := rawwire.Dial(t, addr)
	if err := conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}})); err != nil {
		t.Fatal(err)
	}
	m, err := conn.ReadMessage(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return m.Docs[0]
}

func pxInt(r bson.Raw, k string) int64 {
	v, err := r.LookupErr(k)
	if err != nil {
		return -1
	}
	switch v.Type {
	case bson.TypeInt32:
		return int64(v.Int32())
	case bson.TypeInt64:
		return v.Int64()
	case bson.TypeDouble:
		return int64(v.Double())
	}
	return -1
}

// startSingle starts a one-node replica set and a corral in front of it.
func startSingle(t *testing.T, version string, o pxOpts) (*mongofx.ReplicaSet, *px) {
	t.Helper()
	rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: version, Members: 1})
	o.upstream = rs.Addrs()
	o.rs = "rs0"
	c := pxStart(t, o)
	c.waitReady()
	return rs, c
}

// AC-2 (FR-2, FR-3, FR-48): with proxy.max_client_connections=3 the 4th
// connection is closed with no bytes and counted; the connection ids on the 3
// accepted connections are distinct and positive.
func TestConnectionLimitAndIDs_AC2(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		_, c := startSingle(t, v, pxOpts{set: map[string]string{"proxy.max_client_connections": "3"}})
		var conns []*rawwire.Conn
		ids := map[int64]bool{}
		for i := 0; i < 3; i++ {
			conn := rawwire.Dial(t, c.proxy)
			conn.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}}))
			m, err := conn.ReadMessage(10 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			id := pxInt(m.Docs[0], "connectionId")
			if id <= 0 || ids[id] {
				t.Fatalf("connectionId %d (seen %v)", id, ids)
			}
			ids[id] = true
			conns = append(conns, conn)
		}
		over := rawwire.Dial(t, c.proxy)
		if err := over.ExpectClosedNoBytes(10 * time.Second); err != nil {
			t.Fatalf("the 4th connection: %v", err)
		}
		c.waitMetricAtLeast("limit_exceeded counted", 1, "corral_client_connections_closed_total", "reason", "limit_exceeded")
		_ = conns
	})
}

// AC-9 (functional): the handshake reply carries the upstream's parameters,
// with maxWireVersion capped at 25.
func TestHandshakeMatchesUpstream_AC9(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs, c := startSingle(t, v, pxOpts{})
		direct, err := pxRunAdmin(t, pxDirect(t, rs.Addrs()[0]), bson.D{{Key: "hello", Value: 1}})
		if err != nil {
			t.Fatal(err)
		}
		r := pxHelloRaw(t, c.proxy, 1)
		for _, k := range []string{"maxBsonObjectSize", "maxMessageSizeBytes", "maxWriteBatchSize", "logicalSessionTimeoutMinutes", "minWireVersion"} {
			if want := fmt.Sprint(direct[k]); fmt.Sprint(pxInt(r, k)) != want {
				t.Errorf("%s = %d, upstream %s", k, pxInt(r, k), want)
			}
		}
		up := int64(direct["maxWireVersion"].(int32))
		if up > 25 {
			up = 25
		}
		if got := pxInt(r, "maxWireVersion"); got != up {
			t.Errorf("maxWireVersion = %d, want %d", got, up)
		}
		if s, _ := r.Lookup("msg").StringValueOK(); s != "isdbgrid" {
			t.Errorf("msg = %q", s)
		}
		for _, k := range []string{"setName", "hosts", "topologyVersion", "compression", "saslSupportedMechs"} {
			if _, err := r.LookupErr(k); err == nil {
				t.Errorf("reply carries %s", k)
			}
		}
	})
}

// AC-17 (functional): blocked commands get 13; connectionStatus is forwarded
// and returns mongod's answer.
func TestBlockedAndForwardedCommands_AC17(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs, c := startSingle(t, v, pxOpts{})
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		for _, cmd := range []bson.D{
			{{Key: "currentOp", Value: 1}},
			{{Key: "killOp", Value: 1}, {Key: "op", Value: 1}},
			{{Key: "killAllSessions", Value: bson.A{}}},
			{{Key: "killAllSessionsByPattern", Value: bson.A{}}},
			{{Key: "killSessions", Value: bson.A{}}},
			{{Key: "aggregate", Value: 1}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$currentOp", Value: bson.D{}}}}}, {Key: "cursor", Value: bson.D{}}},
		} {
			_, err := pxRunAdmin(t, cl, cmd)
			if pxCode(err) != 13 {
				t.Errorf("%v: err %v, want code 13", cmd[0].Key, err)
			}
		}
		got, err := pxRunAdmin(t, cl, bson.D{{Key: "connectionStatus", Value: 1}})
		if err != nil {
			t.Fatal(err)
		}
		want, err := pxRunAdmin(t, pxDirect(t, rs.Addrs()[0]), bson.D{{Key: "connectionStatus", Value: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got["authInfo"]) != fmt.Sprint(want["authInfo"]) {
			t.Errorf("connectionStatus differs: %v vs %v", got["authInfo"], want["authInfo"])
		}
		if c.metric("corral_commands_blocked_total") < 6 {
			t.Errorf("commands_blocked_total = %v", c.metric("corral_commands_blocked_total"))
		}
	})
}

// AC-14 (functional): an unacknowledged insert followed by a find on the same
// client connection sees the document, 100 times out of 100, with pool.max_size=4
// under concurrent load from other clients.
func TestUnacknowledgedWriteThenFind_AC14(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		_, c := startSingle(t, v, pxOpts{set: map[string]string{"pool.max_size": "4"}})
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				cl := pxGoClient(t, "mongodb://"+c.proxy)
				coll := cl.Database("pxload").Collection("c")
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					coll.InsertOne(context.Background(), bson.D{{Key: "w", Value: i}, {Key: "n", Value: n}})
					coll.FindOne(context.Background(), bson.D{{Key: "w", Value: i}}).Err()
				}
			}(i)
		}
		defer func() { close(stop); wg.Wait() }()
		cl := pxGoClient(t, "mongodb://"+c.proxy, func(o *options.ClientOptions) {
			o.SetMaxPoolSize(1).SetWriteConcern(writeconcern.Unacknowledged())
		})
		coll := cl.Database("px14").Collection("c")
		seen := 0
		for i := 0; i < 100; i++ {
			if _, err := coll.InsertOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: i}}); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
			if err := coll.FindOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: i}}).Err(); err == nil {
				seen++
			}
		}
		if seen != 100 {
			t.Fatalf("%d of 100 finds saw the unacknowledged insert", seen)
		}
	})
}

// AC-25 / AC-39 (functional): readiness follows the upstream and the gauge
// matches it.
func TestReadinessAndGauge_AC25(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
		fp := faultproxy.Start(t, rs.Addrs()[0], faultproxy.Options{RewriteHelloSelf: true, DropCompression: true})
		c := pxStart(t, pxOpts{upstream: []string{fp.Addr()}, rs: "rs0", set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "500ms"}})
		c.waitReady()
		if c.metric("corral_upstream_available") != 1 {
			t.Fatal("corral_upstream_available is not 1 when ready")
		}
		// Saturating the pool with timeouts leaves readiness at 200.
		h := fp.AddFault(faultproxy.Match{Command: "find"}, faultproxy.Delay(3*time.Second))
		cl := pxGoClient(t, "mongodb://"+c.proxy, func(o *options.ClientOptions) { o.SetRetryReads(false) })
		var timeouts atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := cl.Database("px25").Collection("c").FindOne(context.Background(), bson.D{}).Err(); pxCode(err) == 262 {
					timeouts.Add(1)
				}
			}()
		}
		wg.Wait()
		if timeouts.Load() == 0 {
			t.Fatal("no request timed out while the pool was saturated")
		}
		if code, body := c.readyz(); code != 200 {
			t.Fatalf("readiness during saturation: %d %s", code, body)
		}
		h.Remove()
		// An unreachable upstream gives upstream_unavailable, and recovery 200.
		fp.BlackholeNew(true)
		fp.KillAll()
		c.waitReadyz(503, "upstream_unavailable")
		if c.metric("corral_upstream_available") != 0 {
			t.Fatal("corral_upstream_available is not 0 when not ready")
		}
		fp.BlackholeNew(false)
		c.waitReady()
		pxEventually(t, 30*time.Second, "gauge back to 1", func() bool { return c.metric("corral_upstream_available") == 1 })
	})
}

// AC-39 (functional): a network failure increases connect_errors but not
// auth_failures.
func TestNetworkFailureCountsConnectErrorOnly_AC39(t *testing.T) {
	c := pxStart(t, pxOpts{upstream: []string{pxAddr(t)}})
	c.waitMetricAtLeast("a network connect error", 1, "corral_upstream_connect_errors_total", "kind", "monitor", "error_class", "network")
	if got := c.metric("corral_upstream_auth_failures_total"); got != 0 {
		t.Fatalf("auth_failures_total = %v after network failures only", got)
	}
}

var _ = mongo.ErrNoDocuments
