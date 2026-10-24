package logcheck_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// fakeTB captures failures instead of failing the real test, so the tests can
// assert that logcheck reports a violation (AC-5). Both Error-style and
// Fatal-style reporting by the handler are accepted.
type fakeTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	msgs   []string
}

func (f *fakeTB) record(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = true
	f.msgs = append(f.msgs, m)
}
func (f *fakeTB) Error(args ...any)                 { f.record(fmt.Sprint(args...)) }
func (f *fakeTB) Errorf(format string, args ...any) { f.record(fmt.Sprintf(format, args...)) }
func (f *fakeTB) Fatal(args ...any)                 { f.record(fmt.Sprint(args...)); runtime.Goexit() }
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.record(fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (f *fakeTB) Fail()    { f.record("fail") }
func (f *fakeTB) FailNow() { f.record("failnow"); runtime.Goexit() }
func (f *fakeTB) Failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// run executes fn in its own goroutine (so a Fatal-style Goexit is contained)
// against a handler bound to a fake TB, and returns the TB and the handler.
func run(t *testing.T, fn func(h *logcheck.Handler), opts ...logcheck.Option) (*fakeTB, *logcheck.Handler) {
	t.Helper()
	ftb := &fakeTB{TB: t}
	h := logcheck.New(ftb, opts...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(h)
	}()
	<-done
	return ftb, h
}

func wantFail(t *testing.T, name string, fn func(h *logcheck.Handler), opts ...logcheck.Option) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		ftb, _ := run(t, fn, opts...)
		if !ftb.Failed() {
			t.Error("logcheck did not fail the test for a violating record")
		}
	})
}

func wantPass(t *testing.T, name string, fn func(h *logcheck.Handler), opts ...logcheck.Option) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		ftb, _ := run(t, fn, opts...)
		if ftb.Failed() {
			t.Errorf("logcheck failed a valid record: %v", ftb.msgs)
		}
	})
}

func lg(h *logcheck.Handler, c string) *slog.Logger { return slog.New(h).With("component", c) }

func attrMap(r logcheck.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	for _, a := range r.Attrs {
		m[a.Key] = a.Value.Resolve()
	}
	return m
}

// AC-5, FR-7: valid records, for every component in the fixed set, pass.
func TestValidRecordsForEveryComponentPass(t *testing.T) {
	for _, c := range logging.Components() {
		wantPass(t, string(c), func(h *logcheck.Handler) {
			l := lg(h, string(c))
			l.Info("synthetic message", "setting", "log.level", "error", "timeout")
			l.With("retry_count", 3).Warn("synthetic with")
		})
	}
}

// AC-5, FR-7: the thirdparty component (D5) is accepted because logcheck
// follows logging.Components().
func TestThirdPartyComponentAccepted(t *testing.T) {
	wantPass(t, "thirdparty", func(h *logcheck.Handler) {
		lg(h, string(logging.ThirdParty)).Info("synthetic")
	})
}

// AC-5, FR-7: no component, or a component outside the set, fails.
func TestComponentViolations(t *testing.T) {
	wantFail(t, "missing", func(h *logcheck.Handler) { slog.New(h).Info("synthetic") })
	wantFail(t, "empty", func(h *logcheck.Handler) { lg(h, "").Info("synthetic") })
	wantFail(t, "unknown", func(h *logcheck.Handler) { lg(h, "bogus_component").Info("synthetic") })
	wantFail(t, "wrong_case", func(h *logcheck.Handler) { lg(h, "CONFIG").Info("synthetic") })
	wantFail(t, "third_party_underscore", func(h *logcheck.Handler) { lg(h, "third_party").Info("synthetic") })
	wantFail(t, "non_string", func(h *logcheck.Handler) { slog.New(h).With("component", 7).Info("synthetic") })
	wantFail(t, "at_call_site_unknown", func(h *logcheck.Handler) { slog.New(h).Info("synthetic", "component", "nope") })
}

