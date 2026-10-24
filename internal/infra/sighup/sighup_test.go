package sighup_test

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/sighup"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// SPEC-003 T9 (FR-18; AC-10 unit part, AC-19 unit part): the SIGHUP reporter
// and watcher. Records are warn, component=config (bound by the caller's
// logger), message config.MsgReloadUnsupported, rate-limited to one per 30 s
// with suppressed_count on the record that follows dropped ones.
//
// Interface assumptions: Stop waits for the watcher goroutine to finish, so
// every signal received before Stop has been fully processed when it returns;
// SetLogger installs a fresh Reporter (new rate-limit state) and the latest
// logger wins. No signal.Notify is used: tests feed a channel.

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type rig struct {
	buf *bytes.Buffer
	chk *logcheck.Handler
	log *slog.Logger
}

func newRig(tb testing.TB) *rig {
	tb.Helper()
	var buf bytes.Buffer
	chk := logcheck.New(tb, logcheck.Tee(&buf, logging.FormatJSON))
	l, err := logging.NewFactory(chk).Logger(logging.Config)
	if err != nil {
		tb.Fatalf("factory: %v", err)
	}
	return &rig{buf: &buf, chk: chk, log: l}
}

// records returns the captured records.
func (r *rig) records() []logcheck.Record { return r.chk.Records() }

func attrOf(rec logcheck.Record, key string) (slog.Value, bool) {
	for _, a := range rec.Attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

func attrKeys(rec logcheck.Record) []string {
	var ks []string
	for _, a := range rec.Attrs {
		ks = append(ks, a.Key)
	}
	sort.Strings(ks)
	return ks
}

// requireWarn asserts one FR-18 record and returns its suppressed_count (-1
// when the attribute is absent).
func requireWarn(t *testing.T, rec logcheck.Record) int64 {
	t.Helper()
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}
	if rec.Msg != config.MsgReloadUnsupported {
		t.Errorf("msg = %q", rec.Msg)
	}
	c, ok := attrOf(rec, "component")
	if !ok || c.String() != "config" {
		t.Errorf("component = %v (present %v), want config", c, ok)
	}
	keys := attrKeys(rec)
	n := int64(-1)
	if v, ok := attrOf(rec, "suppressed_count"); ok {
		switch v.Kind() {
		case slog.KindInt64:
			n = v.Int64()
		case slog.KindUint64:
			n = int64(v.Uint64())
		default:
			t.Errorf("suppressed_count has kind %v, want an integer", v.Kind())
		}
		if !reflect.DeepEqual(keys, []string{"component", "suppressed_count"}) {
			t.Errorf("attribute keys = %v", keys)
		}
		if n <= 0 {
			t.Errorf("suppressed_count = %d is present but not greater than zero", n)
		}
	} else if !reflect.DeepEqual(keys, []string{"component"}) {
		t.Errorf("attribute keys = %v, want [component]", keys)
	}
	return n
}

// ---- Reporter ----

func TestMessageConstant(t *testing.T) {
	if config.MsgReloadUnsupported != "configuration reload not supported; restart required" {
		t.Fatalf("message constant = %q", config.MsgReloadUnsupported)
	}
}

func TestReporter_FirstSignalEmitsWithoutSuppressedCount(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	rep.Signal()
	recs := r.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if n := requireWarn(t, recs[0]); n != -1 {
		t.Errorf("first record has suppressed_count=%d, want none", n)
	}
}

// AC-10 (unit): 6 SIGHUPs within 30 seconds give one record; the next after
// 30 seconds carries suppressed_count=5.
func TestAC10_SixSignalsOneRecordThenSuppressed5(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	for i := 0; i < 6; i++ {
		rep.Signal()
		clk.Advance(5 * time.Second) // signals at 0,5,10,15,20,25 s; the clock ends at 30 s
	}
	if n := len(r.records()); n != 1 {
		t.Fatalf("6 signals within 30 s gave %d records, want 1", n)
	}
	// The clock is now exactly 30 s after the first record.
	rep.Signal()
	recs := r.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if n := requireWarn(t, recs[1]); n != 5 {
		t.Errorf("suppressed_count = %d, want 5", n)
	}
}

// Boundary: 29.999 s is still inside the window, 30 s is outside.
func TestReporter_WindowBoundary(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	rep.Signal()
	clk.Advance(30*time.Second - time.Millisecond)
	rep.Signal()
	if n := len(r.records()); n != 1 {
		t.Fatalf("at 29.999 s: %d records, want 1", n)
	}
	clk.Advance(time.Millisecond)
	rep.Signal()
	recs := r.records()
	if len(recs) != 2 {
		t.Fatalf("at 30 s: %d records, want 2", len(recs))
	}
	if n := requireWarn(t, recs[1]); n != 1 {
		t.Errorf("suppressed_count = %d, want 1", n)
	}
}

