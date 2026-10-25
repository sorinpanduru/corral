package config

import (
	"context"
	"log/slog"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

// Constant messages of the configuration records (SPEC-003 FR-8, FR-11,
// FR-18, FR-20).
const (
	// MsgInvalid is the FR-11 error record message (same as LogError's).
	MsgInvalid = logMsg
	// MsgAdminBindFailed is the record for a failed admin bind. It is not a
	// configuration error (FR-24), so it is not MsgInvalid.
	MsgAdminBindFailed = "admin listener bind failed"
	// MsgUnknownEnv is the FR-8 warning for unknown CORRAL_ variables.
	MsgUnknownEnv = "unknown CORRAL_ environment variables ignored"
	// MsgLoaded is the FR-20 info record.
	MsgLoaded = "configuration loaded"
	// MsgReloadUnsupported is the FR-18 SIGHUP warning.
	MsgReloadUnsupported = "configuration reload not supported; restart required"
)

// LogErrors writes one FR-11 record per error, in the given order. Nil
// elements are skipped (see LogError).
//
// Compliance (FR-11, GDPR data minimisation): records carry setting names
// only, never values, paths or cause text. The caller binds component.
func LogErrors(l *slog.Logger, errs []error) {
	for _, err := range errs {
		LogError(l, err)
	}
}

// LogUnknownEnv warns about ignored CORRAL_ variables. names are variable
// names (never values), joined with a comma in the given order. An empty list
// emits nothing.
func LogUnknownEnv(l *slog.Logger, names []string) {
	if len(names) == 0 {
		return
	}
	l.LogAttrs(context.Background(), slog.LevelWarn, MsgUnknownEnv,
		slog.String(attr.Setting, strings.Join(names, ",")))
}

// LogLoaded records which settings were supplied (names only, comma-joined in
// the given order; an empty string when none).
func LogLoaded(l *slog.Logger, supplied []string) {
	l.LogAttrs(context.Background(), slog.LevelInfo, MsgLoaded,
		slog.String(attr.Setting, strings.Join(supplied, ",")))
}

// LogAdminBindFailed writes the admin bind failure record: error level, the
// setting name and the errclass attributes, never the address or port. The
// caller binds component=config.
func LogAdminBindFailed(l *slog.Logger, err error) {
	if err == nil {
		return
	}
	l.LogAttrs(context.Background(), slog.LevelError, MsgAdminBindFailed, errorAttrs(err)...)
}

// MsgWireBindFailed is the constant message of the wire listener bind failure.
const MsgWireBindFailed = "wire listener bind failed"

// LogWireBindFailed writes the wire listener bind failure record: error level,
// the setting name (proxy.listen) and the errclass attributes, never the
// address or port. The caller binds component=config.
func LogWireBindFailed(l *slog.Logger, err error) {
	if err == nil {
		return
	}
	l.LogAttrs(context.Background(), slog.LevelError, MsgWireBindFailed, errorAttrs(err)...)
}
