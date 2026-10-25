//go:build functional

package functional

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/internal/testkit/phpfpm"
	"github.com/sorinpanduru/corral/twins/faultproxy"
)

// upstreamConnections counts the connections to mongod by appName, through
// $currentOp run directly against mongod (not through corral).
func upstreamConnections(t testing.TB, admin *mongo.Client) map[string]map[int64]bool {
	t.Helper()
	cur, err := admin.Database("admin").Aggregate(context.Background(), mongo.Pipeline{
		{{Key: "$currentOp", Value: bson.D{{Key: "allUsers", Value: true}, {Key: "idleConnections", Value: true}}}},
	})
	if err != nil {
		t.Fatalf("$currentOp: %v", err)
	}
	var docs []bson.M
	if err := cur.All(context.Background(), &docs); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[int64]bool{}
	for _, d := range docs {
		app, _ := d["appName"].(string)
		var id int64
		switch v := d["connectionId"].(type) {
		case int32:
			id = int64(v)
		case int64:
			id = v
		default:
			continue
		}
		if out[app] == nil {
			out[app] = map[int64]bool{}
		}
		out[app][id] = true
	}
	return out
}

// sampleUpstream polls until stop is closed and returns the maxima.
func sampleUpstream(t *testing.T, admin *mongo.Client, c *px, stop <-chan struct{}) (maxPool, maxMonitor int, maxGauge float64) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(50 * time.Millisecond):
		}
		conns := upstreamConnections(t, admin)
		if n := len(conns["corral"]); n > maxPool {
			maxPool = n
		}
		if n := len(conns["corral-monitor"]); n > maxMonitor {
			maxMonitor = n
		}
		if g := c.metric("corral_pool_connections"); g > maxGauge {
			maxGauge = g
		}
	}
}

// AC-12 (FR-25, FR-26, FR-28, FR-37, FR-48), the motivating case: with at least
// 50 PHP-FPM workers holding persistent connections and pool.max_size=4, the
// upstream never sees more than 4 pool connections (and at most one monitoring
// connection per host), the gauge never exceeds 4 and every operation succeeds.
func TestPHPFPMFleetStaysWithinPool_AC12(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs, c := startSingle(t, v, pxOpts{set: map[string]string{"pool.max_size": "4"}})
		fx := phpfpm.Start(t, phpfpm.Opts{Workers: 50, ScriptsDir: filepath.Join("php")})
		admin := rs.Admin()
		stop := make(chan struct{})
		type maxima struct {
			pool, monitor int
			gauge         float64
		}
		done := make(chan maxima, 1)
		go func() {
			p, m, g := sampleUpstream(t, admin, c, stop)
			done <- maxima{p, m, g}
		}()
		uri := "mongodb://" + c.proxy + "/"
		var bad atomic.Int32
		var wg sync.WaitGroup
		for w := 0; w < 60; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					out, err := fx.Run("crud.php", map[string]string{"HTTP_X_URI": uri})
					if err != nil || !strings.Contains(string(out), "OK") {
						bad.Add(1)
						t.Logf("php: %v %q", err, out)
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		m := <-done
		if bad.Load() != 0 {
			t.Fatalf("%d PHP operations failed", bad.Load())
		}
		if m.pool > 4 || m.pool == 0 {
			t.Fatalf("upstream saw %d appName=corral connections, want 1..4", m.pool)
		}
		if m.monitor > 1 {
			t.Fatalf("upstream saw %d corral-monitor connections, want at most 1 per host", m.monitor)
		}
		if m.gauge > 4 {
			t.Fatalf("corral_pool_connections reached %v", m.gauge)
		}
	})
}

