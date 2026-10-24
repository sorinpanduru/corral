package config_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// SPEC-003 FR-8, FR-11, FR-18, FR-20 (T7): the configuration log records.
// They follow SPEC-001 FR-7 (component), FR-27 (error record), FR-29 (error
// attributes) and FR-39 (key allowlist).
//
// Compliance (SPEC-003 FR-11, FR-20, GDPR data minimisation): records carry
// setting names only, never values. Values here are synthetic markers.
//
// Helper names in this file are prefixed "rec" so they never clash with the
// helpers of config_test.go, which T12 may remove with the interim loader.

func recMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// recLevelFilter makes the logcheck handler (enabled at every level) honour a
// minimum level, so level filtering is observable.
type recLevelFilter struct {
	slog.Handler
	level slog.Level
}

func (h recLevelFilter) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h recLevelFilter) WithAttrs(as []slog.Attr) slog.Handler {
	return recLevelFilter{h.Handler.WithAttrs(as), h.level}
}
func (h recLevelFilter) WithGroup(n string) slog.Handler {
	return recLevelFilter{h.Handler.WithGroup(n), h.level}
}

// recLogger builds a component=config logger through the factory, behind the
// logcheck handler (FR-7, FR-39 enforcement), at the given minimum level. The
// caller binds the component; the emitters add none.
func recLogger(tb testing.TB, buf *bytes.Buffer, level slog.Level) (*slog.Logger, *logcheck.Handler) {
	tb.Helper()
	chk := logcheck.New(tb, logcheck.Tee(buf, logging.FormatJSON))
	l, err := logging.NewFactory(recLevelFilter{chk, level}).Logger(logging.Config)
	if err != nil {
		tb.Fatalf("factory: %v", err)
	}
	return l, chk
}

