package config_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// Compliance (FR-24, FR-27, GDPR data minimisation): every value in this file
// is synthetic. Markers are random per test and are used to prove that a
// rejected value is never echoed into an error text, an errclass attribute or
// a log record.

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// cfgLogger builds a component=config logger through the factory, behind the
// logcheck handler (AC-5). LogError adds no component itself (D): the logger
// passed in must already carry it.
func cfgLogger(tb testing.TB, buf *bytes.Buffer) *slog.Logger {
	tb.Helper()
	l, err := logging.NewFactory(logcheck.New(tb, logcheck.Tee(buf, logging.FormatJSON))).Logger(logging.Config)
	if err != nil {
		tb.Fatalf("factory: %v", err)
	}
	return l
}

// settingOf extracts the setting a config error names, via the duck-typed
// ConfigSetting() contract that errclass relies on.
func settingOf(tb testing.TB, err error) string {
	tb.Helper()
	var ce interface{ ConfigSetting() string }
	if !errors.As(err, &ce) {
		tb.Fatalf("error %T does not expose ConfigSetting()", err)
	}
	return ce.ConfigSetting()
}

func isConfigError(err error) bool {
	var p *config.Error
	return errors.As(err, &p)
}

// requireRejected asserts err is a config.Error naming the setting, classified
// config, and that no part of it carries the marker.
func requireRejected(tb testing.TB, err error, setting, marker string) {
	tb.Helper()
	if err == nil {
		tb.Fatalf("expected a config error for %s, got nil", setting)
	}
	if !isConfigError(err) {
		tb.Fatalf("error %T is not a config.Error", err)
	}
	if got := settingOf(tb, err); got != setting {
		tb.Errorf("ConfigSetting() = %q, want %q", got, setting)
	}
	if !strings.Contains(err.Error(), setting) {
		tb.Errorf("Error() = %q, want it to contain the setting name %q", err.Error(), setting)
	}
	assertNoMarker(tb, "Error()", marker, err.Error())
	assertNoMarker(tb, "%v", marker, fmt.Sprintf("%v", err))
	assertNoMarker(tb, "%+v", marker, fmt.Sprintf("%+v", err))
	assertNoMarker(tb, "%#v", marker, fmt.Sprintf("%#v", err))
	for _, a := range errclass.LogAttrs(err) {
		assertNoMarker(tb, "errclass attr "+a.Key, marker, a.Key+"="+a.Value.String())
	}
	if c, _ := errclass.Classify(err); c != errclass.ClassConfig {
		tb.Errorf("errclass class = %q, want %q", c, errclass.ClassConfig)
	}
	// The whole log record must also stay marker-free.
	var buf bytes.Buffer
	config.LogError(cfgLogger(tb, &buf), err)
	assertNoMarker(tb, "log record", marker, buf.String())
}

func assertNoMarker(tb testing.TB, what, marker, s string) {
	tb.Helper()
	if marker != "" && strings.Contains(s, marker) {
		tb.Errorf("%s leaks the supplied value (marker found): %q", what, s)
	}
}

// AC-21 / AC-11 / FR-5, FR-6, FR-16, FR-19, FR-21: defaults.
func TestDefaults(t *testing.T) {
	d := config.Defaults()
	want := config.Settings{
		LogFormat:      logging.FormatJSON,
		LogLevel:       slog.LevelInfo,
		AdminListen:    "127.0.0.1:9187",
		TracingEnabled: false,
		SampleRatio:    1.0,
	}
	if d != want {
		t.Fatalf("Defaults() = %+v, want %+v", d, want)
	}
}

// AC-11 (default) / FR-16: with no admin.listen set the value is 127.0.0.1:9187.
func TestParse_AC11_DefaultAdminListen(t *testing.T) {
	for name, raw := range map[string]map[string]string{
		"nil map":   nil,
		"empty map": {},
		"other key": {config.SettingLogLevel: "debug"},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := config.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.AdminListen != "127.0.0.1:9187" {
				t.Fatalf("AdminListen = %q, want 127.0.0.1:9187", s.AdminListen)
			}
		})
	}
}

