package runtime_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// AC-60 (FR-66, FR-47, FR-63, FR-49), startup path and shutdown orchestration
// with an injected listener that fails permanently. The exit status is the
// value Run returns.

// faultListener behaves like the real listener until fail is closed, then
// Accept returns a permanent error (EBADF).
type faultListener struct {
	net.Listener
	fail  chan struct{}
	once  sync.Once
	calls atomic.Int32
}

func newFault() *faultListener { return &faultListener{fail: make(chan struct{})} }

func (l *faultListener) arm() { l.once.Do(func() { close(l.fail) }) }

func (l *faultListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	select {
	case <-l.fail:
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EBADF}
	default:
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := l.Listener.Accept(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-l.fail:
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EBADF}
	}
}

func wrapWith(l *faultListener) func(*corralrt.Hooks) {
	return func(h *corralrt.Hooks) {
		h.WrapWireListener = func(inner net.Listener) net.Listener { l.Listener = inner; return l }
	}
}

func portOf(addr string) string { return addr[strings.LastIndex(addr, ":")+1:] }

// A permanent accept error starts the shutdown at once (the 60 s listener-close
// delay is not waited), drains, closes the pools, flushes tracing and exits 1;
// the one record has no attribute beyond error and corral.error.detail, and
// the port never appears.
func TestPermanentAcceptErrorShutsDownWithoutDelay_AC60(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	hold := twin.AddRule(mongosim.Match{Marker: "held"}, mongosim.Hold())
	// A plain HTTP sink for OTLP/HTTP exports. The in-process gRPC collector is
	// not used: its server races with Run's gRPC log bridge under -race.
	var exports atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n, _ := io.Copy(io.Discard, r.Body); n > 0 {
			exports.Add(1)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	fl := newFault()
	proxyAddr, adminAddr := reserveAddr(t), reserveAddr(t)
	p := startProxy(t, proxyOpts{upstream: twin.Addr(), proxy: proxyAddr, admin: adminAddr, hooks: wrapWith(fl),
		extra: []string{"--proxy.shutdown.listener_close_delay=60s", "--tracing.enabled=true"},
		env:   []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + sink.URL, "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"}})
	p.waitStarted()
	eventually(t, "ready", func() bool { r, err := get(adminAddr, "/readyz"); return err == nil && r.status == 200 })

	busy := rawwire.Dial(t, proxyAddr)
	busy.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonFind("held")}))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("the exchange was not held")
	}
	idle := rawwire.Dial(t, proxyAddr)
	if _, err := sendRecv(idle, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonHello()})); err != nil {
		t.Fatal(err)
	}

	begin := time.Now()
	fl.arm()
	// No delay: readiness reports shutting_down at once, not after 60 s.
	eventually(t, "readyz shutting_down", func() bool {
		r, err := get(adminAddr, "/readyz")
		return err == nil && r.status == 503 && r.body == "shutting_down\n"
	})
	if d := time.Since(begin); d > 10*time.Second {
		t.Fatalf("shutting_down after %v: the listener-close delay was waited", d)
	}
	// The idle connection is closed at once, with no reply.
	if err := idle.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("idle connection: %v", err)
	}
	// The exchange in progress completes normally, then its connection closes.
	hold.Release()
	m, err := busy.ReadMessage(10 * time.Second)
	if err != nil || m.Header.ResponseTo != 1 {
		t.Fatalf("the exchange in progress did not complete: %v", err)
	}
	if err := busy.ExpectClosedNoBytes(10 * time.Second); err != nil {
		t.Fatalf("the connection after its reply: %v", err)
	}
	if code := p.wait(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if d := time.Since(begin); d > 30*time.Second {
		t.Fatalf("exit after %v, well before the 60 s delay was expected", d)
	}
	calls := fl.calls.Load()
	time.Sleep(100 * time.Millisecond)
	if fl.calls.Load() != calls {
		t.Fatal("an accept was attempted after the permanent error")
	}
	// Exactly one record, with only error and corral.error.detail.
	n := 0
	for _, r := range p.out.records(t) {
		if str(r, "msg") != "wire listener serve failed" {
			continue
		}
		n++
		if str(r, "level") != "ERROR" || str(r, "component") != "lifecycle" || str(r, "error") == "" || str(r, "corral.error.detail") == "" {
			t.Fatalf("record %v", r)
		}
		for k := range r {
			switch k {
			case "time", "level", "msg", "component", "error", "corral.error.detail":
			default:
				t.Fatalf("the record carries %q", k)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d `wire listener serve failed` records, want 1", n)
	}
	if regexp.MustCompile(`\b` + portOf(proxyAddr) + `\b`).Match(p.out.bytes()) {
		t.Fatal("the port number appears in the output")
	}
	// Pool and monitoring connections are closed (FR-47 step 5)...
	eventually(t, "every upstream connection closed", func() bool {
		for _, ci := range twin.Conns() {
			if ci.Open {
				return false
			}
		}
		return len(twin.Conns()) > 0
	})
	// ...and the tracing flush ran (SPEC-001 FR-34): the exchange's spans were
	// exported before the process exited.
	if exports.Load() == 0 {
		t.Fatal("no span export reached the OTLP sink: the tracing flush did not run")
	}
}

// A second signal during that shutdown ends the drain at once but the exit
// status stays 1; one record of each kind.
func TestSecondSignalAfterPermanentErrorExits1_AC60(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	hold := twin.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	t.Cleanup(hold.Release)
	fl := newFault()
	proxyAddr, adminAddr := reserveAddr(t), reserveAddr(t)
	p := startProxy(t, proxyOpts{upstream: twin.Addr(), proxy: proxyAddr, admin: adminAddr, hooks: wrapWith(fl),
		extra: []string{"--proxy.shutdown.drain_timeout=10m"}})
	p.waitStarted()
	eventually(t, "ready", func() bool { r, err := get(adminAddr, "/readyz"); return err == nil && r.status == 200 })
	c := rawwire.Dial(t, proxyAddr)
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonFind("stuck")}))
	if !hold.WaitHeld(1, 10*time.Second) {
		t.Fatal("the exchange was not held")
	}
	fl.arm()
	eventually(t, "readyz shutting_down", func() bool { r, err := get(adminAddr, "/readyz"); return err == nil && r.body == "shutting_down\n" })
	begin := time.Now()
	close(p.force) // the second signal
	code := p.wait()
	if code != 1 {
		t.Fatalf("exit %d, want 1 (not 0) after a second signal during the failure shutdown", code)
	}
	if time.Since(begin) > 15*time.Second {
		t.Fatalf("the second signal did not end the drain at once (%v)", time.Since(begin))
	}
	count := map[string]int{}
	for _, r := range p.out.records(t) {
		switch str(r, "msg") {
		case "shutdown forced by second signal", "wire listener serve failed":
			count[str(r, "msg")]++
		}
	}
	if count["shutdown forced by second signal"] != 1 || count["wire listener serve failed"] != 1 {
		t.Fatalf("records %v, want exactly one of each", count)
	}
}