// AC-5, FR-39: keys outside the allowlist fail; allowlisted keys pass.
func TestKeyAllowlist(t *testing.T) {
	wantFail(t, "unknown_key_call", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "user_name", "x") })
	wantFail(t, "unknown_key_with", func(h *logcheck.Handler) { lg(h, "config").With("host", "x").Info("synthetic") })
	wantFail(t, "camel_case_numeric_like", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "retryCount", 1) })
	wantFail(t, "uppercase_suffix", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "retry_COUNT", 1) })
	wantFail(t, "bad_bare_suffix", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "_count", 1) })
	wantPass(t, "allowed_keys", func(h *logcheck.Handler) {
		lg(h, "config").Info("synthetic",
			"error", "timeout", "setting", "log.level", "stack", "frames",
			"corral.error.detail", "ETIMEDOUT", "db.operation.name", "find",
			"client.port", 1234, "suppressed_count", 4, "idle_seconds", 1.5, "payload_bytes", int64(9))
	})
}

// AC-5, FR-39: any slog group fails, however it is created.
func TestGroupsFail(t *testing.T) {
	wantFail(t, "WithGroup", func(h *logcheck.Handler) { lg(h, "config").WithGroup("g").Info("synthetic", "setting", "x") })
	wantFail(t, "WithGroup_no_attrs", func(h *logcheck.Handler) { lg(h, "config").WithGroup("g").Info("synthetic") })
	wantFail(t, "Group_value_call", func(h *logcheck.Handler) {
		lg(h, "config").Info("synthetic", slog.Group("g", slog.String("setting", "x")))
	})
	wantFail(t, "Group_value_with", func(h *logcheck.Handler) {
		lg(h, "config").With(slog.Group("g", slog.String("setting", "x"))).Info("synthetic")
	})
	wantFail(t, "Group_empty_key_inlined_bad_key", func(h *logcheck.Handler) {
		lg(h, "config").Info("synthetic", slog.Group("", slog.String("host", "x")))
	})
	wantFail(t, "nested_group", func(h *logcheck.Handler) {
		lg(h, "config").Info("synthetic", slog.Group("a", slog.Group("b", slog.Int("retry_count", 1))))
	})
}

// AC-5, FR-39: _seconds/_bytes/_count keys need a numeric value.
func TestNumericSuffixValues(t *testing.T) {
	for _, k := range []string{"x_seconds", "x_bytes", "x_count", "suppressed_count"} {
		wantPass(t, k+"_int", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, 5) })
		wantPass(t, k+"_uint64", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, uint64(5)) })
		wantPass(t, k+"_float", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, 0.25) })
		wantFail(t, k+"_string", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, "5") })
		wantFail(t, k+"_bool", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, true) })
		wantFail(t, k+"_any", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, struct{}{}) })
		wantFail(t, k+"_error", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, context.Canceled) })
	}
	wantFail(t, "with_attr_string", func(h *logcheck.Handler) { lg(h, "config").With("x_count", "n").Info("synthetic") })
}

// FR-8, FR-39: callers must never set trace_id/span_id. Without the
// BehindCorrelation option (the handler is not installed behind
// logging.WithTraceCorrelation) any such key fails.
func TestTraceKeysFailWithoutCorrelation(t *testing.T) {
	sc := spanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	for _, k := range []string{"trace_id", "span_id"} {
		wantFail(t, k+"_call", func(h *logcheck.Handler) { lg(h, "tracing").Info("synthetic", k, "abc") })
		wantFail(t, k+"_with", func(h *logcheck.Handler) { lg(h, "tracing").With(k, "abc").Info("synthetic") })
		wantFail(t, k+"_matching_value_with_ctx", func(h *logcheck.Handler) {
			v := sc.TraceID().String()
			if k == "span_id" {
				v = sc.SpanID().String()
			}
			lg(h, "tracing").InfoContext(ctx, "synthetic", k, v)
		})
	}
}

func spanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0xBC, 0xde, 0xf0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0xAA, 0xBB, 0xCC},
		SpanID:     trace.SpanID{0xFE, 0xDC, 0xBA, 0x98, 1, 2, 3, 4},
		TraceFlags: trace.FlagsSampled,
	})
}

// correlated builds slog.Logger over WithTraceCorrelation(logcheck handler
// with BehindCorrelation), the real production wiring order.
func correlated(h *logcheck.Handler) *slog.Logger {
	return slog.New(logging.WithTraceCorrelation(h)).With("component", "tracing")
}

