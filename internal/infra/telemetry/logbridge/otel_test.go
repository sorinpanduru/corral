package logbridge_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
)

const win = 30 * time.Second

// markerErr mimics an exporter error carrying secrets in its text: a url.Error
// with a userinfo marker.
func markerErr(m string) error {
	return &url.Error{Op: "Post", URL: "https://user:" + m + "@collector.invalid/v1/traces", Err: errors.New("boom " + m)}
}

// AC-26 (FR-28): otel error handler -> one warn record, component=otel,
// fixed message, no third-party text.
func TestOTelErrorHandlerRouting(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	h := logbridge.NewOTelErrorHandler(e.f, e.clk.Now)
	h.Handle(markerErr(m))
	expect(t, e.only(), slog.LevelWarn, "otel", 0)
	e.noMarker(m)
}

// FR-28/FR-29: the record carries exactly the sanitised class attrs.
func TestOTelErrorHandlerCarriesErrclassAttrs(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	err := markerErr(m)
	logbridge.NewOTelErrorHandler(e.f, e.clk.Now).Handle(err)
	r := e.only()
	want := errclass.LogAttrs(err)
	if len(want) == 0 {
		t.Fatal("test premise: errclass.LogAttrs returned nothing")
	}
	for _, a := range want {
		got, ok := r.Attrs[a.Key]
		if !ok || got.String() != a.Value.String() {
			t.Errorf("attr %q = %v (present=%v), want %v", a.Key, got, ok, a.Value)
		}
	}
	e.noMarker(m)
}

// FR-28: the message is one constant per source and level.
func TestOTelErrorHandlerMessageIsConstant(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	h := logbridge.NewOTelErrorHandler(e.f, e.clk.Now)
	h.Handle(markerErr(marker(t)))
	e.clk.Advance(win)
	h.Handle(errors.New(marker(t)))
	rs := e.records()
	if len(rs) != 2 || rs[0].Msg != rs[1].Msg {
		t.Fatalf("messages differ or count != 2: %+v", rs)
	}
}

// AC-29 (FR-33): 10 failures in 30 s give one warning; the next failure after
// 30 s gives a warning with suppressed_count 9; no err.Error() text.
func TestAC29OTelErrorHandlerLimit(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	m := marker(t)
	h := logbridge.NewOTelErrorHandler(e.f, e.clk.Now)
	for i := 0; i < 10; i++ {
		h.Handle(markerErr(m))
		e.clk.Advance(time.Second)
	}
	expect(t, e.only(), slog.LevelWarn, "otel", 0)
	e.clk.Advance(win)
	h.Handle(markerErr(m))
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2", e.count())
	}
	expect(t, e.last(), slog.LevelWarn, "otel", 9)
	e.noMarker(m)
}

// D7: records below the configured level do not consume the limiter slot.
func TestOTelErrorHandlerLevelFilterBeforeLimiter(t *testing.T) {
	e := newEnv(t, slog.LevelError) // warn is filtered
	h := logbridge.NewOTelErrorHandler(e.f, e.clk.Now)
	for i := 0; i < 5; i++ {
		h.Handle(errors.New("x"))
	}
	if e.count() != 0 {
		t.Fatalf("records = %d at error level, want 0", e.count())
	}
	e.lv.Set(slog.LevelInfo)
	h.Handle(errors.New("x"))
	expect(t, e.only(), slog.LevelWarn, "otel", 0) // 0: filtered events are not suppressed events
}

// Robustness: nil error does not panic.
func TestOTelErrorHandlerNil(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	logbridge.NewOTelErrorHandler(e.f, e.clk.Now).Handle(nil)
}

// AC-26/FR-28, D7: the logr sink maps Info at any verbosity to debug, Error to
// warn; message and key/values (and names/values from With*) never forwarded.
func TestOTelLogSinkRoutingAndNoForwarding(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))

	lg.WithName(m).WithValues(m, m).V(4).Info(m, m, m)
	expect(t, e.only(), slog.LevelDebug, "otel", 0)
	infoMsg := e.last().Msg

	lg.WithName(m).WithValues(m, m).Error(errors.New(m), m, m, m)
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2 (one limiter per level)", e.count())
	}
	expect(t, e.last(), slog.LevelWarn, "otel", 0)
	if e.last().Msg == "" || infoMsg == "" {
		t.Error("empty message")
	}
	e.noMarker(m)
}

// D7: the logr Info path follows the configured level and is dropped at info
// without consuming the slot.
func TestOTelLogSinkInfoFilteredAtInfoLevel(t *testing.T) {
	e := newEnv(t, slog.LevelInfo)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))
	for _, v := range []int{0, 1, 4} {
		lg.V(v).Info("x")
	}
	if e.count() != 0 {
		t.Fatalf("records = %d at info level, want 0", e.count())
	}
	e.lv.Set(slog.LevelDebug)
	lg.Info("x")
	expect(t, e.only(), slog.LevelDebug, "otel", 0)
}

// FR-33: logr limiter is per level, 10 within 30 s gives 1, then 9 suppressed.
func TestOTelLogSinkLimiterPerLevel(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))
	for i := 0; i < 10; i++ {
		lg.Info("x")
		lg.Error(errors.New("y"), "z")
		e.clk.Advance(time.Second)
	}
	rs := e.records()
	if len(rs) != 2 {
		t.Fatalf("records = %d, want 2 (one per level)", len(rs))
	}
	e.clk.Advance(win)
	lg.Info("x")
	lg.Error(errors.New("y"), "z")
	rs = e.records()
	if len(rs) != 4 {
		t.Fatalf("records = %d, want 4", len(rs))
	}
	for _, r := range rs[2:] {
		if r.suppressed(t) != 9 {
			t.Errorf("%v suppressed_count = %d, want 9", r.Level, r.suppressed(t))
		}
	}
}

// D7: error handler and logger have separate limiters, as do the two logr
// levels (a burst of one does not suppress the other).
func TestOTelErrorHandlerAndLoggerIndependent(t *testing.T) {
	e := newEnv(t, slog.LevelDebug)
	h := logbridge.NewOTelErrorHandler(e.f, e.clk.Now)
	lg := logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now))
	h.Handle(errors.New("a"))
	lg.Error(errors.New("b"), "c") // warn, same level as the handler, different source
	if e.count() != 2 {
		t.Fatalf("records = %d, want 2", e.count())
	}
	for _, r := range e.records() {
		expect(t, r, slog.LevelWarn, "otel", 0)
	}
	_ = fmt.Sprint
}
