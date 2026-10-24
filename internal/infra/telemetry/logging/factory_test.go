package logging_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// FR-7, AC-5 (factory part): every component in the fixed set yields a logger
// whose records carry that component.
func TestFactoryBindsComponent(t *testing.T) {
	for _, c := range logging.Components() {
		t.Run(string(c), func(t *testing.T) {
			var buf syncBuffer
			fac := logging.NewFactory(checked(t, logging.NewHandler(&buf, logging.FormatJSON, slog.LevelInfo), false))
			l, err := fac.Logger(c)
			if err != nil || l == nil {
				t.Fatalf("Logger(%q) = %v, %v", c, l, err)
			}
			l.Info("synthetic message")
			l.With("retry_count", 1).Warn("synthetic with")
			recs := jsonLines(t, &buf)
			if len(recs) != 2 {
				t.Fatalf("got %d records", len(recs))
			}
			for _, r := range recs {
				if r["component"] != string(c) {
					t.Errorf("component = %v, want %q", r["component"], c)
				}
			}
		})
	}
}

// FR-7: the component key is emitted once, at top level, as a string (no
// groups).
func TestFactoryComponentTopLevelOnce(t *testing.T) {
	l, buf := newLogger(t, logging.FormatJSON, slog.LevelInfo, logging.OTel)
	l.Info("synthetic message", "setting", "OTEL_SERVICE_NAME")
	lines := buf.Lines()
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	if n := strings.Count(lines[0], `"component"`); n != 1 {
		t.Errorf(`"component" appears %d times in %s`, n, lines[0])
	}
	r := jsonLines(t, buf)[0]
	for k, v := range r {
		if _, isObj := v.(map[string]any); isObj {
			t.Errorf("key %q is a nested object (group) in %s", k, lines[0])
		}
	}
}

// FR-7, AC-5 (factory part): components outside the fixed set are rejected.
func TestFactoryRejectsUnknownComponent(t *testing.T) {
	var buf syncBuffer
	fac := logging.NewFactory(checked(t, logging.NewHandler(&buf, logging.FormatJSON, slog.LevelInfo), false))
	for _, bad := range []string{"", "proxy", "CONFIG", "third_party", "admin-http", "unknown", marker(t, "cmp-")} {
		l, err := fac.Logger(logging.Component(bad))
		if err == nil {
			t.Errorf("Logger(%q) succeeded, want error", bad)
		} else {
			if !errors.Is(err, logging.ErrUnknownComponent) {
				t.Errorf("Logger(%q) error %v is not ErrUnknownComponent", bad, err)
			}
			if bad != "" && strings.Contains(err.Error(), bad) {
				t.Errorf("Logger error echoes input %q: %v", bad, err)
			}
		}
		if l != nil {
			t.Errorf("Logger(%q) returned a non-nil logger with an error", bad)
		}
	}
	if buf.String() != "" {
		t.Errorf("rejecting components wrote output: %q", buf.String())
	}
}

// FR-7: Factory.Handler returns the underlying handler.
func TestFactoryHandlerNonNil(t *testing.T) {
	var buf syncBuffer
	fac := logging.NewFactory(checked(t, logging.NewHandler(&buf, logging.FormatJSON, slog.LevelInfo), false))
	if fac.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
}

// FR-27: BootstrapLogger writes JSON with component=config.
func TestBootstrapLogger(t *testing.T) {
	var buf syncBuffer
	l := logging.BootstrapLogger(&buf)
	l.Error("synthetic bootstrap failure", "setting", "log.level", "error", "config")
	recs := jsonLines(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	r := recs[0]
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := r[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	if r["component"] != "config" {
		t.Errorf("component = %v, want config", r["component"])
	}
	if r["setting"] != "log.level" || r["error"] != "config" {
		t.Errorf("attributes lost: %v", r)
	}
	for k := range r {
		if !logging.AllowedKey(k) {
			t.Errorf("bootstrap output key %q is not allowlisted (FR-39)", k)
		}
	}
}

// FR-27: the bootstrap logger suppresses debug output.
func TestBootstrapLoggerSuppressesDebug(t *testing.T) {
	var buf syncBuffer
	logging.BootstrapLogger(&buf).Debug("synthetic debug")
	if buf.String() != "" {
		t.Errorf("debug produced output: %q", buf.String())
	}
}