// AC-12: the same holds for the Go driver with 200 goroutines and maxPoolSize=200.
func TestGoDriverFleetStaysWithinPool_AC12(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs, c := startSingle(t, v, pxOpts{set: map[string]string{"pool.max_size": "4"}})
		admin := rs.Admin()
		stop := make(chan struct{})
		type maxima struct {
			pool, monitor int
			gauge         float64
		}
		done := make(chan maxima, 1)
		go func() {
			p, m, g := sampleUpstream(t, admin, c, stop)
			done <- maxima{p, m, g}
		}()
		cl := pxGoClient(t, "mongodb://"+c.proxy, func(o *options.ClientOptions) { o.SetMaxPoolSize(200) })
		coll := cl.Database("corraltest").Collection("go12")
		var bad atomic.Int32
		var wg sync.WaitGroup
		for g := 0; g < 200; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					id := fmt.Sprintf("%d-%d", g, i)
					if _, err := coll.InsertOne(context.Background(), bson.D{{Key: "_id", Value: id}}); err != nil {
						bad.Add(1)
						continue
					}
					if err := coll.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Err(); err != nil {
						bad.Add(1)
					}
				}
			}(g)
		}
		wg.Wait()
		close(stop)
		m := <-done
		if bad.Load() != 0 {
			t.Fatalf("%d operations failed", bad.Load())
		}
		if m.pool > 4 || m.monitor > 1 || m.gauge > 4 {
			t.Fatalf("pool connections %d (max 4), monitor %d (max 1), gauge %v (max 4)", m.pool, m.monitor, m.gauge)
		}
	})
}

func pooledConns(fp *faultproxy.Proxy, filter func(faultproxy.Record) bool) map[int]int {
	out := map[int]int{}
	for _, r := range fp.Requests() {
		if r.AppName == "corral" && filter(r) {
			out[r.ConnIndex]++
		}
	}
	return out
}

// AC-16 (FR-31, FR-32): with a pool of exactly two connections handed out in
// round-robin order (the corraltest hook), the commands of one transaction and
// the getMores of one cursor are spread over both upstream connections and all
// succeed; retryable writes succeed under 20 concurrent clients; a getMore from
// another session is refused by the server and forwarded unchanged.
func TestStatelessLeasing_AC16(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
		fp := faultproxy.Start(t, rs.Addrs()[0], faultproxy.Options{RewriteHelloSelf: true, DropCompression: true})
		c := pxStart(t, pxOpts{upstream: []string{fp.Addr()}, rs: "rs0", roundRob: true,
			set: map[string]string{"pool.min_size": "2", "pool.max_size": "2"}})
		c.waitReady()
		pxEventually(t, 30*time.Second, "two pool connections", func() bool { return c.metric("corral_pool_connections") >= 2 })
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		coll := cl.Database("corraltest").Collection("px16")
		ctx := pxCtx(t, 120*time.Second)

		for _, outcome := range []string{"commit", "abort"} {
			fp.ResetRecording()
			sess, err := cl.StartSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.StartTransaction(); err != nil {
				t.Fatal(err)
			}
			sc := mongo.NewSessionContext(ctx, sess)
			for i := 0; i < 6; i++ {
				if _, err := coll.InsertOne(sc, bson.D{{Key: "o", Value: outcome}, {Key: "i", Value: i}}); err != nil {
					t.Fatalf("%s: insert %d: %v", outcome, i, err)
				}
			}
			if outcome == "commit" {
				err = sess.CommitTransaction(ctx)
			} else {
				err = sess.AbortTransaction(ctx)
			}
			if err != nil {
				t.Fatalf("%s: %v", outcome, err)
			}
			sess.EndSession(ctx)
			txn := pooledConns(fp, func(r faultproxy.Record) bool { return r.Direction == faultproxy.ToUpstream && r.HasTxnNumber })
			if len(txn) != 2 {
				t.Fatalf("%s: transaction commands used connections %v, want both", outcome, txn)
			}
		}

		// A cursor over 100 documents with batchSize 2.
		var docs []any
		for i := 0; i < 100; i++ {
			docs = append(docs, bson.D{{Key: "n", Value: i}})
		}
		cursorColl := cl.Database("corraltest").Collection("px16cursor")
		if _, err := cursorColl.InsertMany(ctx, docs); err != nil {
			t.Fatal(err)
		}
		fp.ResetRecording()
		cur, err := cursorColl.Find(ctx, bson.D{}, options.Find().SetBatchSize(2))
		if err != nil {
			t.Fatal(err)
		}
		var all []bson.M
		if err := cur.All(ctx, &all); err != nil || len(all) != 100 {
			t.Fatalf("cursor returned %d documents, %v", len(all), err)
		}
		gm := pooledConns(fp, func(r faultproxy.Record) bool {
			return r.Direction == faultproxy.ToUpstream && (strings.EqualFold(r.Command, "getMore") || strings.EqualFold(r.Command, "find"))
		})
		if len(gm) != 2 {
			t.Fatalf("find and getMores used connections %v, want both", gm)
		}

		// Retryable writes under 20 concurrent clients.
		var wg sync.WaitGroup
		var bad atomic.Int32
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				wc := pxGoClient(t, "mongodb://"+c.proxy)
				for n := 0; n < 5; n++ {
					if _, err := wc.Database("corraltest").Collection("px16w").InsertOne(context.Background(), bson.D{{Key: "c", Value: i}, {Key: "n", Value: n}}); err != nil {
						bad.Add(1)
					}
				}
			}(i)
		}
		wg.Wait()
		if bad.Load() != 0 {
			t.Fatalf("%d retryable writes failed", bad.Load())
		}

		// Another session's cursor is refused by the server, and the refusal is
		// forwarded unchanged: the same code as asking mongod directly.
		refuse := func(cl *mongo.Client) int32 {
			s1, _ := cl.StartSession()
			s2, _ := cl.StartSession()
			defer s1.EndSession(ctx)
			defer s2.EndSession(ctx)
			var first bson.Raw
			err := cl.Database("corraltest").RunCommand(mongo.NewSessionContext(ctx, s1),
				bson.D{{Key: "find", Value: "px16cursor"}, {Key: "batchSize", Value: 1}}).Decode(&first)
			if err != nil {
				t.Fatal(err)
			}
			id := first.Lookup("cursor", "id").Int64()
			err = cl.Database("corraltest").RunCommand(mongo.NewSessionContext(ctx, s2),
				bson.D{{Key: "getMore", Value: id}, {Key: "collection", Value: "px16cursor"}}).Err()
			if err == nil {
				t.Fatal("a getMore from another session succeeded")
			}
			return pxCode(err)
		}
		viaCorral, direct := refuse(cl), refuse(pxDirect(t, rs.Addrs()[0]))
		if viaCorral != direct || viaCorral <= 0 {
			t.Fatalf("refusal via corral %d, direct %d", viaCorral, direct)
		}
	})
}

