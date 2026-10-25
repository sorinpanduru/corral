// Package config holds the configuration error type and the FR-11 record
// emitters shared by the loader, the runtime and the admin listener. Parsing
// and validation live in internal/infra/config/{types,registry,loader}.
//
// Compliance (SPEC-003 FR-11, SPEC-001 FR-24, GDPR data minimisation): a
// rejected value is never echoed into an error text, a log record or an
// errclass attribute. Errors name the setting only.
package config

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

// Logical setting names (SPEC-001 Interfaces).
const (
	SettingLogFormat      = "log.format"
	SettingLogLevel       = "log.level"
	SettingAdminListen    = "admin.listen"
	SettingTracingEnabled = "tracing.enabled"
	SettingSampleRatio    = "tracing.sample_ratio"

	// SettingUnknown names errors for unknown or malformed settings and flags.
	// The supplied name is never echoed (it may be a mistyped secret).
	SettingUnknown = "unknown"
	// SettingArgument names errors for positional command-line arguments.
	SettingArgument = "argument"
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

// logMsg is the constant message of the FR-27 error record.
const logMsg = "invalid configuration"

// LogError writes the FR-27 record: setting, the errclass attributes and a
// constant message at ERROR level. The logger must already carry
// component=config (the bootstrap logger and the logging factory do); LogError
// does not add it. It never uses err.Error() and never echoes a value. A nil
// err logs nothing.
func LogError(l *slog.Logger, err error) {
	if err == nil {
		return
	}
	l.LogAttrs(context.Background(), slog.LevelError, logMsg, errorAttrs(err)...)
}

// errorAttrs returns the setting and errclass attributes of err; never values.
func errorAttrs(err error) []slog.Attr {
	var attrs []slog.Attr
	var ce errclass.ConfigError
	if errors.As(err, &ce) {
		attrs = append(attrs, slog.String(attr.Setting, ce.ConfigSetting()))
	}
	return append(attrs, errclass.LogAttrs(err)...)
}
