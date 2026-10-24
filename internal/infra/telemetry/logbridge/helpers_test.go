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
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// AC-5: every record goes through internal/testkit/logcheck (allowed keys, no
// groups, exactly one component), teed to a buffer holding the real JSON.

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

// capture applies the configured level (logcheck itself is enabled at every
// level) and delegates the AC-5 checks, recording and JSON tee to logcheck.
type capture struct {
	lc *logcheck.Handler
	lv slog.Leveler
}

func (c *capture) Enabled(ctx context.Context, l slog.Level) bool { return l >= c.lv.Level() }

func (c *capture) WithAttrs(as []slog.Attr) slog.Handler {
	return &capture{lc: c.lc.WithAttrs(as).(*logcheck.Handler), lv: c.lv}
}

func (c *capture) WithGroup(name string) slog.Handler {
	return &capture{lc: c.lc.WithGroup(name).(*logcheck.Handler), lv: c.lv}
}

func (c *capture) Handle(ctx context.Context, r slog.Record) error { return c.lc.Handle(ctx, r) }

type env struct {
	t   *testing.T
	f   *logging.Factory
	lv  *slog.LevelVar
	lc  *logcheck.Handler
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
	lc := logcheck.New(t, logcheck.Tee(buf, logging.FormatJSON))
	c := &capture{lc: lc, lv: lv}
	return &env{t: t, f: logging.NewFactory(c), lv: lv, lc: lc, buf: buf, clk: fakeclock.New(start)}
}

func (e *env) records() []rec {
	var out []rec
	for _, r := range e.lc.Records() {
		attrs := map[string]slog.Value{}
		for _, a := range r.Attrs {
			attrs[a.Key] = a.Value.Resolve()
		}
		out = append(out, rec{Level: r.Level, Msg: r.Msg, Attrs: attrs})
	}
	return out
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
