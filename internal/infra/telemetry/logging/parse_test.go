package logging_test

import (
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
	for _, bad := range []string{"", "xml", "yaml", "json5", "j son", "console"} {
		if _, err := logging.ParseFormat(bad); err == nil {
			t.Errorf("ParseFormat(%q) succeeded, want error", bad)
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
	} {
		if _, err := logging.ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) succeeded, want error", bad)
		}
	}
}