func TestSettingNameConstants(t *testing.T) {
	for got, want := range map[string]string{
		config.SettingLogFormat:      "log.format",
		config.SettingLogLevel:       "log.level",
		config.SettingAdminListen:    "admin.listen",
		config.SettingTracingEnabled: "tracing.enabled",
		config.SettingSampleRatio:    "tracing.sample_ratio",
	} {
		if got != want {
			t.Errorf("constant = %q, want %q", got, want)
		}
	}
}

// Absent keys use defaults (ADR-0003).
func TestParse_AbsentUsesDefaults(t *testing.T) {
	s, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s != config.Defaults() {
		t.Fatalf("Parse(nil) = %+v, want defaults %+v", s, config.Defaults())
	}
}

// FR-5, FR-6, FR-16, FR-19, FR-21: every setting accepted together.
func TestParse_AllSettingsValid(t *testing.T) {
	s, err := config.Parse(map[string]string{
		config.SettingLogFormat:      "text",
		config.SettingLogLevel:       "warn",
		config.SettingAdminListen:    "0.0.0.0:0",
		config.SettingTracingEnabled: "true",
		config.SettingSampleRatio:    "0.25",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := config.Settings{LogFormat: logging.FormatText, LogLevel: slog.LevelWarn, AdminListen: "0.0.0.0:0", TracingEnabled: true, SampleRatio: 0.25}
	if s != want {
		t.Fatalf("got %+v, want %+v", s, want)
	}
}

// AC-21 / FR-6: log.level accepted values, case-insensitive.
func TestParse_LogLevelAccepted(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug, "Info": slog.LevelInfo, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "WARN": slog.LevelWarn, "error": slog.LevelError, "Error": slog.LevelError,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			s, err := config.Parse(map[string]string{config.SettingLogLevel: in})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.LogLevel != want {
				t.Fatalf("LogLevel = %v, want %v", s.LogLevel, want)
			}
		})
	}
}

// AC-21 / FR-5: log.format accepted values.
func TestParse_LogFormatAccepted(t *testing.T) {
	for in, want := range map[string]logging.Format{"json": logging.FormatJSON, "text": logging.FormatText} {
		s, err := config.Parse(map[string]string{config.SettingLogFormat: in})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if s.LogFormat != want {
			t.Fatalf("%s: LogFormat = %q, want %q", in, s.LogFormat, want)
		}
	}
}

// AC-21 / FR-16 / B3: admin.listen accepted values, including explicit
// all-interface hosts and port 0.
func TestParse_AdminListenAccepted(t *testing.T) {
	for _, in := range []string{"127.0.0.1:9187", "127.0.0.1:0", "0.0.0.0:9187", "0.0.0.0:65535", "[::]:9187", "[::1]:8080", "localhost:8080", "db-proxy.example.internal:9187"} {
		t.Run(in, func(t *testing.T) {
			s, err := config.Parse(map[string]string{config.SettingAdminListen: in})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.AdminListen != in {
				t.Fatalf("AdminListen = %q, want %q", s.AdminListen, in)
			}
		})
	}
}

// AC-21 / FR-21: sample_ratio bounds are inclusive.
func TestParse_SampleRatioBoundsAccepted(t *testing.T) {
	for in, want := range map[string]float64{"0": 0, "0.0": 0, "0.5": 0.5, "1": 1, "1.0": 1} {
		t.Run(in, func(t *testing.T) {
			s, err := config.Parse(map[string]string{config.SettingSampleRatio: in})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if s.SampleRatio != want {
				t.Fatalf("SampleRatio = %v, want %v", s.SampleRatio, want)
			}
		})
	}
}