// FR-8: behind the correlation wrapper, the wrapper-added trace_id/span_id
// (exactly once, matching the context's span context) pass.
func TestCorrelationAddedIDsPass(t *testing.T) {
	sc := spanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ftb, h := run(t, func(h *logcheck.Handler) {
		correlated(h).InfoContext(ctx, "synthetic in span", "retry_count", 1)
		correlated(h).Info("synthetic outside span")
	}, logcheck.BehindCorrelation())
	if ftb.Failed() {
		t.Fatalf("wrapper-added ids were rejected: %v", ftb.msgs)
	}
	recs := h.Records()
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	m := attrMap(recs[0])
	if m["trace_id"].String() != sc.TraceID().String() || m["span_id"].String() != sc.SpanID().String() {
		t.Errorf("ids not recorded: %v", m)
	}
	if _, ok := attrMap(recs[1])["trace_id"]; ok {
		t.Error("trace_id recorded for a record with no span context")
	}
}

// FR-8: behind the wrapper, a caller that also sets trace_id/span_id (same or
// different value, call site or With) fails; so does an id with no span
// context in the record's context.
func TestCallerSetTraceKeysFailBehindCorrelation(t *testing.T) {
	sc := spanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	opt := logcheck.BehindCorrelation()
	for _, k := range []string{"trace_id", "span_id"} {
		same := sc.TraceID().String()
		if k == "span_id" {
			same = sc.SpanID().String()
		}
		wantFail(t, k+"_same_value_in_span", func(h *logcheck.Handler) { correlated(h).InfoContext(ctx, "synthetic", k, same) }, opt)
		wantFail(t, k+"_other_value_in_span", func(h *logcheck.Handler) { correlated(h).InfoContext(ctx, "synthetic", k, "deadbeef") }, opt)
		wantFail(t, k+"_with_in_span", func(h *logcheck.Handler) { correlated(h).With(k, same).InfoContext(ctx, "synthetic") }, opt)
		wantFail(t, k+"_no_span_ctx", func(h *logcheck.Handler) { correlated(h).Info("synthetic", k, same) }, opt)
		wantFail(t, k+"_with_no_span_ctx", func(h *logcheck.Handler) { correlated(h).With(k, same).Info("synthetic") }, opt)
	}
}

// AC-5: Records captures level, message and attributes (bound and call-site).
func TestRecordsCaptureLevelMsgAttrs(t *testing.T) {
	ftb, h := run(t, func(h *logcheck.Handler) {
		l := lg(h, "lifecycle").With("setting", "log.level")
		l.Debug("first", "retry_count", 2)
		l.Warn("second")
		l.Error("third", "error", "timeout")
	})
	if ftb.Failed() {
		t.Fatalf("unexpected failure: %v", ftb.msgs)
	}
	recs := h.Records()
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	wantLevels := []slog.Level{slog.LevelDebug, slog.LevelWarn, slog.LevelError}
	wantMsgs := []string{"first", "second", "third"}
	for i, r := range recs {
		if r.Level != wantLevels[i] || r.Msg != wantMsgs[i] {
			t.Errorf("record %d = %v %q", i, r.Level, r.Msg)
		}
		m := attrMap(r)
		if m["component"].String() != "lifecycle" || m["setting"].String() != "log.level" {
			t.Errorf("record %d lacks bound attrs: %v", i, m)
		}
	}
	if v := attrMap(recs[0])["retry_count"]; v.Kind() != slog.KindInt64 || v.Int64() != 2 {
		t.Errorf("retry_count = %v", v)
	}
	if attrMap(recs[2])["error"].String() != "timeout" {
		t.Error("error attr not recorded")
	}
}

// AC-5: all levels are enabled so no record escapes the check.
func TestEnabledAtEveryLevel(t *testing.T) {
	_, h := run(t, func(h *logcheck.Handler) {})
	for _, lv := range []slog.Level{slog.LevelDebug - 4, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if !h.Enabled(context.Background(), lv) {
			t.Errorf("level %v not enabled", lv)
		}
	}
}

// AC-5: Records returns a snapshot; mutating it does not alter the handler.
func TestRecordsReturnsCopy(t *testing.T) {
	_, h := run(t, func(h *logcheck.Handler) { lg(h, "config").Info("one") })
	r := h.Records()
	r[0].Msg = "mutated"
	if h.Records()[0].Msg != "one" {
		t.Error("Records exposes internal storage")
	}
}

// AC-5: concurrent logging is safe (run with -race) and every record kept.
func TestConcurrentLogging(t *testing.T) {
	ftb, h := run(t, func(h *logcheck.Handler) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l := lg(h, "config")
				for j := 0; j < 50; j++ {
					l.Info("synthetic", "retry_count", j)
				}
			}()
		}
		wg.Wait()
	})
	if ftb.Failed() {
		t.Fatalf("unexpected failure: %v", ftb.msgs)
	}
	if n := len(h.Records()); n != 400 {
		t.Errorf("records = %d, want 400", n)
	}
}

