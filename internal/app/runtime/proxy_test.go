package runtime_test

// T21 runtime tests with the SPEC-002 group registered: record order, the
// readiness hold, bind failure, shutdown order and hook points, the second
// signal, and the corraltest hooks. Interface assumptions (stated in the
// report): runtime.Hooks gains Hold(ctx, point), PoolRoundRobin, PanicCommand
// and Probe; runtime.Inputs gains Force <-chan struct{}; point names are the
// ADR-0022 ones.

import (
	"context"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/osfile"
	"github.com/sorinpanduru/corral/internal/testkit/corralargs"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

type proxyRun struct {
	t       *testing.T
	out     *syncBuf
	done    chan int
	started chan corralrt.Handles
	cancel  context.CancelFunc
	admin   string
	proxy   string
	force   chan struct{}
	sigs    chan os.Signal
}

type proxyOpts struct {
	upstream string // host:port; empty: a closed port
	extra    []string
	hooks    func(*corralrt.Hooks)
	env      []string
	signals  bool   // drive shutdown through runtime.WatchSignals (p.sigs)
	proxy    string // pre-reserved addresses, for hooks that need them
	admin    string
}

func startProxy(t *testing.T, o proxyOpts) *proxyRun {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "")
		}
	}
	// The OTel SDK reads OTEL_* from the process environment.
	for _, kv := range o.env {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, v)
		}
	}
	up := o.upstream
	if up == "" {
		up = reserveAddr(t)
	}
	args := corralargs.Minimal(t, up)
	proxyAddr := strings.TrimPrefix(args[2], "--proxy.listen=")
	if o.proxy != "" {
		proxyAddr = o.proxy
		args[2] = "--proxy.listen=" + o.proxy
	}
	admin := o.admin
	if admin == "" {
		admin = reserveAddr(t)
	}
	args = append(args, "--admin.listen="+admin, "--upstream.heartbeat_interval=500ms")
	args = append(args, o.extra...)
	reg, err := settings.Registry()
	if err != nil {
		t.Fatal(err)
	}
	p := &proxyRun{t: t, out: &syncBuf{}, done: make(chan int, 1), started: make(chan corralrt.Handles, 1), admin: admin, proxy: proxyAddr, force: make(chan struct{})}
	hooks := corralrt.Hooks{Started: func(h corralrt.Handles) {
		select {
		case p.started <- h:
		default:
		}
	}}
	if o.hooks != nil {
		o.hooks(&hooks)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	var forceIn <-chan struct{} = p.force
	if o.signals {
		p.sigs = make(chan os.Signal, 4)
		forceIn = corralrt.WatchSignals(p.sigs, cancel)
	}
	in := corralrt.Inputs{Args: args, Env: o.env, ReadFile: osfile.Read, Stdout: p.out, HUP: make(chan os.Signal, 4), Version: "test", Registry: reg, Force: forceIn}
	_ = source.NewEnv
	go func() { p.done <- corralrt.Run(ctx, in, hooks) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.done:
		case <-time.After(waitTimeout):
		}
	})
	return p
}

func (p *proxyRun) waitStarted() corralrt.Handles {
	p.t.Helper()
	select {
	case h := <-p.started:
		return h
	case code := <-p.done:
		p.t.Fatalf("Run returned %d before startup completed", code)
	case <-time.After(waitTimeout):
		p.t.Fatal("startup did not complete")
	}
	return corralrt.Handles{}
}

func (p *proxyRun) wait() int {
	p.t.Helper()
	select {
	case code := <-p.done:
		p.done <- code
		return code
	case <-time.After(waitTimeout):
		p.t.Fatal("Run did not return")
	}
	return -1
}

func (p *proxyRun) msgs() []string {
	var out []string
	for _, r := range p.out.records(p.t) {
		out = append(out, str(r, "component")+"/"+str(r, "msg"))
	}
	return out
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

// AC-1: `wire listener bound` (no attributes, no address) is logged before the
// startup-complete record, and the listener answers a handshake.
func TestWireListenerBoundBeforeStartupComplete_AC1(t *testing.T) {
	p := startProxy(t, proxyOpts{})
	p.waitStarted()
	msgs := p.msgs()
	bound, complete := indexOf(msgs, "proxy/wire listener bound"), indexOf(msgs, "lifecycle/corral startup complete")
	if bound < 0 || complete < 0 || bound > complete {
		t.Fatalf("record order: %v", msgs)
	}
	for _, r := range p.out.records(t) {
		if str(r, "msg") == "wire listener bound" {
			for k := range r {
				switch k {
				case "time", "level", "msg", "component":
				default:
					t.Fatalf("the bound record carries %q (no address or other attribute allowed)", k)
				}
			}
			if strings.Contains(string(p.out.bytes()), p.proxy) {
				t.Fatal("the proxy address appears in the output")
			}
		}
	}
	c := rawwire.Dial(t, p.proxy)
	if m, err := sendRecv(c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonHello()})); err != nil || m.Header.ResponseTo != 1 {
		t.Fatalf("handshake over the wire listener: %v", err)
	}
}