// AC-21 / FR-19: tracing.enabled bool.
func TestParse_TracingEnabledAccepted(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "false": false} {
		s, err := config.Parse(map[string]string{config.SettingTracingEnabled: in})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if s.TracingEnabled != want {
			t.Fatalf("%s: TracingEnabled = %v, want %v", in, s.TracingEnabled, want)
		}
	}
}

// AC-21 / FR-27: each setting rejects invalid values with a config.Error
// naming the setting and never echoing the value. Each case embeds a fresh
// marker, either as the whole value or inside it.
func TestParse_RejectsInvalid(t *testing.T) {
	type tc struct {
		name, setting string
		value         func(m string) string
	}
	lit := func(s string) func(string) string { return func(string) string { return s } }
	cases := []tc{
		{"log.format unknown", config.SettingLogFormat, func(m string) string { return m }},
		{"log.format xml", config.SettingLogFormat, lit("xml")},
		{"log.format empty", config.SettingLogFormat, lit("")},
		{"log.level offset debug-4", config.SettingLogLevel, lit("debug-4")},
		{"log.level offset info+2", config.SettingLogLevel, lit("info+2")},
		{"log.level offset with marker", config.SettingLogLevel, func(m string) string { return "debug-" + m }},
		{"log.level unknown", config.SettingLogLevel, func(m string) string { return m }},
		{"log.level empty", config.SettingLogLevel, lit("")},
		{"log.level padded", config.SettingLogLevel, lit(" debug")},
		{"admin.listen empty host", config.SettingAdminListen, lit(":9187")},
		{"admin.listen empty host port 0", config.SettingAdminListen, lit(":0")},
		{"admin.listen empty bracket host", config.SettingAdminListen, lit("[]:9187")},
		{"admin.listen empty host marker port", config.SettingAdminListen, func(m string) string { return ":" + m }},
		{"admin.listen no port", config.SettingAdminListen, lit("127.0.0.1")},
		{"admin.listen empty port", config.SettingAdminListen, lit("127.0.0.1:")},
		{"admin.listen port too big", config.SettingAdminListen, func(m string) string { return m + ".invalid:65536" }},
		{"admin.listen negative port", config.SettingAdminListen, func(m string) string { return m + ".invalid:-1" }},
		{"admin.listen text port", config.SettingAdminListen, func(m string) string { return "127.0.0.1:" + m }},
		{"admin.listen empty", config.SettingAdminListen, lit("")},
		{"admin.listen blank host", config.SettingAdminListen, lit(" :9187")},
		{"admin.listen spaces in host", config.SettingAdminListen, lit(" a b :1")},
		{"admin.listen newline in host", config.SettingAdminListen, lit("x\n:1")},
		{"admin.listen numeric 0", config.SettingAdminListen, lit("0:9187")},
		{"admin.listen numeric hex", config.SettingAdminListen, lit("0x0:9187")},
		{"admin.listen numeric short ip", config.SettingAdminListen, lit("127.1:9187")},
		{"admin.listen underscore host", config.SettingAdminListen, lit("a_b:9187")},
		{"admin.listen label too long", config.SettingAdminListen, lit(strings.Repeat("a", 64) + ".example:9187")},
		{"admin.listen host too long", config.SettingAdminListen, lit(strings.Repeat(strings.Repeat("a", 60)+".", 5) + "example:9187")},
		{"admin.listen empty label", config.SettingAdminListen, lit("a..b:9187")},
		{"admin.listen leading hyphen", config.SettingAdminListen, lit("-a.example:9187")},
		{"admin.listen marker with control", config.SettingAdminListen, func(m string) string { return m + "\x01:9187" }},
		{"admin.listen bare marker", config.SettingAdminListen, func(m string) string { return m }},
		{"admin.listen bare ipv6 no port", config.SettingAdminListen, lit("[::]")},
		{"tracing.enabled text", config.SettingTracingEnabled, func(m string) string { return m }},
		{"tracing.enabled empty", config.SettingTracingEnabled, lit("")},
		{"tracing.enabled maybe", config.SettingTracingEnabled, lit("maybe")},
		{"sample_ratio negative", config.SettingSampleRatio, lit("-0.1")},
		{"sample_ratio just above 1", config.SettingSampleRatio, lit("1.0000001")},
		{"sample_ratio 2", config.SettingSampleRatio, lit("2")},
		{"sample_ratio large", config.SettingSampleRatio, lit("1e300")},
		{"sample_ratio overflow", config.SettingSampleRatio, lit("1e999")},
		{"sample_ratio NaN", config.SettingSampleRatio, lit("NaN")},
		{"sample_ratio +Inf", config.SettingSampleRatio, lit("+Inf")},
		{"sample_ratio -Inf", config.SettingSampleRatio, lit("-Inf")},
		{"sample_ratio text", config.SettingSampleRatio, func(m string) string { return m }},
		{"sample_ratio empty", config.SettingSampleRatio, lit("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			_, err := config.Parse(map[string]string{c.setting: c.value(m)})
			requireRejected(t, err, c.setting, m)
		})
	}
}

