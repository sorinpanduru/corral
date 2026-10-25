package proxyserver_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// holds is a Hold callback: it records the points reached and blocks each until
// the test releases it.
type holds struct {
	mu      sync.Mutex
	reached []string
	gates   map[string]chan struct{}
	arrived map[string]chan struct{}
}

func newHolds(points ...string) *holds {
	h := &holds{gates: map[string]chan struct{}{}, arrived: map[string]chan struct{}{}}
	for _, p := range points {
		h.gates[p] = make(chan struct{})
		h.arrived[p] = make(chan struct{})
	}
	return h
}

func (h *holds) hook(ctx context.Context, point string) {
	h.mu.Lock()
	h.reached = append(h.reached, point)
	g, ok := h.gates[point]
	a := h.arrived[point]
	h.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-a:
	default:
		close(a)
	}
	select {
	case <-g:
	case <-ctx.Done():
	}
}

func (h *holds) wait(t *testing.T, point string) {
	t.Helper()
	h.mu.Lock()
	a := h.arrived[point]
	h.mu.Unlock()
	select {
	case <-a:
	case <-time.After(10 * time.Second):
		t.Fatalf("hold point %q never reached", point)
	}
}

func (h *holds) release(point string) {
	h.mu.Lock()
	g := h.gates[point]
	h.mu.Unlock()
	select {
	case <-g:
	default:
		close(g)
	}
}

func (h *holds) order() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reached...)
}

func findSlow(id int32, marker string) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "corraltestMarker", Value: marker}}})
}

func shutdownAsync(r *rig, ctx context.Context) chan error {
	ch := make(chan error, 1)
	go func() { ch <- r.srv.Shutdown(ctx) }()
	return ch
}

func waitErr(t *testing.T, ch chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not finish", what)
		return nil
	}
}

// AC-26 (unit), FR-47: the whole sequence with hold points. During the delay
// the server keeps accepting and serving; after the listener closes new
// connections are refused; an idle connection is closed with no reply; an
// exchange in progress completes; a message first read after step 3 starts gets
// 91; drain expiry cuts the rest off.
func TestShutdownSequence_AC26(t *testing.T) {
	h := newHolds(proxyserver.HoldShutdownDelay, proxyserver.HoldListenerClosed, proxyserver.HoldStep3, proxyserver.HoldDrainExpired)
	r := newRig(t, opts{hold: h.hook, delay: 200 * time.Millisecond, drain: 600 * time.Millisecond})
	idle := r.dial()
	roundTrip(t, idle, hello(1))
	busy := r.dial()
	late := r.dial()
	roundTrip(t, late, hello(1))
	stuck := r.dial()
	r.twin.AddRule(mongosim.Match{Marker: "finishes"}, mongosim.Delay(500*time.Millisecond))
	stuckRule := r.twin.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	busy.Send(findSlow(1, "finishes"))
	stuck.Send(findSlow(1, "stuck"))
	if !stuckRule.WaitHeld(1, 5*time.Second) {
		t.Fatal("the stuck request never reached the upstream")
	}

	done := shutdownAsync(r, context.Background())
	h.wait(t, proxyserver.HoldShutdownDelay)
	// Step 1: still serving, including new connections.
	fresh := r.dial()
	if m := roundTrip(t, fresh, hello(7)); m.Header.ResponseTo != 7 {
		t.Fatal("not served during the listener-close delay")
	}
	h.release(proxyserver.HoldShutdownDelay)

	// Step 2 done: the listener is closed.
	h.wait(t, proxyserver.HoldListenerClosed)
	eventually(t, "new connections refused", func() bool {
		c, err := net.DialTimeout("tcp", r.addr(), 300*time.Millisecond)
		if err == nil {
			c.Close()
			return false
		}
		return true
	})
	h.release(proxyserver.HoldListenerClosed)

	// Step 3, before idle connections are closed: a message first read now is
	// late and gets 91, then the connection closes.
	h.wait(t, proxyserver.HoldStep3)
	m := roundTrip(t, late, ping(5))
	if m.Docs[0].Lookup("code").AsInt64() != 91 || m.Docs[0].Lookup("codeName").StringValue() != "ShutdownInProgress" {
		t.Fatalf("late message: %v", m.Docs[0])
	}
	if _, err := late.ReadMessage(5 * time.Second); err == nil {
		t.Fatal("a connection that got 91 stayed open")
	}
	h.release(proxyserver.HoldStep3)

	// Idle connections are closed with no reply.
	if err := idle.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("idle connection: %v", err)
	}
	// An exchange in progress completes under the normal rules.
	mb, err := busy.ReadMessage(10 * time.Second)
	if err != nil || mb.Header.ResponseTo != 1 {
		t.Fatalf("the exchange in progress did not complete: %v", err)
	}
	// Step 4: the drain expires and cuts off the stuck exchange.
	h.wait(t, proxyserver.HoldDrainExpired)
	h.release(proxyserver.HoldDrainExpired)
	if err := stuck.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("a connection still open after the drain: %v", err)
	}
	if err := waitErr(t, done, "Shutdown"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	stuckRule.Release()
	eventually(t, "the stuck upstream connection discarded", func() bool {
		for _, x := range r.pobs.reasons() {
			if x == "client_abandoned" || x == "shutdown" || x == "error" {
				return true
			}
		}
		return false
	})
	order := h.order()
	want := []string{proxyserver.HoldShutdownDelay, proxyserver.HoldListenerClosed, proxyserver.HoldStep3, proxyserver.HoldDrainExpired}
	if len(order) < len(want) {
		t.Fatalf("hold points reached: %v", order)
	}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("hold point order %v, want %v", order, want)
		}
	}
	_ = fresh
}