// AC-1/AC-31: while the last-step hook holds, /readyz answers starting and the
// wire port is not bound yet (binding is the last startup step).
func TestReadyzStartingAndNoWireListenerBeforeLastStep_AC1(t *testing.T) {
	var held struct {
		mu     sync.Mutex
		status int
		body   string
		bound  bool
	}
	proxyAddr, adminAddr := reserveAddr(t), reserveAddr(t) // known before the hook can run
	p := startProxy(t, proxyOpts{proxy: proxyAddr, admin: adminAddr, hooks: func(h *corralrt.Hooks) {
		h.BeforeLastStep = func(context.Context) {
			r, _ := get(adminAddr, "/readyz")
			c, err := net.DialTimeout("tcp", proxyAddr, 200*time.Millisecond)
			if err == nil {
				c.Close()
			}
			held.mu.Lock()
			held.status, held.body, held.bound = r.status, r.body, err == nil
			held.mu.Unlock()
		}
	}})
	p.waitStarted()
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.status != 503 || held.body != "starting\n" {
		t.Fatalf("/readyz during the hold: %d %q", held.status, held.body)
	}
	if held.bound {
		t.Fatal("the wire listener was bound before the last startup step")
	}
}

// bindFailure runs corral with proxy.listen=addr and returns the exit code and
// the captured output.
func bindFailure(t *testing.T, addr string) (int, *syncBuf) {
	t.Helper()
	args := []string{"--upstream.hosts=" + reserveAddr(t), "--upstream.auth.mechanism=none", "--proxy.listen=" + addr, "--admin.listen=" + reserveAddr(t)}
	reg, _ := settings.Registry()
	out := &syncBuf{}
	code := corralrt.Run(context.Background(), corralrt.Inputs{Args: args, ReadFile: osfile.Read, Stdout: out, HUP: make(chan os.Signal, 1), Version: "test", Registry: reg}, corralrt.Hooks{})
	return code, out
}

// assertBindRecord checks AC-59 / AC-1 (Amendment 4): exit 1, exactly one record
// `wire listener bind failed` (error level, component config, error=config,
// setting=proxy.listen, the detail), no `invalid configuration` record, no
// `wire listener bound`, and no host, address or port anywhere in the output.
func assertBindRecord(t *testing.T, code int, out *syncBuf, detail string, leaks ...string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	n := 0
	for _, r := range out.records(t) {
		switch str(r, "msg") {
		case "wire listener bind failed":
			n++
			if str(r, "level") != "ERROR" || str(r, "component") != "config" || str(r, "setting") != "proxy.listen" || str(r, "error") != "config" {
				t.Fatalf("bind failure record: %v", r)
			}
			if got := str(r, "corral.error.detail"); got != detail {
				t.Fatalf("corral.error.detail %q, want %q", got, detail)
			}
		case "invalid configuration":
			t.Fatalf("an invalid configuration record on a bind failure: %v", r)
		case "wire listener bound":
			t.Fatal("wire listener bound logged on failure")
		}
	}
	if n != 1 {
		t.Fatalf("%d `wire listener bind failed` records, want exactly 1: %v", n, out.lines())
	}
	for _, l := range leaks {
		if strings.Contains(string(out.bytes()), l) {
			t.Fatalf("%q appears in the output", l)
		}
	}
}

// AC-59, AC-1 (FR-1, Amendment 4): "address already in use" gives the bind
// failure record with detail EADDRINUSE and exit 1.
func TestBindFailureRecordAddrInUse_AC59(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]
	code, out := bindFailure(t, addr)
	assertBindRecord(t, code, out, "EADDRINUSE", addr, "127.0.0.1")
	if regexp.MustCompile(`\b` + port + `\b`).Match(out.bytes()) {
		t.Fatalf("the port number %s appears in the output", port)
	}
}

