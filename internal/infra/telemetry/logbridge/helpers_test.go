package logbridge_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

// TODO(T4 retrofit): internal/testkit/logcheck was not merged when these
// tests were written. capture below enforces the same AC-5 rules it will
// (allowed keys via logging.AllowedKey, no groups, exactly one component) and
// should be replaced by logcheck.New(t, logcheck.Tee(...)) once available.

var start = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// marker returns a random synthetic marker, unique per call.
func marker(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "MK" + hex.EncodeToString(b)
}

type rec struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

func (r rec) component() string { return r.Attrs["component"].String() }

func (r rec) suppressed(t testing.TB) int64 {
	t.Helper()
	v, ok := r.Attrs["suppressed_count"]
	if !ok {
		t.Fatalf("record %q has no suppressed_count", r.Msg)
	}
	if v.Kind() != slog.KindInt64 {
		t.Fatalf("suppressed_count kind = %v, want int64", v.Kind())
	}
	return v.Int64()
}

type store struct {
	mu   sync.Mutex
	recs []rec
}

type capture struct {
	tb    testing.TB
	st    *store
	lv    slog.Leveler
	pre   []slog.Attr
	inner slog.Handler // real JSON handler, to scan the rendered output
}

func (c *capture) Enabled(_ context.Context, l slog.Level) bool { return l >= c.lv.Level() }

func (c *capture) WithAttrs(as []slog.Attr) slog.Handler {
	n := *c
	n.pre = append(append([]slog.Attr{}, c.pre...), as...)
	n.inner = c.inner.WithAttrs(as)
	return &n
}

func (c *capture) WithGroup(name string) slog.Handler {
	c.tb.Errorf("AC-5: group %q used", name)
	return c
}

func (c *capture) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]slog.Value{}
	comps := 0
	add := func(a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			c.tb.Errorf("AC-5: group attribute %q", a.Key)
			return
		}
		if !logging.AllowedKey(a.Key) {
			c.tb.Errorf("AC-5: key %q is not allowed", a.Key)
		}
		if a.Key == "component" {
			comps++
		}
		attrs[a.Key] = a.Value.Resolve()
	}
	for _, a := range c.pre {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	if comps != 1 {
		c.tb.Errorf("AC-5: record %q has %d component attributes, want 1", r.Message, comps)
	}
	c.st.mu.Lock()
	c.st.recs = append(c.st.recs, rec{Level: r.Level, Msg: r.Message, Attrs: attrs})
	c.st.mu.Unlock()
	return c.inner.Handle(ctx, r)
}

type env struct {
	t   *testing.T
	f   *logging.Factory
	lv  *slog.LevelVar
	st  *store
	buf *syncBuf
	clk *fakeclock.Clock
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

func newEnv(t *testing.T, level slog.Level) *env {
	t.Helper()
	lv := new(slog.LevelVar)
	lv.Set(level)
	buf := new(syncBuf)
	c := &capture{tb: t, st: new(store), lv: lv, inner: logging.NewHandler(buf, logging.FormatJSON, lv)}
	return &env{t: t, f: logging.NewFactory(c), lv: lv, st: c.st, buf: buf, clk: fakeclock.New(start)}
}

func (e *env) records() []rec {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	return append([]rec(nil), e.st.recs...)
}

func (e *env) only() rec {
	e.t.Helper()
	rs := e.records()
	if len(rs) != 1 {
		e.t.Fatalf("got %d records, want 1: %+v", len(rs), rs)
	}
	return rs[0]
}

func (e *env) count() int { return len(e.records()) }

// last returns the most recent record.
func (e *env) last() rec {
	e.t.Helper()
	rs := e.records()
	if len(rs) == 0 {
		e.t.Fatal("no records")
	}
	return rs[len(rs)-1]
}

// noMarker proves the marker is in no record (message, keys, values) and not
// in the rendered JSON output (AC-26, AC-29, FR-28).
func (e *env) noMarker(m string) {
	e.t.Helper()
	for _, r := range e.records() {
		if strings.Contains(r.Msg, m) {
			e.t.Errorf("marker in message %q", r.Msg)
		}
		for k, v := range r.Attrs {
			if strings.Contains(k, m) || strings.Contains(v.String(), m) || strings.Contains(fmt.Sprint(v.Any()), m) {
				e.t.Errorf("marker in attribute %q=%v", k, v)
			}
		}
	}
	if strings.Contains(e.buf.String(), m) {
		e.t.Errorf("marker in rendered output: %s", e.buf.String())
	}
}

// expect asserts level, component, a non-empty message and suppressed_count.
func expect(t testing.TB, r rec, lvl slog.Level, comp string, suppressed int64) {
	t.Helper()
	if r.Level != lvl {
		t.Errorf("level = %v, want %v", r.Level, lvl)
	}
	if got := r.component(); got != comp {
		t.Errorf("component = %q, want %q", got, comp)
	}
	if r.Msg == "" {
		t.Error("empty message")
	}
	if got := r.suppressed(t); got != suppressed {
		t.Errorf("suppressed_count = %d, want %d", got, suppressed)
	}
}
