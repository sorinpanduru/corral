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

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
	"github.com/sorinpanduru/corral/internal/testkit/phpfpm"
)

// primaryOf asks every member directly for the primary and returns its
// address ("" when no member reports one).
func primaryOf(t *testing.T, addrs []string, clients map[string]*mongo.Client) string {
	for _, a := range addrs {
		res, err := pxRunAdmin(t, clients[a], bson.D{{Key: "hello", Value: 1}})
		if err != nil {
			continue
		}
		if w, _ := res["isWritablePrimary"].(bool); w {
			return a
		}
	}
	return ""
}

// AC-18 (FR-18, FR-34, FR-35, FR-36, FR-37), functional: a forced step-down of
// the primary of a 3-node set under continuous retryable load from the Go and
// PHP drivers. pool.checkout_timeout is 120 s so that the outcome does not depend
// on how long the election takes; replSetFreeze on the secondaries holds the
// election open so that readiness can be sampled deterministically.
func TestForcedStepDownUnderLoad_AC18(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 3})
		addrs := rs.Addrs()
		direct := map[string]*mongo.Client{}
		for _, a := range addrs {
			direct[a] = pxDirect(t, a)
		}
		c := pxStart(t, pxOpts{upstream: addrs, rs: "rs0", set: map[string]string{"pool.checkout_timeout": "120s", "pool.max_size": "6"}})
		c.waitReady()
		oldPrimary := primaryOf(t, addrs, direct)
		if oldPrimary == "" {
			t.Fatal("no primary")
		}
		changes := c.metric("corral_upstream_primary_changes_total")

		// Background load: retryable reads and writes only, no transactions.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var ops, failures, timeouts atomic.Int64
		var firstErr atomic.Value
		fail := func(err error) {
			failures.Add(1)
			if pxCode(err) == 262 {
				timeouts.Add(1)
			}
			firstErr.CompareAndSwap(nil, err.Error())
		}
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				cl := pxGoClient(t, "mongodb://"+c.proxy)
				coll := cl.Database("corraltest").Collection("px18")
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					id := fmt.Sprintf("go-%d-%d", i, n)
					if _, err := coll.InsertOne(context.Background(), bson.D{{Key: "_id", Value: id}}); err != nil {
						fail(err)
						continue
					}
					ops.Add(1)
					if err := coll.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Err(); err != nil {
						fail(err)
						continue
					}
					ops.Add(1)
					time.Sleep(20 * time.Millisecond)
				}
			}(i)
		}
		fx := phpfpm.Start(t, phpfpm.Opts{Workers: 8, ScriptsDir: filepath.Join("php")})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					out, err := fx.Run("crud.php", map[string]string{"HTTP_X_URI": "mongodb://" + c.proxy + "/"})
					if err != nil || !strings.Contains(string(out), "OK") {
						fail(fmt.Errorf("php: %v %q", err, out))
						continue
					}
					ops.Add(1)
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
		time.Sleep(2 * time.Second)

		// Hold the election open: freeze every secondary directly, then force the
		// primary down.
		for _, a := range addrs {
			if a == oldPrimary {
				continue
			}
			if _, err := pxRunAdmin(t, direct[a], bson.D{{Key: "replSetFreeze", Value: 20}}); err != nil {
				t.Fatalf("replSetFreeze on %s: %v", a, err)
			}
		}
		_, err := pxRunAdmin(t, direct[oldPrimary], bson.D{{Key: "replSetStepDown", Value: 30}, {Key: "force", Value: true}})
		if err != nil && !mongo.IsNetworkError(err) {
			t.Fatalf("replSetStepDown: %v", err)
		}
		// While no primary is known, readiness is 503 upstream_unavailable.
		var saw503 bool
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if primaryOf(t, addrs, direct) != "" {
				break
			}
			if code, body := c.readyz(); code == 503 && body == "upstream_unavailable" {
				saw503 = true
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !saw503 {
			t.Fatal("readiness was never 503 upstream_unavailable while no primary was known")
		}
		// Unfreeze and let the election finish.
		for _, a := range addrs {
			if a != oldPrimary {
				pxRunAdmin(t, direct[a], bson.D{{Key: "replSetFreeze", Value: 0}})
			}
		}
		var newPrimary string
		pxEventually(t, 120*time.Second, "a new primary", func() bool {
			newPrimary = primaryOf(t, addrs, direct)
			return newPrimary != "" && newPrimary != oldPrimary
		})
		c.waitReady()
		time.Sleep(2 * time.Second)
		close(stop)
		wg.Wait()

		if c.metric("corral_upstream_primary_changes_total") <= changes {
			t.Fatal("corral_upstream_primary_changes_total did not increase")
		}
		if timeouts.Load() != 0 {
			t.Fatalf("%d requests failed with 262", timeouts.Load())
		}
		if failures.Load() != 0 {
			t.Fatalf("%d operations failed (first: %v) out of %d", failures.Load(), firstErr.Load(), ops.Load())
		}
		// New leases go to the new primary: a write through corral is on it.
		cl := pxGoClient(t, "mongodb://"+c.proxy)
		if _, err := cl.Database("corraltest").Collection("px18after").InsertOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: "after"}}); err != nil {
			t.Fatal(err)
		}
		if err := direct[newPrimary].Database("corraltest").Collection("px18after").FindOne(pxCtx(t, 30*time.Second), bson.D{{Key: "_id", Value: "after"}}).Err(); err != nil {
			t.Fatalf("the write is not on the new primary %s: %v", newPrimary, err)
		}
	})
}