// AC-21 / FR-27: the marker is also absent when the whole rejected value is a
// marker, for every setting (explicit no-echo check on every rejection path).
func TestParse_NoEchoForEverySetting(t *testing.T) {
	for _, setting := range []string{
		config.SettingLogFormat, config.SettingLogLevel, config.SettingAdminListen,
		config.SettingTracingEnabled, config.SettingSampleRatio,
	} {
		t.Run(setting, func(t *testing.T) {
			m := newMarker(t)
			_, err := config.Parse(map[string]string{setting: m})
			requireRejected(t, err, setting, m)
		})
	}
}

// FR-27: an unknown setting name is rejected; its value is never echoed.
func TestParse_UnknownSetting(t *testing.T) {
	m := newMarker(t)
	_, err := config.Parse(map[string]string{"log.levl": m})
	requireRejected(t, err, config.SettingUnknown, m)
	assertNoMarker(t, "Error()", "log.levl", err.Error()) // unknown names are never echoed (D)
	// A marker used as the key itself must not leak either.
	_, err = config.Parse(map[string]string{m: "x"})
	requireRejected(t, err, config.SettingUnknown, m)
}

// B3 / D: a 253-byte DNS name with 63-byte labels is accepted.
func TestParse_AdminListenMaxDNSName(t *testing.T) {
	host := strings.Join([]string{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", 61)}, ".")
	if len(host) != 253 {
		t.Fatalf("test bug: len = %d", len(host))
	}
	if _, err := config.Parse(map[string]string{config.SettingAdminListen: host + ":9187"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// D: case variants of bool and format are accepted.
func TestParse_CaseInsensitiveBoolAndFormat(t *testing.T) {
	s, err := config.Parse(map[string]string{config.SettingTracingEnabled: "TRUE", config.SettingLogFormat: "JSON"})
	if err != nil || !s.TracingEnabled || s.LogFormat != logging.FormatJSON {
		t.Fatalf("got %+v, %v", s, err)
	}
	s, err = config.Parse(map[string]string{config.SettingTracingEnabled: "False"})
	if err != nil || s.TracingEnabled {
		t.Fatalf("got %+v, %v", s, err)
	}
}

// FR-27 / D: LogError(nil) writes nothing.
func TestLogError_Nil(t *testing.T) {
	var buf bytes.Buffer
	config.LogError(cfgLogger(t, &buf), nil)
	if buf.Len() != 0 {
		t.Fatalf("LogError(nil) wrote %q", buf.String())
	}
}

// FR-27: a valid setting alongside an invalid one does not mask the error.
func TestParse_InvalidAmongValid(t *testing.T) {
	m := newMarker(t)
	_, err := config.Parse(map[string]string{
		config.SettingLogLevel:    "debug",
		config.SettingAdminListen: ":" + m,
	})
	requireRejected(t, err, config.SettingAdminListen, m)
}

// FR-27: Parse returns zero Settings or the error; callers must not use
// partial settings. We only require a non-nil error here and Defaults
// unchanged afterwards (no shared mutable state).
func TestParse_DoesNotMutateDefaults(t *testing.T) {
	before := config.Defaults()
	_, _ = config.Parse(map[string]string{config.SettingSampleRatio: "9"})
	if config.Defaults() != before {
		t.Fatal("Defaults() changed after a rejected Parse")
	}
}

// FR-27 / FR-29 / D3: config.Error carries the setting, wraps the cause and
// is classified config. Detail follows the wrapped cause (config/<detail>).
func TestNewError_Contract(t *testing.T) {
	m := newMarker(t)
	cause := errors.New("cause text " + m)
	err := config.NewError("admin.listen", cause)
	if err == nil {
		t.Fatal("NewError returned nil")
	}
	if !isConfigError(err) {
		t.Fatalf("%T is not a config.Error", err)
	}
	if got := settingOf(t, err); got != "admin.listen" {
		t.Fatalf("ConfigSetting() = %q", got)
	}
	var ce *config.Error
	if errors.As(err, &ce) && ce.Setting != "admin.listen" {
		t.Errorf("Setting = %q", ce.Setting)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want Unwrap to expose the cause")
	}
	if !strings.Contains(err.Error(), "admin.listen") {
		t.Errorf("Error() = %q, want setting name", err.Error())
	}
	// Error() carries the setting name and a constant reason only (FR-27).
	assertNoMarker(t, "Error()", m, err.Error())
	for _, a := range errclass.LogAttrs(err) {
		assertNoMarker(t, "errclass attr "+a.Key, m, a.Value.String())
	}
}

// FR-27: the Error() reason is constant, so two different causes give the
// same text for the same setting.
func TestNewError_ConstantReason(t *testing.T) {
	a := config.NewError("log.level", errors.New("one"))
	b := config.NewError("log.level", errors.New("two"))
	if a.Error() != b.Error() {
		t.Fatalf("Error() varies with cause: %q vs %q", a.Error(), b.Error())
	}
}

// D3 / FR-29 / FR-35: errclass classifies config errors as config/<detail>.
func TestErrclass_ConfigClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		detail errclass.Detail
	}{
		{"opaque cause", config.NewError("log.level", errors.New("x")), errclass.DetailOther},
		{"nil cause", config.NewError("log.level", nil), errclass.DetailOther},
		{"bind failure", config.NewError("admin.listen", syscall.EADDRINUSE), errclass.DetailEADDRINUSE},
		{"wrapped bind failure", fmt.Errorf("start: %w", config.NewError("admin.listen", fmt.Errorf("bind: %w", syscall.EADDRINUSE))), errclass.DetailEADDRINUSE},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class, detail := errclass.Classify(c.err)
			if class != errclass.ClassConfig || detail != c.detail {
				t.Fatalf("Classify = %s/%s, want config/%s", class, detail, c.detail)
			}
		})
	}
}

// D3: every Parse rejection is config/other (cause is a parse sentinel).
func TestErrclass_ParseRejectionIsConfigOther(t *testing.T) {
	_, err := config.Parse(map[string]string{config.SettingLogLevel: "debug-4"})
	class, detail := errclass.Classify(err)
	if class != errclass.ClassConfig || detail != errclass.DetailOther {
		t.Fatalf("Classify = %s/%s, want config/other", class, detail)
	}
	attrs := map[string]string{}
	for _, a := range errclass.LogAttrs(err) {
		attrs[a.Key] = a.Value.String()
	}
	if attrs["error"] != "config" || attrs["corral.error.detail"] != "other" {
		t.Fatalf("LogAttrs = %v, want error=config detail=other", attrs)
	}
}

func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one log line, got %d: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("record is not JSON: %v: %q", err, lines[0])
	}
	return rec
}

