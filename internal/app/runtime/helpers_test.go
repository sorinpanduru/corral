package runtime_test

// Test helpers for the T18 runtime suite. Synthetic data only, loopback only.
// API expected (plans/SPEC-001.md T18):
//
//	runtime.Run(ctx, args []string, env tracing.LookupEnv, stdout io.Writer, hooks Hooks) int
//	runtime.Hooks{BeforeLastStep func(ctx context.Context); Started func(Handles)}
//	runtime.Handles{AdminAddr net.Addr; TracerProvider trace.TracerProvider; Gatherer prometheus.Gatherer}

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corralrt "github.com/sorinpanduru/corral/internal/app/runtime"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

const (
	waitTimeout = 20 * time.Second
	plainUTF8   = "text/plain; charset=utf-8"
)

// syncBuf is a goroutine-safe io.Writer that keeps everything written.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// lines returns the non-empty output lines.
func (s *syncBuf) lines() []string {
	var out []string
	for _, l := range strings.Split(string(s.bytes()), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// jsonRecords parses every line as a JSON object (the default format). A
// non-JSON line is reported through tb.
func jsonRecords(tb testing.TB, lines []string) []map[string]any {
	tb.Helper()
	var out []map[string]any
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			tb.Errorf("stdout line is not a JSON object (len %d)", len(l))
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *syncBuf) records(tb testing.TB) []map[string]any {
	tb.Helper()
	return jsonRecords(tb, s.lines())
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// byComponent returns the records whose component equals c.
func byComponent(recs []map[string]any, c logging.Component) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if str(r, "component") == string(c) {
			out = append(out, r)
		}
	}
	return out
}

func lifecycleMsgs(recs []map[string]any) []string {
	var out []string
	for _, r := range byComponent(recs, logging.Lifecycle) {
		out = append(out, str(r, "msg"))
	}
	return out
}

// hasMsg reports whether a lifecycle record whose message contains frag
// (case-insensitive) exists.
func hasLifecycle(recs []map[string]any, frag string) bool {
	for _, m := range lifecycleMsgs(recs) {
		if strings.Contains(strings.ToLower(m), frag) {
			return true
		}
	}
	return false
}

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// marker returns a fresh synthetic marker of 16+ bytes in [A-Za-z0-9_-].
func marker(tb testing.TB, prefix string) string {
	tb.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// reserveAddr returns a loopback host:port that is free right now.
func reserveAddr(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("reserve: %v", err)
	}
	a := ln.Addr().String()
	_ = ln.Close()
	return a
}

var noKeepAlive = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true},
}

type resp struct {
	status int
	body   string
	ctype  string
}

func get(addr, path string) (resp, error) {
	r, err := noKeepAlive.Get("http://" + addr + path)
	if err != nil {
		return resp{}, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	return resp{r.StatusCode, string(b), r.Header.Get("Content-Type")}, nil
}

// harness runs corralrt.Run in a goroutine.
type harness struct {
	tb           testing.TB
	cancel       context.CancelFunc
	done         chan int
	out          *syncBuf
	started      chan corralrt.Handles
	hooks        corralrt.Hooks // the hooks given to Run (Started wrapped)
	startedCalls func() int
}

type runOpts struct {
	args   []string
	env    map[string]string
	before func(ctx context.Context)
	out    *syncBuf // optional, so hooks can read it before run returns
}

func run(tb testing.TB, o runOpts) *harness {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if o.out == nil {
		o.out = &syncBuf{}
	}
	h := &harness{
		tb: tb, cancel: cancel, done: make(chan int, 1),
		out: o.out, started: make(chan corralrt.Handles, 1),
	}
	var mu sync.Mutex
	calls := 0
	h.startedCalls = func() int { mu.Lock(); defer mu.Unlock(); return calls }
	hooks := corralrt.Hooks{
		BeforeLastStep: o.before,
		Started: func(hd corralrt.Handles) {
			mu.Lock()
			calls++
			mu.Unlock()
			select {
			case h.started <- hd:
			default:
			}
		},
	}
	env := envMap(o.env)
	go func() { h.done <- corralrt.Run(ctx, o.args, env, h.out, hooks) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitTimeout):
		}
	})
	return h
}

func (h *harness) waitStarted() corralrt.Handles {
	h.tb.Helper()
	select {
	case hd := <-h.started:
		return hd
	case code := <-h.done:
		h.tb.Fatalf("Run returned %d before the Started hook; stdout: %d lines", code, len(h.out.lines()))
	case <-time.After(waitTimeout):
		h.tb.Fatalf("timed out waiting for the Started hook")
	}
	return corralrt.Handles{}
}

// stop cancels the context (graceful shutdown) and returns the exit code.
func (h *harness) stop() int {
	h.tb.Helper()
	h.cancel()
	return h.wait()
}

func (h *harness) wait() int {
	h.tb.Helper()
	select {
	case code := <-h.done:
		h.done <- code // keep for cleanup
		return code
	case <-time.After(waitTimeout):
		h.tb.Fatalf("Run did not return in time")
	}
	return -1
}

// eventually polls cond until it holds or the timeout expires.
func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for: %s", what)
}

func readResponse(tb testing.TB, c net.Conn) resp {
	tb.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		tb.Fatalf("read response: %v", err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	return resp{r.StatusCode, string(b), r.Header.Get("Content-Type")}
}