// The window is anchored at the last emitted record, not at dropped signals,
// and suppressed_count counts only the signals dropped since the previous
// record.
func TestReporter_WindowAnchoredAtLastEmission(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	rep.Signal() // t=0   emitted
	clk.Advance(20 * time.Second)
	rep.Signal() // t=20  dropped
	clk.Advance(20 * time.Second)
	rep.Signal() // t=40  emitted, suppressed 1
	clk.Advance(29 * time.Second)
	rep.Signal() // t=69  dropped
	clk.Advance(time.Second)
	rep.Signal() // t=70  emitted, suppressed 1
	clk.Advance(30 * time.Second)
	rep.Signal() // t=100 emitted, nothing dropped: no suppressed_count
	recs := r.records()
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}
	want := []int64{-1, 1, 1, -1}
	for i, rec := range recs {
		if n := requireWarn(t, rec); n != want[i] {
			t.Errorf("record %d: suppressed_count = %d, want %d", i, n, want[i])
		}
	}
}

func TestReporter_ManyWindows(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	for w := 0; w < 3; w++ {
		for i := 0; i < 4; i++ { // 4 signals in each window: 1 emitted, 3 dropped
			rep.Signal()
			clk.Advance(time.Second)
		}
		clk.Advance(26 * time.Second)
	}
	// One final signal to flush the count of the last window.
	rep.Signal()
	recs := r.records()
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}
	want := []int64{-1, 3, 3, 3}
	for i, rec := range recs {
		if n := requireWarn(t, rec); n != want[i] {
			t.Errorf("record %d: suppressed_count = %d, want %d", i, n, want[i])
		}
	}
}

// Exactly one record when many goroutines signal at the same instant; the
// rest are counted.
func TestReporter_ConcurrentSignals(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(r.log, clk.Now)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rep.Signal() }()
	}
	wg.Wait()
	if n := len(r.records()); n != 1 {
		t.Fatalf("100 concurrent signals gave %d records, want 1", n)
	}
	clk.Advance(30 * time.Second)
	rep.Signal()
	recs := r.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}
	if n := requireWarn(t, recs[1]); n != 99 {
		t.Errorf("suppressed_count = %d, want 99", n)
	}
}

// The reporter adds no component itself: with the bootstrap logger the key
// appears exactly once, and the record is JSON on the writer.
func TestReporter_AddsNoComponent(t *testing.T) {
	var buf bytes.Buffer
	clk := fakeclock.New(t0)
	rep := sighup.NewReporter(logging.BootstrapLogger(&buf), clk.Now)
	rep.Signal()
	line := strings.TrimSpace(buf.String())
	if n := strings.Count(line, `"component":`); n != 1 {
		t.Fatalf("component key appears %d times in %q", n, line)
	}
	if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, config.MsgReloadUnsupported) {
		t.Fatalf("unexpected record %q", line)
	}
	if strings.Contains(line, "suppressed_count") {
		t.Fatalf("first record carries suppressed_count: %q", line)
	}
}

// A logger filtering warn out emits nothing and does not panic.
func TestReporter_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(logging.NewHandler(&buf, logging.FormatJSON, slog.LevelError)).With("component", "config")
	rep := sighup.NewReporter(l, fakeclock.New(t0).Now)
	rep.Signal()
	if buf.Len() != 0 {
		t.Fatalf("record emitted below the logger level: %q", buf.String())
	}
}

// ---- Watcher ----

// send delivers n signals through an unbuffered channel, failing if the
// watcher stops draining it. An unbuffered send completes when the watcher
// receives, so all n are received when send returns.
func send(t *testing.T, ch chan os.Signal, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case ch <- syscall.SIGHUP:
		case <-time.After(3 * time.Second):
			t.Fatalf("watcher stopped draining the channel at signal %d", i+1)
		}
	}
}

func stopWithin(t *testing.T, w *sighup.Watcher) {
	t.Helper()
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
}

// AC-19 (unit part) / FR-18: before a logger exists, nothing is recorded or
// written anywhere, however many signals arrive, and the channel keeps being
// drained.
func TestWatcher_NoLoggerNoRecord(t *testing.T) {
	// Any accidental fallback to the default loggers is observed.
	r := newRig(t)
	oldSlog := slog.Default()
	slog.SetDefault(r.log)
	var stdlog bytes.Buffer
	log.SetOutput(&stdlog)
	t.Cleanup(func() { slog.SetDefault(oldSlog); log.SetOutput(os.Stderr) })

	ch := make(chan os.Signal)
	w := sighup.Start(ch, fakeclock.New(t0).Now)
	send(t, ch, 200)
	stopWithin(t, w)
	if n := len(r.records()); n != 0 {
		t.Errorf("%d records without a logger", n)
	}
	if stdlog.Len() != 0 {
		t.Errorf("wrote %d bytes to the standard logger without a logger", stdlog.Len())
	}
}

// Signals dropped before SetLogger are never replayed, and are not counted as
// suppressed.
func TestWatcher_SignalsBeforeSetLoggerAreDropped(t *testing.T) {
	r := newRig(t)
	ch := make(chan os.Signal)
	w := sighup.Start(ch, fakeclock.New(t0).Now)
	send(t, ch, 3)
	send(t, ch, 1) // barrier: the three above are fully processed once this is received
	time.Sleep(100 * time.Millisecond)
	w.SetLogger(r.log)
	stopWithin(t, w)
	if n := len(r.records()); n != 0 {
		t.Fatalf("%d records after SetLogger with no new signal (earlier signals were replayed)", n)
	}
}