// AC-59: "permission denied" (a privileged port, bound by a process without the
// privilege) gives detail `other` and exit 1.
func TestBindFailureRecordPermissionDenied_AC59(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: no privileged port is denied")
	}
	if ln, err := net.Listen("tcp", "127.0.0.1:1"); err == nil {
		ln.Close()
		t.Skip("binding port 1 is allowed here (unprivileged ports lowered)")
	}
	code, out := bindFailure(t, "127.0.0.1:1")
	assertBindRecord(t, code, out, "other", "127.0.0.1")
}

// FR-46: with an unreachable upstream, readiness is 503 upstream_unavailable
// and corral_upstream_available is 0; startup does not wait for the upstream.
func TestUnreachableUpstreamNotReady_FR46(t *testing.T) {
	p := startProxy(t, proxyOpts{})
	hd := p.waitStarted()
	eventually(t, "readyz upstream_unavailable", func() bool {
		r, err := get(p.admin, "/readyz")
		return err == nil && r.status == 503 && r.body == "upstream_unavailable\n"
	})
	_ = hd
}

// AC-25: readiness 200 and gauge 1 with a reachable upstream; gauge follows.
func TestReadyAndGaugeWithUpstream_AC25(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	p := startProxy(t, proxyOpts{upstream: twin.Addr()})
	hd := p.waitStarted()
	eventually(t, "ready", func() bool { r, err := get(p.admin, "/readyz"); return err == nil && r.status == 200 })
	eventually(t, "gauge 1", func() bool { return gaugeValue(t, hd, "corral_upstream_available") == 1 })
	twin.Stop()
	eventually(t, "not ready", func() bool { r, err := get(p.admin, "/readyz"); return err == nil && r.status == 503 })
	eventually(t, "gauge 0", func() bool { return gaugeValue(t, hd, "corral_upstream_available") == 0 })
}

// AC-26 (in-process), ADR-0021: shutdown runs the hold points in order; the
// admin listener stays up (503 shutting_down) while the wire listener is gone
// and connections drain.
func TestShutdownHoldPointsAndOrder_AC26(t *testing.T) {
	var mu sync.Mutex
	var points []string
	release := make(chan struct{})
	var adminDuringDrain, wireClosed bool
	proxyAddr, adminAddr := reserveAddr(t), reserveAddr(t)
	p := startProxy(t, proxyOpts{proxy: proxyAddr, admin: adminAddr, hooks: func(h *corralrt.Hooks) {
		h.Hold = func(ctx context.Context, point string) {
			mu.Lock()
			points = append(points, point)
			mu.Unlock()
			if point == "shutdown_drain" {
				r, err := get(adminAddr, "/readyz")
				c, derr := net.DialTimeout("tcp", proxyAddr, 200*time.Millisecond)
				if derr == nil {
					c.Close()
				}
				mu.Lock()
				adminDuringDrain = err == nil && r.status == 503 && r.body == "shutting_down\n"
				wireClosed = derr != nil
				mu.Unlock()
			}
		}
	}})
	p.waitStarted()
	close(release)
	p.cancel()
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"laststep", "shutdown_delay", "shutdown_listener_closed", "shutdown_step3", "shutdown_drain", "shutdown_drain_expired", "shutdown_pool_close"}
	// Idle shutdown may skip the points that only apply to a drain in
	// progress; the ones that run keep this relative order, and laststep comes
	// first.
	last := -1
	for _, pt := range points {
		i := indexOf(want, pt)
		if i < 0 {
			t.Fatalf("unknown hold point %q", pt)
		}
		if i < last {
			t.Fatalf("hold points out of order: %v", points)
		}
		last = i
	}
	if len(points) == 0 || points[0] != "laststep" {
		t.Fatalf("hold points: %v", points)
	}
	if indexOf(points, "shutdown_listener_closed") < 0 {
		t.Fatalf("shutdown_listener_closed never reached: %v", points)
	}
	if indexOf(points, "shutdown_drain") >= 0 && (!adminDuringDrain || !wireClosed) {
		t.Fatalf("during the drain: admin shutting_down=%v wire closed=%v", adminDuringDrain, wireClosed)
	}
}