// AC-5: Tee produces real output in the requested format while still
// checking. JSON lines parse and carry the bound component.
func TestTeeJSON(t *testing.T) {
	var buf bytes.Buffer
	ftb, h := run(t, func(h *logcheck.Handler) {
		lg(h, "admin_http").Info("synthetic tee", "retry_count", 2)
	}, logcheck.Tee(&buf, logging.FormatJSON))
	if ftb.Failed() {
		t.Fatalf("unexpected failure: %v", ftb.msgs)
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("tee output is not JSON: %q: %v", buf.String(), err)
	}
	if m["msg"] != "synthetic tee" || m["component"] != "admin_http" || m["retry_count"] != float64(2) {
		t.Errorf("tee output = %v", m)
	}
	if len(h.Records()) != 1 {
		t.Errorf("records = %d", len(h.Records()))
	}
}

func TestTeeText(t *testing.T) {
	var buf bytes.Buffer
	run(t, func(h *logcheck.Handler) {
		lg(h, "grpc").Warn("synthetic tee", "setting", "OTEL_X")
	}, logcheck.Tee(&buf, logging.FormatText))
	s := buf.String()
	for _, want := range []string{"level=WARN", `msg="synthetic tee"`, "component=grpc", "setting=OTEL_X"} {
		if !strings.Contains(s, want) {
			t.Errorf("text tee lacks %q: %q", want, s)
		}
	}
	if strings.HasPrefix(s, "{") {
		t.Errorf("text tee looks like JSON: %q", s)
	}
}

// AC-5: the test handler composes with the production correlation wrapper and
// factory, as every later suite wires it.
func TestWorksThroughFactory(t *testing.T) {
	ftb, h := run(t, func(h *logcheck.Handler) {
		l, err := logging.NewFactory(logging.WithTraceCorrelation(h)).Logger(logging.OTel)
		if err != nil {
			t.Error(err)
			return
		}
		l.Info("synthetic via factory", "suppressed_count", 3)
	}, logcheck.BehindCorrelation())
	if ftb.Failed() {
		t.Fatalf("unexpected failure: %v", ftb.msgs)
	}
	recs := h.Records()
	if len(recs) != 1 || attrMap(recs[0])["component"].String() != "otel" {
		t.Errorf("records = %+v", recs)
	}
}

// AC-5: a violation is reported against the bound TB, not silently dropped,
// and the offending record is still handled without panicking.
func TestViolationDoesNotPanic(t *testing.T) {
	ftb, _ := run(t, func(h *logcheck.Handler) {
		slog.New(h).WithGroup("g").Info("synthetic", "x", 1)
	})
	if !ftb.Failed() {
		t.Error("expected failure")
	}
}

// FR-39: msg, level and time are produced by the handler, never by callers; a
// caller attribute with one of these keys (call site or With) fails.
func TestReservedHandlerKeysFail(t *testing.T) {
	for _, k := range []string{"msg", "level", "time"} {
		wantFail(t, k+"_call", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", k, "x") })
		wantFail(t, k+"_with", func(h *logcheck.Handler) { lg(h, "config").With(k, "x").Info("synthetic") })
		wantFail(t, k+"_call_no_component_with", func(h *logcheck.Handler) { slog.New(h).With("component", "config").Info("synthetic", slog.Int(k, 1)) })
	}
}

// FR-7: exactly one component per record. A factory-bound component plus a
// call-site component fails even when both are valid and even when equal.
func TestDuplicateComponentFails(t *testing.T) {
	wantFail(t, "valid_and_different", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "component", "grpc") })
	wantFail(t, "valid_and_same", func(h *logcheck.Handler) { lg(h, "config").Info("synthetic", "component", "config") })
	wantFail(t, "with_twice", func(h *logcheck.Handler) { lg(h, "config").With("component", "otel").Info("synthetic") })
	wantFail(t, "via_factory", func(h *logcheck.Handler) {
		l, _ := logging.NewFactory(h).Logger(logging.Tracing)
		l.Info("synthetic", "component", "tracing")
	})
}