// FR-47: with no delay, Shutdown completes quickly when everything is idle.
func TestShutdownIdleCompletes_AC26(t *testing.T) {
	r := newRig(t, opts{})
	c := r.dial()
	roundTrip(t, c, hello(1))
	ch := shutdownAsync(r, context.Background())
	if err := waitErr(t, ch, "Shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := c.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("idle connection not closed: %v", err)
	}
}

// FR-63 (AC-47 unit): with the drain held open, ForceDrainEnd closes the
// remaining connections at once; a second call is a no-op.
func TestForceDrainEnd_AC47(t *testing.T) {
	h := newHolds(proxyserver.HoldDrain)
	r := newRig(t, opts{hold: h.hook, drain: time.Hour})
	rule := r.twin.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	stuck := r.dial()
	stuck.Send(findSlow(1, "stuck"))
	if !rule.WaitHeld(1, 5*time.Second) {
		t.Fatal("request not held upstream")
	}
	done := shutdownAsync(r, context.Background())
	h.wait(t, proxyserver.HoldDrain)
	// The drain is open: the exchange in progress is still alive.
	if _, err := stuck.ReadMessage(300 * time.Millisecond); err == nil {
		t.Fatal("reply during the held drain")
	}
	begin := time.Now()
	r.srv.ForceDrainEnd()
	r.srv.ForceDrainEnd() // a second call is a no-op
	h.release(proxyserver.HoldDrain)
	if err := stuck.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("remaining connection not closed: %v", err)
	}
	if err := waitErr(t, done, "Shutdown"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("the forced drain took %v", d)
	}
	rule.Release()
	r.srv.ForceDrainEnd() // and after the end
}

// FR-63 (R-4): a forced end during the listener-close delay skips the rest of
// the delay.
func TestForceDrainEndDuringDelay_FR63(t *testing.T) {
	h := newHolds(proxyserver.HoldShutdownDelay)
	r := newRig(t, opts{hold: h.hook, delay: time.Hour, drain: time.Hour})
	c := r.dial()
	roundTrip(t, c, hello(1))
	done := shutdownAsync(r, context.Background())
	h.wait(t, proxyserver.HoldShutdownDelay)
	r.srv.ForceDrainEnd()
	h.release(proxyserver.HoldShutdownDelay)
	if err := waitErr(t, done, "Shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := c.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("connection not closed: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", r.addr(), 300*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the listener is still open after a forced end")
	}
}

// Serve returns when its context ends, and the listener is released.
func TestServeReturnsOnContextCancel_FR43(t *testing.T) {
	r := newRig(t, opts{})
	r.cancel()
	select {
	case <-r.served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	_ = proxy.CloseShutdown
}