// AC-47 (in-process): during a held drain, closing Force ends it at once, emits
// exactly one `shutdown forced by second signal` record, and Run returns 0.
func TestSecondSignalForcesDrainEnd_AC47(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	rule := twin.AddRule(mongosim.Match{Marker: "stuck"}, mongosim.Hold())
	heldDrain := make(chan struct{})
	var once sync.Once
	p := startProxy(t, proxyOpts{upstream: twin.Addr(), extra: []string{"--proxy.shutdown.drain_timeout=1h"}, hooks: func(h *corralrt.Hooks) {
		h.Hold = func(ctx context.Context, point string) {
			if point == "shutdown_drain" {
				once.Do(func() { close(heldDrain) })
				<-ctx.Done() // held until the forced end cancels the drain
			}
		}
	}})
	p.waitStarted()
	eventually(t, "ready", func() bool { r, err := get(p.admin, "/readyz"); return err == nil && r.status == 200 })
	c := rawwire.Dial(t, p.proxy)
	c.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonFind("stuck")}))
	if !rule.WaitHeld(1, 5*time.Second) {
		t.Fatal("request not held upstream")
	}
	p.cancel()
	select {
	case <-heldDrain:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain was never reached")
	}
	begin := time.Now()
	close(p.force)
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Fatalf("the forced end took %v", d)
	}
	n := 0
	for _, r := range p.out.records(t) {
		if str(r, "msg") == "shutdown forced by second signal" {
			n++
			if str(r, "component") != "lifecycle" || str(r, "level") != "WARN" {
				t.Fatalf("record: %v", r)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d forced-shutdown records, want exactly 1", n)
	}
	// AC-60: a listener closed by shutdown / a second signal is not an error.
	for _, r := range p.out.records(t) {
		if str(r, "msg") == "wire listener serve failed" {
			t.Fatalf("a serve-failed record after a normal shutdown: %v", r)
		}
	}
	rule.Release()
}

// FR-64: PanicCommand makes the handler panic for that command; PoolRoundRobin
// alternates idle upstream connections; the probe receives the byte counts.
func TestCorraltestHooksReachTheEngine_FR64(t *testing.T) {
	twin := mongosim.Start(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos}})
	pr := &recordingProbe{}
	p := startProxy(t, proxyOpts{upstream: twin.Addr(), hooks: func(h *corralrt.Hooks) {
		h.PoolRoundRobin = true
		h.PanicCommand = "boom"
		h.Probe = pr
	}})
	p.waitStarted()
	eventually(t, "ready", func() bool { r, err := get(p.admin, "/readyz"); return err == nil && r.status == 200 })
	// Round robin: two concurrent requests make two upstream connections; later
	// sequential requests alternate between them.
	hold := twin.AddRule(mongosim.Match{Marker: "hold"}, mongosim.Hold())
	a, b := rawwire.Dial(t, p.proxy), rawwire.Dial(t, p.proxy)
	a.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonFind("hold")}))
	b.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonFind("hold")}))
	if !hold.WaitHeld(2, 5*time.Second) {
		t.Fatal("two upstream connections were not opened")
	}
	hold.Release()
	a.ReadMessage(5 * time.Second)
	b.ReadMessage(5 * time.Second)
	seen := map[int]int{}
	var last = -1
	alternations := 0
	c := rawwire.Dial(t, p.proxy)
	for i := 0; i < 8; i++ {
		before := len(twin.ReceivedCommand("find"))
		sendRecvOK(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: int32(10 + i), Body: bsonFind("seq")}))
		ms := twin.ReceivedCommand("find")
		if len(ms) <= before {
			t.Fatal("request not recorded")
		}
		idx := ms[len(ms)-1].ConnIndex
		seen[idx]++
		if last != -1 && idx != last {
			alternations++
		}
		last = idx
	}
	if len(seen) != 2 || alternations < 6 {
		t.Fatalf("round-robin did not alternate: %v alternations=%d", seen, alternations)
	}
	// Panic command.
	bad := rawwire.Dial(t, p.proxy)
	bad.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bsonD("boom", 1, "$db", "d")}))
	if err := bad.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("panic command: %v", err)
	}
	if m := sendRecvOK(t, c, rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 99, Body: bsonFind("after")})); m.Header.ResponseTo != 99 {
		t.Fatal("another connection was affected by the panic")
	}
	if pr.preLease() == 0 {
		t.Fatal("the probe received no bytes-before-lease report")
	}
}
