//go:build twin

package twin

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-3 (FR-4): with a short proxy.client_idle_timeout an idle connection is
// closed and one with an exchange in progress is not; with the default 0s an
// idle connection stays open.
func TestClientIdleTimeout_AC3(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"proxy.client_idle_timeout": "600ms"}})
	c.waitReady()
	tw.AddRule(mongosim.Match{Marker: "slow"}, mongosim.Delay(1800*time.Millisecond))
	idle := c.dial()
	busy := c.dial()
	rt(t, idle, ping(1))
	busy.Send(find(1, "slow"))
	begin := time.Now()
	closedNoBytes(t, idle)
	if d := time.Since(begin); d < 400*time.Millisecond || d > 5*time.Second {
		t.Fatalf("idle connection closed after %v", d)
	}
	m, err := busy.ReadMessage(10 * time.Second)
	if err != nil || !isOK(m) {
		t.Fatalf("the connection with an exchange in progress was closed: %v", err)
	}
	eventually(t, "an idle_timeout close to be counted", func() bool {
		return c.metric("corral_client_connections_closed_total", "reason", "idle_timeout") >= 1
	})
}

func TestClientIdleTimeoutDefaultOff_AC3(t *testing.T) {
	c, _ := startMongos(t, opts{})
	c.waitReady()
	conn := c.dial()
	rt(t, conn, ping(1))
	if _, err := conn.ReadMessage(2500 * time.Millisecond); err != rawwire.ErrTimeout {
		t.Fatalf("an idle connection with the default timeout: %v", err)
	}
	if m := rt(t, conn, ping(2)); !isOK(m) {
		t.Fatal("connection unusable after idling")
	}
}

// AC-13 (FR-27, FR-39, FR-50): pool.max_size=1 with the first request held.
func TestPoolCheckoutTimeoutAndQueue_AC13(t *testing.T) {
	o, col := traced(t, opts{set: map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "1s", "pool.max_waiters": "1"}})
	c, tw := startMongos(t, o)
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "hold"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("the first request was not held")
	}
	failedBefore := c.metric("corral_pool_checkouts_total", "outcome", "failed")
	toBefore := c.metric("corral_pool_checkout_failures_total", "error_detail", "pool_wait_timeout")
	qfBefore := c.metric("corral_pool_checkout_failures_total", "error_detail", "pool_queue_full")
	// Second request: waits for the checkout timeout, then 262. With
	// max_waiters=1 a third gets 262 at once while the second is queued.
	type res struct {
		m   *rawwire.Message
		d   time.Duration
		err error
	}
	second := make(chan res, 1)
	go func() {
		conn := rawwire.Dial(t, c.proxy)
		begin := time.Now()
		conn.Send(mk(1, "second", "find", "c", "$db", "d"))
		m, err := conn.ReadMessage(10 * time.Second)
		second <- res{m, time.Since(begin), err}
	}()
	time.Sleep(250 * time.Millisecond)
	begin := time.Now()
	third := rt(t, c.dial(), find(1, "third"))
	if code(third) != 262 || time.Since(begin) > 500*time.Millisecond {
		t.Fatalf("third request: %v after %v", doc(third), time.Since(begin))
	}
	s := <-second
	if s.err != nil || code(s.m) != 262 {
		t.Fatalf("second request: %v %v", s.err, s.m)
	}
	if s.d < 800*time.Millisecond || s.d > 3*time.Second {
		t.Fatalf("second request failed after %v, want about 1s", s.d)
	}
	if hasLabel(s.m, "RetryableWriteError") || hasLabel(s.m, "TransientTransactionError") {
		t.Fatal("a plain read carries error labels")
	}
	// Labels. (max_waiters=1: wait one at a time.)
	retry := rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 2, Body: d("insert", "c", "$db", "d", "txnNumber", int64(1), "documents", bson.A{d("_id", 1)}, "lsid", d("id", bson.Binary{Subtype: 4, Data: make([]byte, 16)}))})
	if m := rt(t, c.dial(), retry); code(m) != 262 || !hasLabel(m, "RetryableWriteError") {
		t.Fatalf("retryable write: %v", doc(m))
	}
	txn := cmd(3, "find", "c", "$db", "d", "txnNumber", int64(2), "autocommit", false, "startTransaction", true)
	if m := rt(t, c.dial(), txn); code(m) != 262 || !hasLabel(m, "TransientTransactionError") {
		t.Fatalf("transaction statement: %v", doc(m))
	}
	// Metrics agree.
	failed := c.metric("corral_pool_checkouts_total", "outcome", "failed") - failedBefore
	to := c.metric("corral_pool_checkout_failures_total", "error_detail", "pool_wait_timeout") - toBefore
	qf := c.metric("corral_pool_checkout_failures_total", "error_detail", "pool_queue_full") - qfBefore
	if failed != to+qf || to < 1 || qf < 1 {
		t.Fatalf("failed=%v timeouts=%v queue_full=%v", failed, to, qf)
	}
	hold.Release()
	a.ReadMessage(10 * time.Second)
	c.stop()
	sp := col.Spans()
	var errRoot, errCheckout int
	for _, x := range spansNamed(sp, "pool.checkout") {
		if isErrSpan(x) && x.Attributes["error.type"] == "timeout" && x.Attributes["corral.error.detail"] == "pool_wait_timeout" {
			errCheckout++
		}
	}
	for _, x := range spansNamed(sp, "find c") {
		if isErrSpan(x) && x.Attributes["error.type"] == "timeout" && x.Attributes["corral.error.detail"] == "pool_wait_timeout" {
			errRoot++
		}
		if _, has := x.Attributes["db.response.status_code"]; has && isErrSpan(x) {
			t.Error("a synthesized error span carries db.response.status_code")
		}
	}
	if errRoot < 1 || errCheckout < 1 {
		t.Fatalf("timeout spans: %d root, %d checkout (all: %v)", errRoot, errCheckout, names(sp))
	}
}
