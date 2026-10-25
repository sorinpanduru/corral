package proxyserver

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

// AC-60 (FR-66, FR-47, FR-63), unit: the accept loop with an injected listener
// and a fake clock. Transient errors are retried after exactly 5 ms doubling to
// 1 s with no jitter; the listener closed by shutdown is not an error; a
// permanent error ends Serve at once with that error and no further accept.

// scriptListener returns scripted results from Accept, then delegates to the
// real listener (which blocks until a client connects).
type scriptListener struct {
	net.Listener
	mu     sync.Mutex
	script []error // non-nil: return it; nil: delegate
	calls  atomic.Int32
}

func (l *scriptListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	l.mu.Lock()
	var step error
	delegate := true
	if len(l.script) > 0 {
		step, l.script = l.script[0], l.script[1:]
		delegate = step == nil
	}
	l.mu.Unlock()
	if !delegate {
		return nil, step
	}
	return l.Listener.Accept()
}

func (l *scriptListener) push(steps ...error) {
	l.mu.Lock()
	l.script = append(l.script, steps...)
	l.mu.Unlock()
}

type countObs struct {
	proxy.Noop
	rejected atomic.Int32
}

func (o *countObs) ConnRejected(context.Context, proxy.RejectReason) { o.rejected.Add(1) }

func fakeClockServer(t *testing.T, script ...error) (*Server, *scriptListener, *fakeclock.Clock, *countObs) {
	t.Helper()
	clk := fakeclock.New(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	obs := &countObs{}
	srv := New(Options{Addr: "127.0.0.1:0", MaxConns: 4, IOTimeout: time.Second, Obs: obs, Clock: clk,
		Shutdown: proxy.NewShutdown(0, time.Second, clk), IDs: proxy.NewConnIDs()})
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	sl := &scriptListener{Listener: srv.ln, script: script}
	srv.ln = sl
	return srv, sl, clk, obs
}

func waitCalls(t *testing.T, sl *scriptListener, n int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for sl.calls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("Accept was called %d times, want %d", sl.calls.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// retryAfter asserts that, from the moment a transient error was returned
// (Accept call number errCall), the next Accept happens after exactly d.
func retryAfter(t *testing.T, sl *scriptListener, clk *fakeclock.Clock, errCall int32, d time.Duration) {
	t.Helper()
	waitCalls(t, sl, errCall)
	if !clk.WaitForTimers(1, 3*time.Second) {
		t.Fatalf("no backoff timer armed after Accept call %d", errCall)
	}
	clk.Advance(d - time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if got := sl.calls.Load(); got != errCall {
		t.Fatalf("after %v of a %v backoff Accept had been called again (%d calls, want %d)", d-time.Millisecond, d, got, errCall)
	}
	clk.Advance(time.Millisecond)
	waitCalls(t, sl, errCall+1)
}

type tempErr struct{}

func (tempErr) Error() string   { return "synthetic temporary" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "synthetic timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func opErr(e error) error { return &net.OpError{Op: "accept", Net: "tcp", Err: e} }

func TestAcceptBackoffExactDelays_AC60(t *testing.T) {
	emfile := opErr(syscall.EMFILE)
	srv, sl, clk, obs := fakeClockServer(t, emfile, emfile, emfile, nil, emfile, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	// EMFILE three times: 5, 10 and 20 ms.
	retryAfter(t, sl, clk, 1, 5*time.Millisecond)
	retryAfter(t, sl, clk, 2, 10*time.Millisecond)
	retryAfter(t, sl, clk, 3, 20*time.Millisecond)
	// Then a connection is accepted and served.
	c, err := net.DialTimeout("tcp", srv.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitCalls(t, sl, 5) // the connection was accepted; the loop accepts again (EMFILE)
	// The next transient error after that success is retried after 5 ms again.
	retryAfter(t, sl, clk, 5, 5*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v: a transient error must not end it", err)
	default:
	}
	if obs.rejected.Load() != 0 {
		t.Fatal("a transient accept error was reported as a rejected connection")
	}
}

func TestAcceptBackoffReachesOneSecondCap_AC60(t *testing.T) {
	var script []error
	for i := 0; i < 14; i++ {
		script = append(script, opErr(syscall.ENFILE))
	}
	srv, sl, clk, _ := fakeClockServer(t, script...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)
	want := []time.Duration{5, 10, 20, 40, 80, 160, 320, 640}
	for i, ms := range want {
		retryAfter(t, sl, clk, int32(i+1), ms*time.Millisecond)
	}
	for i := len(want); i < 13; i++ { // 1 s from the ninth retry on, and it stays there
		retryAfter(t, sl, clk, int32(i+1), time.Second)
	}
}

func TestEveryTransientKindRetriedAfter5ms_AC60(t *testing.T) {
	for name, e := range map[string]error{
		"timeout": timeoutErr{}, "temporary": tempErr{},
		"ENFILE": opErr(syscall.ENFILE), "ENOBUFS": opErr(syscall.ENOBUFS), "ENOMEM": opErr(syscall.ENOMEM),
		"ECONNABORTED": opErr(syscall.ECONNABORTED), "EINTR": opErr(syscall.EINTR), "EMFILE": opErr(syscall.EMFILE),
	} {
		t.Run(name, func(t *testing.T) {
			srv, sl, clk, _ := fakeClockServer(t, e, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx) }()
			retryAfter(t, sl, clk, 1, 5*time.Millisecond)
			select {
			case err := <-done:
				t.Fatalf("Serve returned %v", err)
			default:
			}
		})
	}
}

// A listener closed by the shutdown sequence (step 2), and the force-end of a
// second signal, are not errors: Serve returns nil.
func TestListenerClosedByShutdownIsNotAnError_AC60(t *testing.T) {
	for _, mode := range []string{"shutdown", "second signal"} {
		t.Run(mode, func(t *testing.T) {
			srv, _, _, _ := fakeClockServer(t)
			done := make(chan error, 1)
			go func() { done <- srv.Serve(context.Background()) }()
			time.Sleep(30 * time.Millisecond)
			if mode == "second signal" {
				srv.ForceDrainEnd()
			}
			srv.closeListener() // what shutdown step 2 (and a forced end) does to the listener
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Serve returned %v after the listener was closed by shutdown", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return")
			}
		})
	}
}

// A permanent error (EBADF, EINVAL, anything not transient) ends Serve at once
// with that error, and no further accept is attempted.
func TestPermanentAcceptErrorEndsServe_AC60(t *testing.T) {
	for name, e := range map[string]error{"EBADF": opErr(syscall.EBADF), "EINVAL": opErr(syscall.EINVAL), "other": errors.New("synthetic permanent")} {
		t.Run(name, func(t *testing.T) {
			srv, sl, clk, _ := fakeClockServer(t, e)
			done := make(chan error, 1)
			go func() { done <- srv.Serve(context.Background()) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Serve returned nil for a permanent error")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Serve did not return")
			}
			clk.Advance(10 * time.Second)
			time.Sleep(50 * time.Millisecond)
			if n := sl.calls.Load(); n != 1 {
				t.Fatalf("Accept was called %d times, want 1 (no further accept)", n)
			}
		})
	}
}