// AC-20 (FR-40, functional): a retryable write that meets a connection reset
// after the request was sent succeeds on the driver's retry, with the Go driver
// and the PHP driver.
func TestRetryableWriteAfterReset_AC20(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1})
		fp := faultproxy.Start(t, rs.Addrs()[0], faultproxy.Options{RewriteHelloSelf: true, DropCompression: true})
		c := pxStart(t, pxOpts{upstream: []string{fp.Addr()}, rs: "rs0"})
		c.waitReady()
		t.Run("go", func(t *testing.T) {
			cl := pxGoClient(t, "mongodb://"+c.proxy)
			fp.ResetRecording()
			h := fp.AddFault(faultproxy.Match{Command: "insert", Once: true}, faultproxy.ResetAfterRequest())
			defer h.Remove()
			if _, err := cl.Database("corraltest").Collection("px20").InsertOne(pxCtx(t, 60*time.Second), bson.D{{Key: "_id", Value: "go"}}); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if h.Hits() != 1 {
				t.Fatalf("the fault was applied %d times", h.Hits())
			}
		})
		t.Run("php", func(t *testing.T) {
			fx := phpfpm.Start(t, phpfpm.Opts{Workers: 4, ScriptsDir: filepath.Join("php")})
			h := fp.AddFault(faultproxy.Match{Command: "insert", Once: true}, faultproxy.ResetAfterRequest())
			defer h.Remove()
			out, err := fx.Run("crud.php", map[string]string{"HTTP_X_URI": "mongodb://" + c.proxy + "/", "HTTP_X_ID": "php-reset"})
			if err != nil || !strings.Contains(string(out), "OK") {
				t.Fatalf("php: %v %q", err, out)
			}
			if h.Hits() != 1 {
				t.Fatalf("the fault was applied %d times", h.Hits())
			}
		})
	})
}
