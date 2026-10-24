package tracing_test

// Integration tests for the tracing bootstrap core (plans/SPEC-001.md T15,
// ADR-0007). They run in-process against internal/testkit/otlpcollector and
// create test spans through the provider that tracing.Start configured.
//
// Expected API (plans/SPEC-001.md T15, "Interfaces defined"):
//
//	tracing.Options{Settings config.Settings; Env tracing.LookupEnv;
//	    Config tracing.EnvConfig; Enabled bool; Metrics *metrics.Factory;
//	    Logger *slog.Logger; Exporter sdktrace.SpanExporter}
//	tracing.Start(ctx, Options) (*tracing.Tracing, error)
//	(*Tracing).Enabled() bool
//	(*Tracing).TracerProvider() trace.TracerProvider
//	(*Tracing).ForceFlush(ctx) error
//	(*Tracing).Shutdown(ctx) error
//
// Tests set OTEL_* with t.Setenv and pass os.LookupEnv as Options.Env, because
// the OTLP exporters read os.Getenv themselves (ADR-0007 b). None of these
// tests run in parallel. Test data is synthetic only.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/grpclog"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const droppedMetric = "corral_tracing_spans_dropped_total"

var clockStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// Constant log messages (AC-5: slog messages must be compile-time constants).
const (
	msgInsideSampled   = "inside sampled span"
	msgInsideUnsampled = "inside unsampled span"
	msgOutsideSpan     = "outside any span"
)

// newMarker returns a random synthetic marker of 18 bytes in [A-Za-z0-9_-].
func newMarker(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "MK" + hex.EncodeToString(b)
}

// restoreGlobals puts the process-wide sinks back after Install and Start.
func restoreGlobals(t *testing.T) {
	t.Helper()
	oldDef, oldW, oldF, oldP := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	oldH := otel.GetErrorHandler()
	oldTP := otel.GetTracerProvider()
	t.Cleanup(func() {
		slog.SetDefault(oldDef)
		log.SetOutput(oldW)
		log.SetFlags(oldF)
		log.SetPrefix(oldP)
		otel.SetErrorHandler(oldH)
		otel.SetTracerProvider(oldTP)
		otel.SetLogger(logr.Discard())
		grpcLog.set(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
	})
}

// setEnv clears every ambient OTEL_* variable (empty counts as unset) and sets
// the given ones, all through t.Setenv.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "")
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// levelHandler applies the configured level (logcheck itself is enabled at
// every level) and delegates AC-5 checks, recording and the JSON tee.
type levelHandler struct {
	lc *logcheck.Handler
	lv slog.Leveler
}

func (c *levelHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= c.lv.Level() }
func (c *levelHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &levelHandler{lc: c.lc.WithAttrs(as).(*logcheck.Handler), lv: c.lv}
}
func (c *levelHandler) WithGroup(n string) slog.Handler {
	return &levelHandler{lc: c.lc.WithGroup(n).(*logcheck.Handler), lv: c.lv}
}
func (c *levelHandler) Handle(ctx context.Context, r slog.Record) error {
	return c.lc.Handle(ctx, r)
}

type opts struct {
	// on is the tracing.enabled setting.
	on bool
	// level is the configured log level (zero value is info).
	level slog.Level
	// env is applied with t.Setenv after clearing OTEL_*.
	env map[string]string
	// exporter is the test-only exporter override.
	exporter sdktrace.SpanExporter
	// afterResolve, when set, runs between tracing.Resolve and tracing.Start
	// (T24.4: make validated files unreadable).
	afterResolve func()
}

type harness struct {
	t        *testing.T
	tr       *tracing.Tracing
	enabled  bool
	cfg      tracing.EnvConfig
	lc       *logcheck.Handler
	clk      *fakeclock.Clock
	reg      *prometheus.Registry
	logger   *slog.Logger
	stopIO   func() ([]byte, []byte)
	shutDown bool
}

// boot performs the startup order of ADR-0006 steps 1-8 that matter for
// tracing: capture stdio, build the handler (with trace correlation exactly
// when tracing is enabled), logbridge.Install, metrics, then tracing.Start.
func boot(t *testing.T, o opts) *harness {
	t.Helper()
	restoreGlobals(t)
	setEnv(t, o.env)

	h := &harness{t: t, clk: fakeclock.New(clockStart)}
	h.stopIO = markerscan.CaptureStdio(t)

	st := config.Defaults()
	st.TracingEnabled = o.on
	enabled, cfg, err := tracing.Resolve(st, os.LookupEnv)
	if err != nil {
		t.Fatalf("Resolve returned an error for a valid environment: %v", err)
	}
	h.enabled, h.cfg = enabled, cfg
	if o.afterResolve != nil {
		o.afterResolve()
	}

	lv := new(slog.LevelVar)
	lv.Set(o.level)
	var lcOpts []logcheck.Option
	lcOpts = append(lcOpts, logcheck.Tee(os.Stdout, logging.FormatJSON))
	if enabled {
		lcOpts = append(lcOpts, logcheck.BehindCorrelation())
	}
	h.lc = logcheck.New(t, lcOpts...)
	var hd slog.Handler = &levelHandler{lc: h.lc, lv: lv}
	if enabled {
		hd = logging.WithTraceCorrelation(hd)
	}
	factory := logging.NewFactory(hd)
	// PINNED COPY of logbridge.Install (internal/infra/telemetry/logbridge/
	// thirdparty.go): otel error handler, otel logger, grpclog, slog.Default,
	// std log writer, in that order. The only difference is that grpclog goes
	// through the delegate (see main_test.go). If Install gains a step, this
	// copy must follow; TestHarnessInstallsEveryBridge checks every observable
	// global is wired.
	otel.SetErrorHandler(logbridge.NewOTelErrorHandler(factory, h.clk.Now))
	otel.SetLogger(logr.New(logbridge.NewOTelLogSink(factory, h.clk.Now)))
	grpcLog.set(logbridge.NewGRPCLogger(factory, lv, h.clk.Now))
	slog.SetDefault(slog.New(logbridge.NewSlogDefaultHandler(factory, lv, h.clk.Now)))
	log.SetOutput(logbridge.NewStdLogWriter(factory, lv, h.clk.Now))

	lg, err := factory.Logger(logging.Tracing)
	if err != nil {
		t.Fatal(err)
	}
	h.logger = lg

	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	h.reg = metrics.NewRegistry()
	mf, err := metrics.NewFactory(h.reg, cat)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	if err := metrics.RegisterAll(mf); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	tr, err := tracing.Start(context.Background(), tracing.Options{
		Settings: st,
		Env:      os.LookupEnv,
		Config:   cfg,
		Enabled:  enabled,
		Metrics:  mf,
		Logger:   lg,
		Exporter: o.exporter,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.tr = tr
	t.Cleanup(func() {
		if !h.shutDown {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = h.tr.Shutdown(ctx)
		}
	})
	return h
}

// emit creates and ends one root span per name through the configured provider.
func (h *harness) emit(names ...string) {
	h.t.Helper()
	tr := h.tr.TracerProvider().Tracer("corral-test")
	for _, n := range names {
		_, sp := tr.Start(context.Background(), n)
		sp.End()
	}
}

// flush forces a flush bounded by 10 s and returns its error.
func (h *harness) flush() error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.tr.ForceFlush(ctx)
}

// shutdown shuts tracing down with a 10 s caller deadline.
func (h *harness) shutdown() error {
	h.t.Helper()
	h.shutDown = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.tr.Shutdown(ctx)
}

// finish shuts tracing down (if not done), stops the stdio capture, asserts the
// AC-3 invariants that hold for every test (stderr empty, every stdout line
// valid JSON with time/level/msg/component) and returns stdout and stderr.
func (h *harness) finish() (stdout, stderr []byte) {
	h.t.Helper()
	if !h.shutDown {
		_ = h.shutdown()
	}
	stdout, stderr = h.stopIO()
	if len(stderr) != 0 {
		h.t.Errorf("AC-3/FR-28: %d bytes written to stderr", len(stderr))
	}
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			h.t.Errorf("AC-3: stdout line is not JSON (%d bytes)", len(line))
			continue
		}
		for _, k := range []string{"time", "level", "msg", "component"} {
			if _, ok := m[k]; !ok {
				h.t.Errorf("AC-3: stdout JSON line lacks %q", k)
			}
		}
	}
	return stdout, stderr
}

