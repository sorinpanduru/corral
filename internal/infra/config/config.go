// Package config parses and validates the SPEC-001 logical settings
// (FR-5, FR-6, FR-16, FR-19, FR-21, FR-27) and provides the interim flag
// loader from ADR-0003.
//
// Compliance (FR-24, FR-27, GDPR data minimisation): a rejected value is never
// echoed into an error text, a log record or an errclass attribute. Errors name
// the setting only.
package config

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// Logical setting names (SPEC-001 Interfaces).
const (
	SettingLogFormat      = "log.format"
	SettingLogLevel       = "log.level"
	SettingAdminListen    = "admin.listen"
	SettingTracingEnabled = "tracing.enabled"
	SettingSampleRatio    = "tracing.sample_ratio"
)

// Settings is the validated set of logical settings. It is comparable.
type Settings struct {
	LogFormat      logging.Format
	LogLevel       slog.Level
	AdminListen    string
	TracingEnabled bool
	SampleRatio    float64
}

// Defaults returns the default settings (FR-5, FR-6, FR-16, FR-19, FR-21).
func Defaults() Settings {
	return Settings{
		LogFormat:      logging.FormatJSON,
		LogLevel:       slog.LevelInfo,
		AdminListen:    "127.0.0.1:9187",
		TracingEnabled: false,
		SampleRatio:    1.0,
	}
}

var (
	errNotBool       = errors.New("config: not a boolean")
	errBadListen     = errors.New("config: invalid listen address")
	errBadRatio      = errors.New("config: invalid sample ratio")
	errUnknown       = errors.New("config: unknown setting")
	errMissingValue  = errors.New("config: missing value")
	errRepeated      = errors.New("config: repeated setting")
	errPositionalArg = errors.New("config: unexpected argument")
)

// Error is a configuration error (FR-27). Its text carries the setting name
// and a constant reason only; the wrapped cause is reachable through Unwrap
// but never rendered.
type Error struct {
	Setting string
	cause   error
}

// NewError returns a *Error for setting wrapping cause (which may be nil).
func NewError(setting string, cause error) error {
	return &Error{Setting: setting, cause: cause}
}

// Error returns a constant reason plus the setting name; never the value or
// the cause text.
func (e *Error) Error() string { return "config: invalid setting " + e.Setting }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.cause }

// ConfigSetting names the setting; errclass uses it to classify as config.
func (e *Error) ConfigSetting() string { return e.Setting }

// Parse turns raw logical-setting values into Settings. An absent key uses the
// default. Any invalid value or unknown key is a *Error naming the setting.
func Parse(raw map[string]string) (Settings, error) {
	s := Defaults()
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic first error
	for _, k := range keys {
		v := raw[k]
		var err error
		switch k {
		case SettingLogFormat:
			s.LogFormat, err = logging.ParseFormat(v)
		case SettingLogLevel:
			s.LogLevel, err = logging.ParseLevel(v)
		case SettingAdminListen:
			err = validateListen(v)
			if err == nil {
				s.AdminListen = v
			}
		case SettingTracingEnabled:
			s.TracingEnabled, err = parseBool(v)
		case SettingSampleRatio:
			s.SampleRatio, err = parseRatio(v)
		default:
			err = errUnknown
		}
		if err != nil {
			return Settings{}, NewError(k, err)
		}
	}
	return s, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errNotBool
}

func parseRatio(v string) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
		return 0, errBadRatio
	}
	return f, nil
}

// validateListen requires host:port with a non-empty host and port 0..65535
// (FR-16). Errors never echo the address.
func validateListen(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil || host == "" {
		return errBadListen
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errBadListen
	}
	return nil
}

// logMsg is the constant message of the FR-27 error record.
const logMsg = "invalid configuration"

// LogError writes the FR-27 record: component=config, setting, the errclass
// attributes and a constant message. It never uses err.Error() and never
// echoes a value. A nil err logs nothing.
func LogError(l *slog.Logger, err error) {
	if err == nil {
		return
	}
	attrs := []slog.Attr{slog.String(attr.Component, string(logging.Config))}
	var ce interface{ ConfigSetting() string }
	if errors.As(err, &ce) {
		attrs = append(attrs, slog.String(attr.Setting, ce.ConfigSetting()))
	}
	attrs = append(attrs, errclass.LogAttrs(err)...)
	l.LogAttrs(context.Background(), slog.LevelError, logMsg, attrs...)
}

var knownSettings = map[string]struct{}{
	SettingLogFormat: {}, SettingLogLevel: {}, SettingAdminListen: {},
	SettingTracingEnabled: {}, SettingSampleRatio: {},
}

// FromArgs is the interim loader (ADR-0003, D1): -name=value flags named like
// the logical settings. Values are passed through unvalidated. Unknown,
// valueless and repeated flags are config errors naming the flag only.
func FromArgs(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			// Never name or echo a positional argument.
			return nil, NewError("argument", errPositionalArg)
		}
		body := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		name, val, hasVal := strings.Cut(body, "=")
		if _, ok := knownSettings[name]; !ok {
			return nil, NewError(name, errUnknown)
		}
		if !hasVal {
			return nil, NewError(name, errMissingValue)
		}
		if _, dup := out[name]; dup {
			return nil, NewError(name, errRepeated)
		}
		out[name] = val
	}
	return out, nil
}
