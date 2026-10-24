package logging_test

import (
	"errors"
	"strings"
	"log/slog"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// FR-5: format is json or text.
func TestParseFormat(t *testing.T) {
	if f, err := logging.ParseFormat("json"); err != nil || f != logging.FormatJSON {
		t.Errorf("ParseFormat(json) = %v, %v", f, err)
	}
	if f, err := logging.ParseFormat("text"); err != nil || f != logging.FormatText {
		t.Errorf("ParseFormat(text) = %v, %v", f, err)
	}
	for in, want := range map[string]logging.Format{
		"JSON": logging.FormatJSON, "Text": logging.FormatText,
		"jSoN": logging.FormatJSON, "TEXT": logging.FormatText,
	} {
		if f, err := logging.ParseFormat(in); err != nil || f != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, f, err, want)
		}
	}
	for _, bad := range []string{"", "xml", "yaml", "json5", "j son", "console", marker(t, "fmt-")} {
		_, err := logging.ParseFormat(bad)
		if err == nil {
			t.Errorf("ParseFormat(%q) succeeded, want error", bad)
			continue
		}
		if !errors.Is(err, logging.ErrInvalidFormat) {
			t.Errorf("ParseFormat(%q) error %v is not ErrInvalidFormat", bad, err)
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("ParseFormat error echoes input %q: %v", bad, err)
		}
	}
}

// FR-6: levels are debug, info, warn, error; matching is case-insensitive.
func TestParseLevelAccepts(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError,
		"DEBUG": slog.LevelDebug, "Info": slog.LevelInfo,
		"WARN": slog.LevelWarn, "Error": slog.LevelError,
	}
	for in, want := range cases {
		got, err := logging.ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// FR-6: offsets and unknown names are rejected.
func TestParseLevelRejects(t *testing.T) {
	for _, bad := range []string{
		"", "debug-4", "info+2", "DEBUG-4", "warn+1", "error-1", "info+0",
		"trace", "fatal", "verbose", " info", "info ", "4", "0",
		marker(t, "lvl-"), marker(t, "lvl-") + "+2",
	} {
		_, err := logging.ParseLevel(bad)
		if err == nil {
			t.Errorf("ParseLevel(%q) succeeded, want error", bad)
			continue
		}
		if !errors.Is(err, logging.ErrInvalidLevel) {
			t.Errorf("ParseLevel(%q) error %v is not ErrInvalidLevel", bad, err)
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("ParseLevel error echoes input %q: %v", bad, err)
		}
	}
}
