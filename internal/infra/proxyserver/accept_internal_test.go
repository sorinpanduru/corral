package proxyserver

import (
	"context"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
)

// failingListener returns the given errors from the first Accept calls, then
// delegates.
type failingListener struct {
	net.Listener
	errs  []error
	calls atomic.Int32
}

func (l *failingListener) Accept() (net.Conn, error) {
	if n := int(l.calls.Add(1)); n <= len(l.errs) {
		return nil, l.errs[n-1]
	}
	return l.Listener.Accept()
}

func newTestServer(t *testing.T, errs ...error) (*Server, *failingListener) {
	t.Helper()
	srv := New(Options{Addr: "127.0.0.1:0", MaxConns: 1, IOTimeout: time.Second,
		Shutdown: proxy.NewShutdown(0, time.Second, clock.System()), IDs: proxy.NewConnIDs()})
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	fl := &failingListener{Listener: srv.ln, errs: errs}
	srv.ln = fl
	return srv, fl
}

// A transient accept error (EMFILE) does not stop the listener: Serve backs off
// and keeps accepting (RC3).
func TestServeSurvivesTransientAcceptError(t *testing.T) {
	srv, fl := newTestServer(t, &net.OpError{Op: "accept", Err: syscall.EMFILE})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	c, err := net.DialTimeout("tcp", srv.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for fl.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fl.calls.Load() < 2 {
		t.Fatalf("Accept calls = %d: the listener stopped after EMFILE", fl.calls.Load())
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v after a transient error", err)
	default:
	}
}

// A permanent accept error is returned to the caller.
func TestServeReturnsPermanentAcceptError(t *testing.T) {
	srv, _ := newTestServer(t, &net.OpError{Op: "accept", Err: syscall.EINVAL})
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
}
