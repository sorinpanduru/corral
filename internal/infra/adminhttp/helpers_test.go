package adminhttp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/infra/adminhttp"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// Expected API (plans/SPEC-001.md T17):
//
//	adminhttp.Options{Addr string; Gatherer prometheus.Gatherer;
//	    Readiness health.Readiness; ErrorLog *log.Logger}
//	adminhttp.New(Options) *Server
//	(*Server).Listen() error      binds Options.Addr; failure is a
//	                              *config.Error for "admin.listen"
//	(*Server).Serve() error       blocks; nil or http.ErrServerClosed after
//	                              Shutdown
//	(*Server).Addr() net.Addr     bound address after Listen
//	(*Server).MarkStarted(), MarkStopping()
//	(*Server).Handler() http.Handler
//	(*Server).Shutdown(ctx) error
//
// Round 1 additions: Options.IdleTimeout and Options.ReadTimeout
// (time.Duration, zero = default, for tests); exported constants IdleTimeout
// (60s), ReadTimeout (10s), WriteTimeout (30s), MaxHeaderBytes (16<<10). Hint:
// "OPTIONS *" needs http.Server.DisableGeneralOptionsHandler = true.
//
// Beyond that these tests assume: an empty Options.Addr means the FR-16
// default 127.0.0.1:9187, a nil Readiness means "no check registered", and a
// nil ErrorLog is allowed.

const loopback = "127.0.0.1:0"

var (
	t0           = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	allowAll     = "GET, HEAD"
	textPlainUTF = "text/plain; charset=utf-8"
)

// marker returns a random synthetic marker, unique per call.
func marker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return "MK" + hex.EncodeToString(b)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logs wires logcheck (AC-5) behind the logging factory and returns the
// admin ErrorLog built by logbridge (FR-28), plus the checker and the JSON tee.
type logs struct {
	lc  *logcheck.Handler
	buf *syncBuf
	l   *log.Logger
	f   *logging.Factory
}

func newLogs(tb testing.TB) *logs {
	tb.Helper()
	buf := new(syncBuf)
	lc := logcheck.New(tb, logcheck.Tee(buf, logging.FormatJSON))
	f := logging.NewFactory(lc)
	clk := fakeclock.New(t0)
	return &logs{lc: lc, buf: buf, f: f, l: logbridge.NewHTTPErrorLog(f, clk.Now)}
}

// fakeReadiness is a health.Readiness fake counting its calls.
type fakeReadiness struct {
	calls  atomic.Int64
	mu     sync.Mutex
	status health.Status
	panicV any
}

func (f *fakeReadiness) set(s health.Status) {
	f.mu.Lock()
	f.status = s
	f.mu.Unlock()
}

func (f *fakeReadiness) Readiness(context.Context) health.Status {
	f.calls.Add(1)
	f.mu.Lock()
	s, p := f.status, f.panicV
	f.mu.Unlock()
	if p != nil {
		panic(p)
	}
	return s
}

// newRegistry returns a private registry with go_/process_ collectors and the
// catalog metrics (including corral_tracing_spans_dropped_total).
func newRegistry(tb testing.TB) *prometheus.Registry {
	tb.Helper()
	reg := metrics.NewRegistry()
	c, err := metrics.LoadCatalog()
	if err != nil {
		tb.Fatal(err)
	}
	f, err := metrics.NewFactory(reg, c)
	if err != nil {
		tb.Fatal(err)
	}
	if err := metrics.RegisterAll(f); err != nil {
		tb.Fatal(err)
	}
	return reg
}

type running struct {
	srv  *adminhttp.Server
	base string
	logs *logs
}

// start builds a server on 127.0.0.1:0, listens, serves and registers cleanup.
// It does not call MarkStarted.
func start(tb testing.TB, ready health.Readiness, reg prometheus.Gatherer) *running {
	tb.Helper()
	return startOpts(tb, adminhttp.Options{Readiness: ready, Gatherer: reg})
}

// startOpts is start with caller-supplied Options. Addr defaults to loopback
// port 0, Gatherer to a fresh private registry and ErrorLog to the logbridge
// logger wired through logcheck, unless set.
func startOpts(tb testing.TB, o adminhttp.Options) *running {
	tb.Helper()
	lg := newLogs(tb)
	if o.Gatherer == nil {
		o.Gatherer = newRegistry(tb)
	}
	if o.Addr == "" {
		o.Addr = loopback
	}
	if o.ErrorLog == nil {
		o.ErrorLog = lg.l
	}
	srv := adminhttp.New(o)
	if err := srv.Listen(); err != nil {
		tb.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			tb.Error("Serve did not return after Shutdown")
		}
	})
	return &running{srv: srv, base: "http://" + srv.Addr().String(), logs: lg}
}

// listenOnly listens (no Serve) and always releases the port in Cleanup.
func listenOnly(tb testing.TB, o adminhttp.Options) *adminhttp.Server {
	tb.Helper()
	srv := adminhttp.New(o)
	if err := srv.Listen(); err != nil {
		tb.Fatalf("Listen: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// rawConn dials the server for byte-level HTTP tests.
func rawConn(tb testing.TB, r *running) net.Conn {
	tb.Helper()
	c, err := net.Dial("tcp", r.srv.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	return c
}

// waitClosed reports whether the server closes c within d (reading and
// discarding anything it sends first).
func waitClosed(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 512)
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}

// failGatherer is a prometheus.Gatherer whose error carries a marker and a
// control character.
type failGatherer struct{ err error }

func (f failGatherer) Gather() ([]*dto.MetricFamily, error) { return nil, f.err }

// do sends one request and returns status, headers and the body.
func (r *running) do(tb testing.TB, method, path string, hdr map[string]string) (int, http.Header, string) {
	tb.Helper()
	req, err := http.NewRequest(method, r.base+path, nil)
	if err != nil {
		tb.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cl := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := cl.Do(req)
	if err != nil {
		tb.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func (r *running) get(tb testing.TB, path string) (int, http.Header, string) {
	tb.Helper()
	return r.do(tb, http.MethodGet, path, nil)
}

func isClosed(err error) bool {
	return err == nil || errors.Is(err, http.ErrServerClosed)
}

func noControl(tb testing.TB, what, s string) {
	tb.Helper()
	if strings.ContainsAny(s, "\x1b\r") {
		tb.Fatalf("%s contains control characters: %q", what, s)
	}
}

func newClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}
