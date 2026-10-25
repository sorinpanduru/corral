package upstream_test

// Test harness for the upstream monitor: a scripted Checker driven by the
// fake clock. Interface assumptions beyond the plan's text (stated in the
// test-writer report):
//   - upstream.Noop is a struct type (upstream.Noop{}) implementing Observer,
//     and can be embedded to override single methods.
//   - Constants: upstream.Ready/Wait/FailFast (AdmissionKind),
//     upstream.Forwarded/Barrier (ReplySource), upstream.TargetChanged,
//     HostNotPrimary, HostUnknown, BecameIncompatible, AvailabilityChanged
//     (ChangeKind). Reason tokens are compared as strings.
//   - upstream.AuthFailed is a struct with an exported field Err (the cause).

import (
	"context"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

var t0 = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func eid(n byte) [12]byte { return [12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, n} }

func params(minW, maxW int32) upstream.Params {
	return upstream.Params{MaxBsonObjectSize: 16777216, MaxMessageSizeBytes: 48000000, MaxWriteBatchSize: 100000,
		LogicalSessionTimeoutMinutes: 30, HasLSTM: true, MinWire: minW, MaxWire: maxW}
}

func primary(host, set string, hosts ...string) upstream.Report {
	return upstream.Report{Host: host, IsWritablePrimary: true, SetName: set, HasSetName: true, Hosts: hosts, Primary: host,
		ElectionID: eid(1), HasElectionID: true, SetVersion: 1, HasSetVersion: true, Params: params(0, 21)}
}

func primaryAt(host, set string, e byte, sv int64, hosts ...string) upstream.Report {
	r := primary(host, set, hosts...)
	r.ElectionID, r.SetVersion = eid(e), sv
	return r
}

func secondary(host, set, primaryHost string, hosts ...string) upstream.Report {
	return upstream.Report{Host: host, Secondary: true, SetName: set, HasSetName: true, Hosts: hosts, Primary: primaryHost, Params: params(0, 21)}
}

func mongos(host string) upstream.Report {
	return upstream.Report{Host: host, IsMongos: true, IsWritablePrimary: true, Params: params(0, 21)}
}

func standalone(host string) upstream.Report {
	return upstream.Report{Host: host, IsWritablePrimary: true, Params: params(0, 21)}
}

func netErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func rsCfg(seeds ...string) upstream.Config {
	return upstream.Config{Seeds: seeds, ReplicaSet: "rs0", HasReplicaSet: true, HeartbeatInterval: 10 * time.Second}
}

func mongosCfg(seed string) upstream.Config {
	return upstream.Config{Seeds: []string{seed}, HeartbeatInterval: 10 * time.Second}
}

type call struct {
	Host string
	Req  upstream.CheckRequest
	At   time.Time
}

// fakeChecker answers from a table that the test edits between steps. A host
// with no entry is unreachable.
type fakeChecker struct {
	clk *fakeclock.Clock

	mu    sync.Mutex
	table map[string]upstream.Report
	fail  map[string]error
	hook  func(upstream.CheckRequest)
	calls []call
}

func newChecker(clk *fakeclock.Clock) *fakeChecker {
	return &fakeChecker{clk: clk, table: map[string]upstream.Report{}, fail: map[string]error{}}
}

func (f *fakeChecker) set(r upstream.Report) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.table[r.Host] = r
	delete(f.fail, r.Host)
}

func (f *fakeChecker) down(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[host] = netErr()
}

// failWith makes checks of host return err (any error type).
func (f *fakeChecker) failWith(host string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[host] = err
}

func (f *fakeChecker) Check(ctx context.Context, req upstream.CheckRequest) upstream.Report {
	f.mu.Lock()
	hook := f.hook
	f.calls = append(f.calls, call{req.Host, req, f.clk.Now()})
	f.mu.Unlock()
	if hook != nil {
		hook(req) // may block; the answer is read afterwards
	}
	f.mu.Lock()
	r, ok := f.table[req.Host]
	err := f.fail[req.Host]
	f.mu.Unlock()
	if err != nil || !ok {
		if err == nil {
			err = netErr()
		}
		return upstream.Report{Host: req.Host, Err: err}
	}
	return r
}

func (f *fakeChecker) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeChecker) of(host string) []call {
	var out []call
	for _, c := range f.all() {
		if c.Host == host {
			out = append(out, c)
		}
	}
	return out
}

type change struct{ c upstream.Change }

type harness struct {
	t   *testing.T
	clk *fakeclock.Clock
	chk *fakeChecker
	m   *upstream.Monitor
	rec *recorder

	mu      sync.Mutex
	changes []upstream.Change
}