// AC-21 / FR-27: LogError writes one record with component=config, setting,
// errclass attributes and a constant message; no value is echoed.
func TestLogError_Record(t *testing.T) {
	cases := []struct {
		name, setting, value string
	}{
		{"log.level", config.SettingLogLevel, "debug-4"},
		{"admin.listen", config.SettingAdminListen, ":9187"},
		{"tracing.sample_ratio", config.SettingSampleRatio, "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			_, err := config.Parse(map[string]string{c.setting: c.value + m})
			if err == nil {
				// Marker-suffixed values must be invalid for all three cases.
				t.Fatal("expected rejection")
			}
			var buf bytes.Buffer
			config.LogError(cfgLogger(t, &buf), err)
			rec := decodeRecord(t, &buf)
			if rec["component"] != "config" {
				t.Errorf("component = %v, want config", rec["component"])
			}
			if rec["setting"] != c.setting {
				t.Errorf("setting = %v, want %q", rec["setting"], c.setting)
			}
			if rec["error"] != "config" {
				t.Errorf("error = %v, want config", rec["error"])
			}
			if rec["corral.error.detail"] != "other" {
				t.Errorf("corral.error.detail = %v, want other", rec["corral.error.detail"])
			}
			if lvl, _ := rec["level"].(string); lvl != "ERROR" {
				t.Errorf("level = %v, want ERROR", rec["level"])
			}
			msg, _ := rec["msg"].(string)
			if msg != "invalid configuration" {
				t.Errorf("msg = %q, want %q", msg, "invalid configuration")
			}
			assertNoMarker(t, "record", m, buf.String())
			for k := range rec {
				if !logging.AllowedKey(k) {
					t.Errorf("key %q is outside the FR-39 allowlist", k)
				}
			}
		})
	}
}