func recDecodeAll(tb testing.TB, buf *bytes.Buffer) []map[string]any {
	tb.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			tb.Fatalf("record is not JSON: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

func recKeys(rec map[string]any) []string {
	var ks []string
	for k := range rec {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ---- message constants ----

func TestMessageConstants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"MsgInvalid", config.MsgInvalid, "invalid configuration"},                                                    // FR-11
		{"MsgUnknownEnv", config.MsgUnknownEnv, "unknown CORRAL_ environment variables ignored"},                      // FR-8
		{"MsgLoaded", config.MsgLoaded, "configuration loaded"},                                                       // FR-20
		{"MsgReloadUnsupported", config.MsgReloadUnsupported, "configuration reload not supported; restart required"}, // FR-18
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// ---- LogErrors (FR-11) ----

func TestLogErrors_OneRecordPerErrorInOrder(t *testing.T) {
	var buf bytes.Buffer
	l, chk := recLogger(t, &buf, slog.LevelDebug)
	errs := []error{
		config.NewError("argument", nil),
		config.NewError("CORRAL_LOG_LEVEL", nil),
		config.NewError("log.format", nil),
		config.NewError("CORRAL_UPSTREAM_PASSWORD_FILE", nil),
		config.NewError("unknown", nil),
	}
	config.LogErrors(l, errs)
	recs := recDecodeAll(t, &buf)
	if len(recs) != len(errs) {
		t.Fatalf("got %d records, want %d", len(recs), len(errs))
	}
	if n := len(chk.Records()); n != len(errs) {
		t.Fatalf("logcheck captured %d records, want %d", n, len(errs))
	}
	for i, rec := range recs {
		want := errs[i].(*config.Error).Setting
		if rec["setting"] != want {
			t.Errorf("record %d: setting = %v, want %q (order must be kept)", i, rec["setting"], want)
		}
		if rec["msg"] != config.MsgInvalid {
			t.Errorf("record %d: msg = %v", i, rec["msg"])
		}
		if rec["level"] != "ERROR" {
			t.Errorf("record %d: level = %v, want ERROR", i, rec["level"])
		}
		if rec["component"] != "config" {
			t.Errorf("record %d: component = %v", i, rec["component"])
		}
		if rec["error"] != "config" {
			t.Errorf("record %d: error = %v, want class config", i, rec["error"])
		}
		if _, ok := rec["corral.error.detail"]; !ok {
			t.Errorf("record %d: missing corral.error.detail (SPEC-001 FR-29)", i)
		}
		for _, k := range recKeys(rec) {
			if !logging.AllowedKey(k) {
				t.Errorf("record %d: key %q outside the FR-39 allowlist", i, k)
			}
		}
	}
}

// The slice is not reordered or deduplicated: the loader owns FR-13 order.
func TestLogErrors_DoesNotReorderOrDedupe(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	errs := []error{config.NewError("z", nil), config.NewError("a", nil), config.NewError("z", nil)}
	config.LogErrors(l, errs)
	var got []string
	for _, r := range recDecodeAll(t, &buf) {
		got = append(got, r["setting"].(string))
	}
	if !reflect.DeepEqual(got, []string{"z", "a", "z"}) {
		t.Fatalf("settings = %v, want [z a z]", got)
	}
	if errs[0].(*config.Error).Setting != "z" || errs[1].(*config.Error).Setting != "a" {
		t.Fatal("input slice was modified")
	}
}

func TestLogErrors_NoErrorsNoRecords(t *testing.T) {
	for name, errs := range map[string][]error{"nil": nil, "empty": {}} {
		var buf bytes.Buffer
		l, chk := recLogger(t, &buf, slog.LevelDebug)
		config.LogErrors(l, errs)
		if buf.Len() != 0 || len(chk.Records()) != 0 {
			t.Errorf("%s: wrote %d bytes", name, buf.Len())
		}
	}
}

// LogErrors delegates to LogError, so a nil element logs nothing (LogError's
// documented contract) and does not stop later errors.
func TestLogErrors_NilElementSkipped(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogErrors(l, []error{config.NewError("a", nil), nil, config.NewError("b", nil)})
	recs := recDecodeAll(t, &buf)
	if len(recs) != 2 || recs[0]["setting"] != "a" || recs[1]["setting"] != "b" {
		t.Fatalf("records = %v", recs)
	}
}

// FR-11: never the supplied value, part of it, a path or file contents, even
// when the wrapped cause text carries them.
func TestLogErrors_NeverCarriesValuesOrCauseText(t *testing.T) {
	m := recMarker(t)
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogErrors(l, []error{
		config.NewError("CORRAL_ADMIN_LISTEN", fmt.Errorf("bad value %s", m)),
		config.NewError("upstream.password_file", fmt.Errorf("open /run/secrets/%s: no such file", m)),
	})
	if buf.Len() == 0 {
		t.Fatal("no output")
	}
	if strings.Contains(buf.String(), m) {
		t.Fatal("log output carries a marker from the error cause")
	}
}

func TestLogErrors_BootstrapLoggerAddsNoComponent(t *testing.T) {
	var buf bytes.Buffer
	config.LogErrors(logging.BootstrapLogger(&buf), []error{config.NewError("a", nil), config.NewError("b", nil)})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, line := range lines {
		if n := strings.Count(line, `"component":`); n != 1 {
			t.Errorf("line %d: component key appears %d times, want 1 (the caller binds it)", i, n)
		}
	}
}

// ---- LogUnknownEnv (FR-8) ----

func TestLogUnknownEnv_Record(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogUnknownEnv(l, []string{"CORRAL_LOG_LEVLE", "CORRAL_ZZZ"})
	recs := recDecodeAll(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want exactly 1", len(recs))
	}
	rec := recs[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["msg"] != config.MsgUnknownEnv {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["component"] != "config" {
		t.Errorf("component = %v", rec["component"])
	}
	if rec["setting"] != "CORRAL_LOG_LEVLE,CORRAL_ZZZ" {
		t.Errorf("setting = %v, want names joined with a comma", rec["setting"])
	}
	// Exactly time, level, msg, component, setting: no error class, no values.
	want := []string{"component", "level", "msg", "setting", "time"}
	if got := recKeys(rec); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestLogUnknownEnv_SingleName(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogUnknownEnv(l, []string{"CORRAL_X"})
	recs := recDecodeAll(t, &buf)
	if len(recs) != 1 || recs[0]["setting"] != "CORRAL_X" {
		t.Fatalf("records = %v", recs)
	}
}

// The emitter renders the names it is given, in the given order. Sorting and
// the <invalid-name> rendering belong to the loader (T5).
func TestLogUnknownEnv_JoinsInGivenOrder(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogUnknownEnv(l, []string{"CORRAL_Z", "CORRAL_<invalid-name>", "CORRAL_A"})
	recs := recDecodeAll(t, &buf)
	if len(recs) != 1 || recs[0]["setting"] != "CORRAL_Z,CORRAL_<invalid-name>,CORRAL_A" {
		t.Fatalf("records = %v", recs)
	}
}

func TestLogUnknownEnv_EmptyListNoRecord(t *testing.T) {
	for name, names := range map[string][]string{"nil": nil, "empty": {}} {
		var buf bytes.Buffer
		l, chk := recLogger(t, &buf, slog.LevelDebug)
		config.LogUnknownEnv(l, names)
		if buf.Len() != 0 || len(chk.Records()) != 0 {
			t.Errorf("%s: emitted a record for an empty list", name)
		}
	}
}

// ---- LogLoaded (FR-20) ----

func TestLogLoaded_Record(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogLoaded(l, []string{"admin.listen", "log.level"})
	recs := recDecodeAll(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want exactly 1", len(recs))
	}
	rec := recs[0]
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["msg"] != config.MsgLoaded {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["component"] != "config" {
		t.Errorf("component = %v", rec["component"])
	}
	// AC-11: the supplied names, comma-joined, and nothing else.
	if rec["setting"] != "admin.listen,log.level" {
		t.Errorf("setting = %v, want admin.listen,log.level", rec["setting"])
	}
	want := []string{"component", "level", "msg", "setting", "time"}
	if got := recKeys(rec); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

// AC-11: when nothing is supplied the record is still emitted, with an empty
// setting string (the attribute is present).
func TestLogLoaded_NothingSupplied(t *testing.T) {
	for name, names := range map[string][]string{"nil": nil, "empty": {}} {
		var buf bytes.Buffer
		l, _ := recLogger(t, &buf, slog.LevelDebug)
		config.LogLoaded(l, names)
		recs := recDecodeAll(t, &buf)
		if len(recs) != 1 {
			t.Fatalf("%s: got %d records, want exactly 1", name, len(recs))
		}
		v, present := recs[0]["setting"]
		if !present || v != "" {
			t.Errorf("%s: setting = %v (present %v), want the empty string", name, v, present)
		}
		if recs[0]["msg"] != "configuration loaded" {
			t.Errorf("%s: msg = %v", name, recs[0]["msg"])
		}
	}
}

func TestLogLoaded_SingleNameAndGivenOrder(t *testing.T) {
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogLoaded(l, []string{"log.level"})
	config.LogLoaded(l, []string{"tracing.enabled", "admin.listen"}) // sorting is the loader's job
	recs := recDecodeAll(t, &buf)
	if len(recs) != 2 || recs[0]["setting"] != "log.level" || recs[1]["setting"] != "tracing.enabled,admin.listen" {
		t.Fatalf("records = %v", recs)
	}
}

// FR-20 / SPEC-001 FR-7: level filtering applies. At warn the info record is
// not emitted; the warn record is.
func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l, chk := recLogger(t, &buf, slog.LevelWarn)
	config.LogLoaded(l, []string{"log.level"})
	if buf.Len() != 0 || len(chk.Records()) != 0 {
		t.Fatalf("LogLoaded emitted at level warn")
	}
	config.LogUnknownEnv(l, []string{"CORRAL_X"})
	if len(recDecodeAll(t, &buf)) != 1 {
		t.Fatal("LogUnknownEnv (warn) was filtered at level warn")
	}

	buf.Reset()
	l, _ = recLogger(t, &buf, slog.LevelError)
	config.LogUnknownEnv(l, []string{"CORRAL_X"})
	config.LogLoaded(l, nil)
	if buf.Len() != 0 {
		t.Fatal("warn or info record emitted at level error")
	}
	config.LogErrors(l, []error{config.NewError("a", nil)})
	if len(recDecodeAll(t, &buf)) != 1 {
		t.Fatal("error record was filtered at level error")
	}
}

// The emitters add no component themselves (SPEC-001 rule): with the
// bootstrap logger the component key appears exactly once per record.
func TestUnknownEnvAndLoaded_BootstrapLoggerAddsNoComponent(t *testing.T) {
	var buf bytes.Buffer
	l := logging.BootstrapLogger(&buf)
	config.LogUnknownEnv(l, []string{"CORRAL_X"})
	config.LogLoaded(l, []string{"log.level"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, line := range lines {
		if n := strings.Count(line, `"component":`); n != 1 {
			t.Errorf("line %d: component key appears %d times, want 1", i, n)
		}
	}
}

// Names only: the emitters take names, so a value-bearing marker supplied in
// the CORRAL_ name position is the only way a marker could appear; it must
// appear verbatim in `setting` and nowhere else.
func TestNamesOnlyAppearInSetting(t *testing.T) {
	m := recMarker(t)
	var buf bytes.Buffer
	l, _ := recLogger(t, &buf, slog.LevelDebug)
	config.LogUnknownEnv(l, []string{"CORRAL_" + m})
	config.LogLoaded(l, []string{"x." + m})
	for _, rec := range recDecodeAll(t, &buf) {
		for k, v := range rec {
			if k != "setting" && strings.Contains(fmt.Sprint(v), m) {
				t.Errorf("name leaked into attribute %q", k)
			}
		}
	}
}