type ctxKey struct{}

func newHarness(t *testing.T, cfg upstream.Config, setup func(*fakeChecker), noop bool) *harness {
	t.Helper()
	clk := fakeclock.New(t0)
	chk := newChecker(clk)
	if setup != nil {
		setup(chk)
	}
	h := &harness{t: t, clk: clk, chk: chk}
	var obs upstream.Observer = upstream.Noop{}
	if !noop {
		h.rec = &recorder{}
		obs = h.rec
	}
	h.m = upstream.NewMonitor(cfg, chk, clk, obs)
	if h.rec != nil {
		h.rec.m = h.m
	}
	h.m.OnChange(func(c upstream.Change) {
		h.mu.Lock()
		h.changes = append(h.changes, c)
		h.mu.Unlock()
	})
	return h
}

func (h *harness) start() *harness {
	h.t.Helper()
	ctx := context.WithValue(context.Background(), ctxKey{}, "start-ctx")
	h.m.Start(ctx)
	h.t.Cleanup(func() { h.m.Stop(context.Background()) })
	return h
}

func (h *harness) changeKinds() []upstream.Change {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]upstream.Change(nil), h.changes...)
}

// eventually polls until cond holds (real time, bounded).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) waitState(what string, f func(upstream.State) bool) upstream.State {
	h.t.Helper()
	var s upstream.State
	eventually(h.t, what, func() bool { s = h.m.State(); return f(s) })
	return s
}

func (h *harness) waitCount(host string, n int) {
	h.t.Helper()
	eventually(h.t, "check count", func() bool { return len(h.chk.of(host)) >= n })
}

// quiet lets any in-flight work settle, in real time.
func (h *harness) quiet() { time.Sleep(40 * time.Millisecond) }

// armed waits until the monitor has a timer pending, then advances the fake
// clock by d.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clk.WaitForTimers(1, 2*time.Second)
	h.clk.Advance(d)
}

func (h *harness) readiness() health.Status { return h.m.Readiness(context.Background()) }

// recorder is an Observer that records calls and re-enters the monitor from
// inside callbacks (they must run outside the monitor's lock).
type recorder struct {
	upstream.Noop
	m *upstream.Monitor

	mu       sync.Mutex
	avail    []health.Reason
	availOK  []bool
	incompat []string
	primary  []string
	marked   []markedCall
	ctxs     []context.Context
	availCnt int
}

type markedCall struct {
	Host     string
	Code     int32
	CodeName string
}

func (r *recorder) AvailabilityChanged(ctx context.Context, ready bool, reason health.Reason) {
	if r.m != nil {
		_ = r.m.State() // re-entrancy: must not deadlock
		_ = r.m.Readiness(ctx)
	}
	r.mu.Lock()
	r.availOK = append(r.availOK, ready)
	r.avail = append(r.avail, reason)
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
}
func (r *recorder) Incompatible(ctx context.Context, reason upstream.Reason, host string) {
	if r.m != nil {
		_ = r.m.State()
	}
	r.mu.Lock()
	r.incompat = append(r.incompat, string(reason)+"@"+host)
	r.mu.Unlock()
}
func (r *recorder) Available(ctx context.Context) {
	r.mu.Lock()
	r.availCnt++
	r.mu.Unlock()
}
func (r *recorder) PrimaryChanged(ctx context.Context, host string) {
	r.mu.Lock()
	r.primary = append(r.primary, host)
	r.mu.Unlock()
}
func (r *recorder) TargetMarkedNotPrimary(ctx context.Context, host string, code int32, codeName string) {
	if r.m != nil {
		_ = r.m.State()
	}
	r.mu.Lock()
	r.marked = append(r.marked, markedCall{host, code, codeName})
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
}

func (r *recorder) snapshotMarked() []markedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]markedCall(nil), r.marked...)
}

// stepUntil advances the fake clock in small steps until host has been checked
// at least n times, so a test does not depend on the exact spacing (a failing
// authentication state delays checks by the reconnect backoff).
func (h *harness) stepUntil(host string, n int) {
	h.t.Helper()
	for i := 0; i < 400 && len(h.chk.of(host)) < n; i++ {
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(250 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	h.waitCount(host, n)
}

// stepUntilState steps the clock until cond holds.
func (h *harness) stepUntilState(what string, cond func(upstream.State) bool) upstream.State {
	h.t.Helper()
	var s upstream.State
	for i := 0; i < 400; i++ {
		if s = h.m.State(); cond(s) {
			return s
		}
		h.clk.WaitForTimers(1, time.Second)
		h.clk.Advance(250 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out stepping the clock until %s", what)
	return s
}