// A failure message must identify the offending record (message and level)
// so the author can find the log call. It must not need attribute values.
func TestFailureMessagesIncludeRecordMessageAndLevel(t *testing.T) {
	cases := []struct {
		name  string
		level slog.Level
		msg   string
		fn    func(l *slog.Logger)
	}{
		{"warn_bad_key", slog.LevelWarn, "synthetic-marker-warn_bad_key",
			func(l *slog.Logger) { l.Warn("synthetic-marker-warn_bad_key", "bad_key", "v") }},
		{"error_group", slog.LevelError, "synthetic-marker-error_group",
			func(l *slog.Logger) {
				l.Error("synthetic-marker-error_group", slog.Group("g", slog.Int("retry_count", 1)))
			}},
		{"debug_nonnumeric", slog.LevelDebug, "synthetic-marker-debug_nonnumeric",
			func(l *slog.Logger) { l.Debug("synthetic-marker-debug_nonnumeric", "x_count", "n") }},
		{"info_dup_component", slog.LevelInfo, "synthetic-marker-info_dup_component",
			func(l *slog.Logger) { l.Info("synthetic-marker-info_dup_component", "component", "grpc") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := c.msg
			ftb, _ := run(t, func(h *logcheck.Handler) { c.fn(lg(h, "config")) })
			if !ftb.Failed() {
				t.Fatal("expected failure")
			}
			all := strings.Join(ftb.msgs, "\n")
			if !strings.Contains(all, msg) {
				t.Errorf("failure text lacks the record message: %q", all)
			}
			if !strings.Contains(strings.ToUpper(all), c.level.String()) {
				t.Errorf("failure text lacks the level %s: %q", c.level, all)
			}
		})
	}
	t.Run("missing_component", func(t *testing.T) {
		ftb, _ := run(t, func(h *logcheck.Handler) { slog.New(h).Info("synthetic-marker-nocomp") })
		all := strings.Join(ftb.msgs, "\n")
		if !strings.Contains(all, "synthetic-marker-nocomp") || !strings.Contains(strings.ToUpper(all), "INFO") {
			t.Errorf("failure text = %q", all)
		}
	})
}

// SPEC-001 FR-39 (Amendment 3), AC-41: a reason attribute must be a string.
func TestReasonKeyKindCheck_AC41(t *testing.T) {
	log := func(a slog.Attr) func(h *logcheck.Handler) {
		return func(h *logcheck.Handler) {
			lg(h, "pool").LogAttrs(context.Background(), slog.LevelInfo, "synthetic", a)
		}
	}
	wantPass(t, "string reason", log(slog.String("reason", "pool_queue_full")))
	wantFail(t, "int reason", log(slog.Int("reason", 3)))
	wantFail(t, "int64 reason", log(slog.Int64("reason", 3)))
	wantFail(t, "uint64 reason", log(slog.Uint64("reason", 3)))
	wantFail(t, "float reason", log(slog.Float64("reason", 1.5)))
	wantFail(t, "bool reason", log(slog.Bool("reason", true)))
	wantFail(t, "any reason", log(slog.Any("reason", struct{ A int }{1})))
	wantFail(t, "group reason", log(slog.Group("reason", slog.String("a", "b"))))
	wantFail(t, "duration reason", log(slog.Duration("reason", 1)))
	wantFail(t, "time reason", log(slog.Time("reason", time.Unix(0, 0))))
}

// A reason attribute bound with With is checked too.
func TestReasonKeyBoundWith_AC41(t *testing.T) {
	wantFail(t, "bound int reason", func(h *logcheck.Handler) {
		l := lg(h, "upstream").With("reason", 7)
		l.Info("synthetic")
	})
	wantPass(t, "bound string reason", func(h *logcheck.Handler) {
		lg(h, "upstream").With("reason", "eof").Info("synthetic")
	})
}

// Amendment 3 components are accepted by logcheck, and the key set is still
// closed.
func TestNewComponentsAndUnknownKey_AC41(t *testing.T) {
	for _, c := range []string{"proxy", "pool", "upstream"} {
		wantPass(t, "component "+c, func(h *logcheck.Handler) {
			lg(h, c).Info("synthetic", "reason", "x")
		})
	}
	wantFail(t, "unknown key still rejected", func(h *logcheck.Handler) {
		lg(h, "proxy").Info("synthetic", "reasons", "x")
	})
	wantFail(t, "unknown component still rejected", func(h *logcheck.Handler) {
		lg(h, "proxies").Info("synthetic")
	})
}
