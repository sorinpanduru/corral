// Package logcheck provides a slog.Handler for tests that enforces the
// SPEC-001 log conventions (AC-5, FR-7, FR-39) and records what was logged.
// It is test infrastructure and must not be imported by production code.
package logcheck

import (
	"context"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// Record is one captured log record. Attrs holds the attributes bound with
// With first, then the record's own attributes.
type Record struct {
	Level slog.Level
	Msg   string
	Attrs []slog.Attr
}

// Option configures a Handler.
type Option func(*shared)

// Tee also writes every record to w in format f, so tests can inspect real
// JSON or text output. The tee never filters by level.
func Tee(w io.Writer, f logging.Format) Option {
	return func(s *shared) {
		s.tee = logging.NewHandler(w, f, slog.Level(math.MinInt))
	}
}

// BehindCorrelation declares that the handler is installed behind
// logging.WithTraceCorrelation. trace_id and span_id are then allowed once
// each, and only when the record's context holds a valid span context that
// they match. Without this option any trace_id or span_id fails (FR-8, FR-39).
func BehindCorrelation() Option {
	return func(s *shared) { s.behindCorrelation = true }
}

type shared struct {
	tb                testing.TB
	tee               slog.Handler
	behindCorrelation bool

	mu      sync.Mutex
	records []Record
}

// Handler is a slog.Handler that fails the bound test on convention
// violations. It is safe for concurrent use.
type Handler struct {
	s     *shared
	attrs []slog.Attr // attributes bound with WithAttrs, already checked
	tee   slog.Handler
}

// New returns a Handler reporting violations through tb.
func New(tb testing.TB, opts ...Option) *Handler {
	s := &shared{tb: tb}
	for _, o := range opts {
		o(s)
	}
	return &Handler{s: s, tee: s.tee}
}

// Enabled is true at every level so no record escapes the check.
func (h *Handler) Enabled(context.Context, slog.Level) bool { return true }

// Records returns a snapshot of the captured records.
func (h *Handler) Records() []Record {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	out := make([]Record, len(h.s.records))
	for i, r := range h.s.records {
		out[i] = Record{Level: r.Level, Msg: r.Msg, Attrs: append([]slog.Attr(nil), r.Attrs...)}
	}
	return out
}

func (h *Handler) fail(format string, args ...any) {
	h.s.tb.Helper()
	// Errorf, not Fatalf: Handle may run on a goroutine other than the test's.
	h.s.tb.Errorf("logcheck: "+format, args...)
}

// WithAttrs binds attrs; they are checked when bound and again per record.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	for _, a := range as {
		h.checkAttr(a, false)
	}
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	if h.tee != nil {
		c.tee = h.tee.WithAttrs(as)
	}
	return &c
}

// WithGroup always fails: groups are not allowed (FR-39).
func (h *Handler) WithGroup(name string) slog.Handler {
	h.fail("slog group %q is not allowed (FR-39)", name)
	c := *h
	if h.tee != nil {
		c.tee = h.tee.WithGroup(name)
	}
	return &c
}

// checkAttr validates one attribute. Trace keys are validated in Handle.
func (h *Handler) checkAttr(a slog.Attr, inRecord bool) {
	h.s.tb.Helper()
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		h.fail("slog group value %q is not allowed (FR-39)", a.Key)
		return
	}
	if !logging.AllowedKey(a.Key) {
		h.fail("key %q is outside the FR-39 allowlist", a.Key)
		return
	}
	if (a.Key == attr.TraceID || a.Key == attr.SpanID) && (!inRecord || !h.s.behindCorrelation) {
		h.fail("callers must not set %q (FR-8, FR-39)", a.Key)
		return
	}
	if logging.NumericKey(a.Key) {
		switch v.Kind() {
		case slog.KindInt64, slog.KindUint64, slog.KindFloat64:
		default:
			h.fail("key %q needs a numeric value, got kind %v (FR-39)", a.Key, v.Kind())
		}
	}
}

// Handle checks and records r.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	h.s.tb.Helper()
	all := append([]slog.Attr(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		h.checkAttr(a, true)
		all = append(all, slog.Attr{Key: a.Key, Value: a.Value.Resolve()})
		return true
	})
	// Re-resolve bound attrs for the stored copy.
	for i := range h.attrs {
		all[i].Value = all[i].Value.Resolve()
	}
	h.checkComponent(all)
	if h.s.behindCorrelation {
		h.checkTraceIDs(ctx, all)
	}

	h.s.mu.Lock()
	h.s.records = append(h.s.records, Record{Level: r.Level, Msg: r.Message, Attrs: all})
	h.s.mu.Unlock()

	if h.tee != nil {
		return h.tee.Handle(ctx, r)
	}
	return nil
}

func (h *Handler) checkComponent(all []slog.Attr) {
	h.s.tb.Helper()
	found := false
	for _, a := range all {
		if a.Key != attr.Component {
			continue
		}
		found = true
		if a.Value.Kind() != slog.KindString || !logging.ValidComponent(logging.Component(a.Value.String())) {
			h.fail("component %v is outside the FR-7 set", a.Value)
		}
	}
	if !found {
		h.fail("record has no component (FR-7)")
	}
}

// checkTraceIDs enforces that, behind the correlation wrapper, each id
// appears exactly when the context has a valid span context, once, with the
// matching value. Caller-set ids make a key appear twice or without context.
func (h *Handler) checkTraceIDs(ctx context.Context, all []slog.Attr) {
	h.s.tb.Helper()
	sc := trace.SpanContextFromContext(ctx)
	for _, key := range []string{attr.TraceID, attr.SpanID} {
		n := 0
		var val slog.Value
		for _, a := range all {
			if a.Key == key {
				n++
				val = a.Value
			}
		}
		if n == 0 {
			continue
		}
		want := ""
		if sc.IsValid() {
			want = sc.TraceID().String()
			if key == attr.SpanID {
				want = sc.SpanID().String()
			}
		}
		switch {
		case n > 1:
			h.fail("%s appears %d times; only the correlation wrapper may add it", key, n)
		case !sc.IsValid():
			h.fail("%s present but the record context has no valid span context", key)
		case val.Kind() != slog.KindString || val.String() != want:
			h.fail("%s does not match the record context's span context", key)
		}
	}
}