// record is a captured log record with attributes indexed by key.
type record struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

func (r record) str(k string) string { return r.Attrs[k].String() }

func (h *harness) records() []record {
	var out []record
	for _, r := range h.lc.Records() {
		m := map[string]slog.Value{}
		for _, a := range r.Attrs {
			m[a.Key] = a.Value.Resolve()
		}
		out = append(out, record{Level: r.Level, Msg: r.Msg, Attrs: m})
	}
	return out
}

func (h *harness) otelRecords() []record {
	var out []record
	for _, r := range h.records() {
		if r.str("component") == "otel" {
			out = append(out, r)
		}
	}
	return out
}

// metricsText renders the private registry (the /metrics source for scans).
func (h *harness) metricsText() []byte {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatalf("Gather: %v", err)
	}
	var b bytes.Buffer
	for _, mf := range mfs {
		b.WriteString(mf.String())
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// droppedValue returns the value of corral_tracing_spans_dropped_total and
// whether the metric is registered.
func (h *harness) droppedValue() (float64, bool) {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == droppedMetric {
			ms := mf.GetMetric()
			if len(ms) != 1 {
				h.t.Fatalf("%s has %d series, want 1", droppedMetric, len(ms))
			}
			return ms[0].GetCounter().GetValue(), true
		}
	}
	return 0, false
}

func httpURL(c *otlpcollector.Collector) string { return "http://" + c.HTTPEndpoint() }
func grpcURL(c *otlpcollector.Collector) string { return "http://" + c.GRPCEndpoint() }

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func spanNames(c *otlpcollector.Collector) []string {
	var out []string
	for _, s := range c.Spans() {
		out = append(out, s.Name)
	}
	return out
}

func hasSpan(c *otlpcollector.Collector, name string) bool {
	for _, s := range c.Spans() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// headerValues looks a header up case-insensitively (HTTP canonicalises, gRPC
// metadata lower-cases).
func headerValues(ri otlpcollector.RequestInfo, name string) []string {
	var out []string
	for k, v := range ri.Headers {
		if strings.EqualFold(k, name) {
			out = append(out, v...)
		}
	}
	return out
}

// blockingExporter blocks every export until released or its context ends.
type blockingExporter struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingExporter() *blockingExporter {
	return &blockingExporter{release: make(chan struct{})}
}

func (b *blockingExporter) Release() { b.once.Do(func() { close(b.release) }) }

func (b *blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingExporter) Shutdown(context.Context) error { b.Release(); return nil }

var _ sdktrace.SpanExporter = (*blockingExporter)(nil)
