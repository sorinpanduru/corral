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

// FR-27 / D: LogError(nil) writes nothing.
func TestLogError_Nil(t *testing.T) {
	var buf bytes.Buffer
	config.LogError(cfgLogger(t, &buf), nil)
	if buf.Len() != 0 {
		t.Fatalf("LogError(nil) wrote %q", buf.String())
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
			// The cause text carries the value; Error() and the record must not.
			err := config.NewError(c.setting, errors.New("rejected value "+c.value+m))
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
		err := config.NewError(s, errors.New("x"))
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
	err := config.NewError(config.SettingAdminListen, errors.New(":"+m))
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