// FR-27: the message is a constant and does not depend on the setting or value.
func TestLogError_ConstantMessage(t *testing.T) {
	msgs := map[string]bool{}
	for _, s := range []string{config.SettingLogLevel, config.SettingAdminListen} {
		_, err := config.Parse(map[string]string{s: ":"})
		var buf bytes.Buffer
		config.LogError(cfgLogger(t, &buf), err)
		msgs[decodeRecord(t, &buf)["msg"].(string)] = true
	}
	if len(msgs) != 1 {
		t.Fatalf("message varies across settings: %v", msgs)
	}
}

// FR-27: a config error from the bind path (wrapped cause EADDRINUSE) is
// logged with detail EADDRINUSE and no address.
func TestLogError_BindFailureDetail(t *testing.T) {
	m := newMarker(t)
	err := config.NewError("admin.listen", fmt.Errorf("listen "+m+": %w", syscall.EADDRINUSE))
	var buf bytes.Buffer
	config.LogError(cfgLogger(t, &buf), err)
	rec := decodeRecord(t, &buf)
	if rec["setting"] != "admin.listen" || rec["error"] != "config" || rec["corral.error.detail"] != "EADDRINUSE" {
		t.Fatalf("record = %v", rec)
	}
	assertNoMarker(t, "record", m, buf.String())
}

// FR-27: LogError handles a wrapped config error (errors.As semantics).
func TestLogError_WrappedConfigError(t *testing.T) {
	err := fmt.Errorf("startup: %w", config.NewError(config.SettingLogFormat, errors.New("x")))
	var buf bytes.Buffer
	config.LogError(cfgLogger(t, &buf), err)
	rec := decodeRecord(t, &buf)
	if rec["setting"] != config.SettingLogFormat {
		t.Fatalf("setting = %v", rec["setting"])
	}
}

// FR-27: errors raised before the logger is configured go to the bootstrap
// logger (stdout JSON, component=config).
func TestLogError_BootstrapLogger(t *testing.T) {
	m := newMarker(t)
	_, err := config.Parse(map[string]string{config.SettingAdminListen: ":" + m})
	var buf bytes.Buffer
	config.LogError(logging.BootstrapLogger(&buf), err)
	rec := decodeRecord(t, &buf)
	if rec["component"] != "config" || rec["setting"] != config.SettingAdminListen {
		t.Fatalf("record = %v", rec)
	}
	// LogError adds no component of its own: exactly one key in the raw line (D).
	if n := strings.Count(buf.String(), `"component":`); n != 1 {
		t.Fatalf("component key appears %d times, want 1: %q", n, buf.String())
	}
	assertNoMarker(t, "record", m, buf.String())
}