func TestWatcher_SignalAfterSetLoggerEmitsOneRecord(t *testing.T) {
	r := newRig(t)
	ch := make(chan os.Signal)
	w := sighup.Start(ch, fakeclock.New(t0).Now)
	send(t, ch, 2)
	send(t, ch, 1)
	time.Sleep(100 * time.Millisecond)
	w.SetLogger(r.log)
	send(t, ch, 1)
	stopWithin(t, w)
	recs := r.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if n := requireWarn(t, recs[0]); n != -1 {
		t.Errorf("suppressed_count = %d: signals dropped before the logger must not be counted", n)
	}
}

// AC-10 (unit) through the watcher.
func TestWatcher_AC10(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	ch := make(chan os.Signal)
	w := sighup.Start(ch, clk.Now)
	w.SetLogger(r.log)
	send(t, ch, 6)
	// Wait until the sixth signal has been processed: Stop is the barrier, so
	// use a second watcher phase instead. Poll for the first record, then
	// make sure no more arrive.
	waitRecords(t, r, 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(r.records()); n != 1 {
		t.Fatalf("6 signals gave %d records, want 1", n)
	}
	clk.Advance(30 * time.Second)
	send(t, ch, 1)
	waitRecords(t, r, 2)
	stopWithin(t, w)
	recs := r.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if n := requireWarn(t, recs[1]); n != 5 {
		t.Errorf("suppressed_count = %d, want 5", n)
	}
}

func waitRecords(t *testing.T, r *rig, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(r.records()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d records (have %d)", n, len(r.records()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SetLogger installs a fresh reporter: the latest logger receives later
// records, the earlier one none.
func TestWatcher_LatestLoggerWins(t *testing.T) {
	a, b := newRig(t), newRig(t)
	ch := make(chan os.Signal)
	w := sighup.Start(ch, fakeclock.New(t0).Now)
	w.SetLogger(a.log)
	send(t, ch, 1)
	waitRecords(t, a, 1)
	w.SetLogger(b.log)
	send(t, ch, 1)
	waitRecords(t, b, 1)
	stopWithin(t, w)
	if len(a.records()) != 1 || len(b.records()) != 1 {
		t.Fatalf("records: first logger %d, second logger %d, want 1 and 1", len(a.records()), len(b.records()))
	}
}

func TestWatcher_StopIsPromptAndIdempotent(t *testing.T) {
	ch := make(chan os.Signal)
	w := sighup.Start(ch, nil)
	stopWithin(t, w)
	stopWithin(t, w)
	stopWithin(t, w)
	// SetLogger after Stop must not panic or block.
	done := make(chan struct{})
	go func() { w.SetLogger(newRig(t).log); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SetLogger after Stop blocked")
	}
}

func TestWatcher_StopWithoutAnySignal(t *testing.T) {
	r := newRig(t)
	ch := make(chan os.Signal, 4)
	w := sighup.Start(ch, fakeclock.New(t0).Now)
	w.SetLogger(r.log)
	stopWithin(t, w)
	if len(r.records()) != 0 {
		t.Fatal("record without a signal")
	}
}

// Stop ends the goroutine.
func TestWatcher_StopEndsGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	var ws []*sighup.Watcher
	for i := 0; i < 20; i++ {
		ws = append(ws, sighup.Start(make(chan os.Signal), nil))
	}
	for _, w := range ws {
		stopWithin(t, w)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after Stop", before, after)
	}
}

// A closed channel ends the watcher without panic or spinning; Stop still works.
func TestWatcher_ClosedChannel(t *testing.T) {
	ch := make(chan os.Signal)
	w := sighup.Start(ch, nil)
	close(ch)
	time.Sleep(50 * time.Millisecond)
	stopWithin(t, w)
}

// The watcher uses the injected clock, not the wall clock.
func TestWatcher_UsesInjectedClock(t *testing.T) {
	r := newRig(t)
	clk := fakeclock.New(t0)
	ch := make(chan os.Signal)
	w := sighup.Start(ch, clk.Now)
	w.SetLogger(r.log)
	send(t, ch, 1)
	waitRecords(t, r, 1)
	time.Sleep(50 * time.Millisecond) // real time passes, fake time does not
	send(t, ch, 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(r.records()); n != 1 {
		t.Fatalf("got %d records, want 1 (fake clock did not advance)", n)
	}
	stopWithin(t, w)
}

// Race detector: signals, SetLogger and Stop from different goroutines.
func TestWatcher_ConcurrentUse(t *testing.T) {
	ch := make(chan os.Signal)
	w := sighup.Start(ch, nil)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			select {
			case ch <- syscall.SIGHUP:
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.SetLogger(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))) }()
	}
	time.Sleep(50 * time.Millisecond)
	stopWithin(t, w)
	cancel()
	wg.Wait()
}
