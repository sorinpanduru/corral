//go:build twin

package twin

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

const flushBound = 15 * time.Second // SPEC-001 FR-34 flush bound plus tolerance

func refusesDial(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		conn.Close()
		return false
	}
	return true
}

// AC-26 (FR-47): the shutdown sequence, step by step.
func TestShutdownSequence_AC26(t *testing.T) {
	c, tw := startMongos(t, opts{
		set:    map[string]string{"pool.max_size": "1", "pool.checkout_timeout": "60s", "proxy.shutdown.listener_close_delay": "1500ms", "proxy.shutdown.drain_timeout": "20s"},
		holdAt: []string{"shutdown_listener_closed", "shutdown_step3"},
	})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "drain-hold"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "drain-hold")) // exchange in progress at step 3
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	b := c.dial()
	b.Send(find(1, "drain-queued")) // waiting in the queue at step 3
	idle := c.dial()
	rt(t, idle, cmd(1, "hello", 1, "$db", "admin")) // local: the pool is held
	idle2 := c.dial()
	rt(t, idle2, cmd(1, "hello", 1, "$db", "admin"))
	time.Sleep(200 * time.Millisecond)
	c.p.Signal(syscall.SIGTERM)
	c.waitReadyz(503, "shutting_down")
	// During listener_close_delay: new connections are accepted and served.
	during := c.dial()
	if m := rt(t, during, cmd(2, "hello", 1, "$db", "admin")); !isOK(m) { // answered locally: the pool is held
		t.Fatalf("a connection during the delay: %v", doc(m))
	}
	if err := c.p.WaitHeld("shutdown_listener_closed", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if !refusesDial(c.proxy) {
		t.Fatal("new connections are accepted after step 2")
	}
	c.p.Release("shutdown_listener_closed")
	if err := c.p.WaitHeld("shutdown_step3", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	// With the hook holding step 3, a request on a not-yet-closed connection
	// gets 91, then the connection closes.
	if m := rt(t, idle, find(3, "late")); code(m) != 91 {
		t.Fatalf("request during step 3: %v", doc(m))
	}
	closedNoBytes(t, idle)
	c.p.Release("shutdown_step3")
	// An idle connection is closed with no reply.
	closedNoBytes(t, idle2)
	// The exchange in progress and the queued request complete normally, and
	// each connection is closed after its reply.
	hold.Release()
	for name, conn := range map[string]*rawwire.Conn{"in progress": a, "queued": b} {
		m, err := conn.ReadMessage(15 * time.Second)
		if err != nil || !isOK(m) {
			t.Fatalf("%s exchange: %v %v", name, err, m)
		}
		closedNoBytes(t, conn)
	}
	if code, err := c.p.Wait(flushBound); err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
}

// AC-26: after drain_timeout a held exchange is cut off; exit 0 within the
// bound.
func TestShutdownDrainTimeoutCutsOff_AC26(t *testing.T) {
	c, tw := startMongos(t, opts{set: map[string]string{"proxy.shutdown.drain_timeout": "1s"}})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "stuck"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	begin := time.Now()
	c.p.Signal(syscall.SIGTERM)
	closedNoBytes(t, a)
	if d := time.Since(begin); d < 800*time.Millisecond || d > 6*time.Second {
		t.Fatalf("the held exchange was cut off after %v, want about 1s", d)
	}
	if code, err := c.p.Wait(flushBound); err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	if d := time.Since(begin); d > 1*time.Second+flushBound {
		t.Fatalf("exit after %v", d)
	}
}

// AC-47 (FR-63): a second SIGTERM during a held drain ends it at once with one
// warn record and exit 0; a third signal changes nothing.
func TestSecondSignalEndsDrain_AC47(t *testing.T) {
	c, tw := startMongos(t, opts{holdAt: []string{"shutdown_drain"}, set: map[string]string{"proxy.shutdown.drain_timeout": "10m"}})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	a := c.dial()
	a.Send(find(1, "stuck"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	c.p.Signal(syscall.SIGTERM)
	if err := c.p.WaitHeld("shutdown_drain", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	c.p.Signal(syscall.SIGTERM)
	c.p.Signal(syscall.SIGTERM) // a third signal changes nothing
	code, err := c.p.Wait(flushBound)
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	if d := time.Since(begin); d > flushBound {
		t.Fatalf("exit after %v", d)
	}
	if n := len(c.records("shutdown forced by second signal")); n != 1 {
		t.Fatalf("%d forced-shutdown records, want 1", n)
	}
	r := c.records("shutdown forced by second signal")[0]
	if r.Component() != "lifecycle" || r["level"] != "WARN" {
		t.Fatalf("record %v", r)
	}
}

// AC-53 (FR-63; R-4): a second signal during listener_close_delay skips the
// rest of the delay and the drain.
func TestSecondSignalDuringListenerCloseDelay_AC53(t *testing.T) {
	c, tw := startMongos(t, opts{
		holdAt: []string{"shutdown_pool_close"},
		set:    map[string]string{"proxy.shutdown.listener_close_delay": "60s"},
	})
	c.waitReady()
	hold := tw.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	c.p.Signal(syscall.SIGTERM)
	c.waitReadyz(503, "shutting_down")
	during := c.dial() // still accepted during the delay
	if m := rt(t, during, ping(1)); !isOK(m) {
		t.Fatalf("%v", doc(m))
	}
	a := c.dial()
	a.Send(find(1, "stuck"))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("not held")
	}
	time.Sleep(time.Second)
	begin := time.Now()
	c.p.Signal(syscall.SIGTERM)
	if err := c.p.WaitHeld("shutdown_pool_close", 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if !refusesDial(c.proxy) {
		t.Fatal("a new connection was accepted after the second signal")
	}
	closedNoBytes(t, a)
	c.p.Release("shutdown_pool_close")
	code, err := c.p.Wait(flushBound)
	if err != nil || code != 0 {
		t.Fatalf("exit %d, %v", code, err)
	}
	if d := time.Since(begin); d > flushBound {
		t.Fatalf("exit after %v", d)
	}
	if n := len(c.records("shutdown forced by second signal")); n != 1 {
		t.Fatalf("%d forced-shutdown records, want 1", n)
	}
}