// ---- Interim loader (ADR-0003, D1) ----

// D1: flags named like the logical settings become the raw map.
func TestFromArgs_Valid(t *testing.T) {
	raw, err := config.FromArgs([]string{
		"-log.level=debug", "-log.format=text", "-admin.listen=127.0.0.1:0",
		"-tracing.enabled=true", "-tracing.sample_ratio=0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"log.level": "debug", "log.format": "text", "admin.listen": "127.0.0.1:0",
		"tracing.enabled": "true", "tracing.sample_ratio": "0.5",
	}
	if len(raw) != len(want) {
		t.Fatalf("raw = %v, want %v", raw, want)
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("raw[%q] = %q, want %q", k, raw[k], v)
		}
	}
}

// D1: no arguments gives an empty map and Parse yields defaults.
func TestFromArgs_NoArgs(t *testing.T) {
	for _, args := range [][]string{nil, {}} {
		raw, err := config.FromArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 {
			t.Fatalf("raw = %v, want empty", raw)
		}
		s, err := config.Parse(raw)
		if err != nil || s != config.Defaults() {
			t.Fatalf("Parse = %+v, %v", s, err)
		}
	}
}

// D1 / AC-21: the loader end-to-end with Parse (log.level=DEBUG accepted).
func TestFromArgs_ThenParse(t *testing.T) {
	raw, err := config.FromArgs([]string{"-log.level=DEBUG", "-admin.listen=0.0.0.0:0"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.LogLevel != slog.LevelDebug || s.AdminListen != "0.0.0.0:0" {
		t.Fatalf("got %+v", s)
	}
}

// D1 / AC-21: invalid values arrive intact from the loader (it does not
// validate) and are rejected by Parse without echo.
func TestFromArgs_ThenParse_Rejects(t *testing.T) {
	m := newMarker(t)
	raw, err := config.FromArgs([]string{"-admin.listen=:" + m})
	if err != nil {
		t.Fatalf("FromArgs must not validate values: %v", err)
	}
	_, err = config.Parse(raw)
	requireRejected(t, err, config.SettingAdminListen, m)
}

// D1: a value may contain '=' and is passed through raw.
func TestFromArgs_ValueContainingEquals(t *testing.T) {
	raw, err := config.FromArgs([]string{"-admin.listen=a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if raw["admin.listen"] != "a=b" {
		t.Fatalf("raw = %v", raw)
	}
}

// D1 / FR-27 / D: an unknown flag is a config error with setting "unknown";
// neither the flag name nor the value is ever echoed.
func TestFromArgs_UnknownFlag(t *testing.T) {
	for name, mk := range map[string]func(m string) string{
		"with value":        func(m string) string { return "-no.such.flag=" + m },
		"typo":              func(m string) string { return "-log.levl=" + m },
		"without value":     func(string) string { return "-no.such.flag" },
		"secret-like name":  func(m string) string { return "-" + m },
		"secret-like value": func(m string) string { return "-" + m + "=v" },
		"double dash name":  func(m string) string { return "--" + m + "=v" },
		"newline in name":   func(m string) string { return "-bad\nname" + m + "=v" },
		"control in name":   func(m string) string { return "-bad\x01\x1b[31mname" + m + "=v" },
		"oversized name":    func(m string) string { return "-" + strings.Repeat("a", 1<<20) + m + "=v" },
	} {
		t.Run(name, func(t *testing.T) {
			m := newMarker(t)
			arg := mk(m)
			_, err := config.FromArgs([]string{arg})
			requireRejected(t, err, config.SettingUnknown, m)
			for _, frag := range []string{"no.such.flag", "log.levl"} {
				if strings.Contains(arg, frag) {
					assertNoMarker(t, "Error()", frag, err.Error())
				}
			}
			if len(err.Error()) > 1024 {
				t.Errorf("Error() length %d is unbounded", len(err.Error()))
			}
		})
	}
}

// D: malformed flags and positional arguments. "--" is not an end-of-flags
// marker; it is a malformed flag.
func TestFromArgs_MalformedAndPositional(t *testing.T) {
	cases := []struct {
		name    string
		mk      func(m string) []string
		setting string
	}{
		{"single dash", func(string) []string { return []string{"-"} }, config.SettingUnknown},
		{"double dash", func(string) []string { return []string{"--"} }, config.SettingUnknown},
		{"double dash then flags", func(string) []string { return []string{"--", "-log.level=debug"} }, config.SettingUnknown},
		{"empty name with value", func(m string) []string { return []string{"-=" + m} }, config.SettingUnknown},
		{"double dash empty name", func(m string) []string { return []string{"--=" + m} }, config.SettingUnknown},
		{"empty arg", func(string) []string { return []string{""} }, config.SettingArgument},
		{"positional", func(m string) []string { return []string{m} }, config.SettingArgument},
		{"positional after flag", func(m string) []string { return []string{"-log.level=debug", m} }, config.SettingArgument},
		{"positional with newline", func(m string) []string { return []string{"a\n" + m} }, config.SettingArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			_, err := config.FromArgs(c.mk(m))
			requireRejected(t, err, c.setting, m)
		})
	}
	if config.SettingUnknown != "unknown" || config.SettingArgument != "argument" {
		t.Fatalf("constants = %q, %q", config.SettingUnknown, config.SettingArgument)
	}
}

// D1: a double-dash form of a known flag is accepted (Go flag style).
func TestFromArgs_DoubleDashKnownFlag(t *testing.T) {
	raw, err := config.FromArgs([]string{"--log.level=debug"})
	if err != nil {
		t.Fatal(err)
	}
	if raw[config.SettingLogLevel] != "debug" || len(raw) != 1 {
		t.Fatalf("raw = %v", raw)
	}
}

// D1 / FR-27: a known flag with no value is a config error naming the flag.
func TestFromArgs_MissingValue(t *testing.T) {
	cases := map[string][]string{
		"last arg":          {"-log.level"},
		"before other flag": {"-log.level", "-admin.listen=127.0.0.1:0"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.FromArgs(args)
			requireRejected(t, err, "log.level", "")
		})
	}
}

// D1 / FR-27: a repeated flag is a config error naming the flag, never either value.
func TestFromArgs_RepeatedFlag(t *testing.T) {
	m1, m2 := newMarker(t), newMarker(t)
	_, err := config.FromArgs([]string{"-log.level=" + m1, "-admin.listen=127.0.0.1:0", "-log.level=" + m2})
	requireRejected(t, err, "log.level", m1)
	assertNoMarker(t, "Error()", m2, err.Error())
	var buf bytes.Buffer
	config.LogError(cfgLogger(t, &buf), err)
	assertNoMarker(t, "log record", m2, buf.String())
}

// D1: repeating a flag with the same value is still an error.
func TestFromArgs_RepeatedSameValue(t *testing.T) {
	_, err := config.FromArgs([]string{"-log.level=info", "-log.level=info"})
	requireRejected(t, err, "log.level", "")
}

// D1 / FR-27 / D3: loader errors classify as config/other.
func TestFromArgs_ErrorsClassifyAsConfig(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown":  {"-nope=1"},
		"missing":  {"-log.level"},
		"repeated": {"-log.level=a", "-log.level=b"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.FromArgs(args)
			if err == nil {
				t.Fatal("expected error")
			}
			class, detail := errclass.Classify(err)
			if class != errclass.ClassConfig || detail != errclass.DetailOther {
				t.Fatalf("Classify = %s/%s, want config/other", class, detail)
			}
		})
	}
}
