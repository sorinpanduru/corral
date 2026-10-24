package logging_test

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

func newLogger(t *testing.T, f logging.Format, lvl slog.Leveler, c logging.Component) (*slog.Logger, *syncBuffer) {
	t.Helper()
	var buf syncBuffer
	fac := logging.NewFactory(logging.NewHandler(&buf, f, lvl))
	l, err := fac.Logger(c)
	if err != nil {
		t.Fatalf("Logger(%q): %v", c, err)
	}
	return l, &buf
}

// AC-3 (unit), FR-5: with json, every line is valid JSON with time, level,
// msg and component.
func TestJSONLinesHaveRequiredFields(t *testing.T) {
	l, buf := newLogger(t, logging.FormatJSON, slog.LevelDebug, logging.Lifecycle)
	l.Debug("synthetic debug")
	l.Info("synthetic info", "setting", "log.level")
	l.Warn("synthetic warn", "retry_count", 3)
	l.Error("synthetic error", "error", "timeout")
	recs := jsonLines(t, buf)
	if len(recs) != 4 {
		t.Fatalf("got %d lines, want 4: %q", len(recs), buf.String())
	}
	for _, r := range recs {
		for _, k := range []string{"time", "level", "msg", "component"} {
			if _, ok := r[k]; !ok {
				t.Errorf("record %v lacks %q", r, k)
			}
		}
		if r["component"] != "lifecycle" {
			t.Errorf("component = %v, want lifecycle", r["component"])
		}
	}
	if recs[2]["retry_count"] != float64(3) {
		t.Errorf("retry_count = %v, want number 3", recs[2]["retry_count"])
	}
}

// AC-3 (unit), FR-5: with text, lines are slog text format (key=value).
func TestTextLinesAreSlogText(t *testing.T) {
	l, buf := newLogger(t, logging.FormatText, slog.LevelInfo, logging.Config)
	l.Info("synthetic message")
	l.Warn("another message", "setting", "log.level")
	lines := buf.Lines()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	re := regexp.MustCompile(`^time=\S+ level=(INFO|WARN) msg=("[^"]*"|\S+) .*component=config`)
	for _, ln := range lines {
		if strings.HasPrefix(ln, "{") {
			t.Errorf("text line looks like JSON: %q", ln)
		}
		if !re.MatchString(ln) {
			t.Errorf("line is not slog text format with component: %q", ln)
		}
	}
	if !strings.Contains(lines[1], "setting=log.level") {
		t.Errorf("missing setting attribute: %q", lines[1])
	}
}

// AC-4, FR-6: at each level, records below are not emitted and records at or
// above are.
func TestLevelFiltering(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  []string
	}{
		{slog.LevelDebug, []string{"debug", "info", "warn", "error"}},
		{slog.LevelInfo, []string{"info", "warn", "error"}},
		{slog.LevelWarn, []string{"warn", "error"}},
		{slog.LevelError, []string{"error"}},
	}
	for _, c := range cases {
		t.Run(c.level.String(), func(t *testing.T) {
			l, buf := newLogger(t, logging.FormatJSON, c.level, logging.Config)
			l.Debug("msg debug")
			l.Info("msg info")
			l.Warn("msg warn")
			l.Error("msg error")
			var got []string
			for _, r := range jsonLines(t, buf) {
				got = append(got, strings.ToLower(r["level"].(string)))
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("emitted %v, want %v", got, c.want)
			}
		})
	}
}

// AC-4: the default level (zero-value LevelVar) is info.
func TestDefaultLevelIsInfo(t *testing.T) {
	var lv slog.LevelVar
	l, buf := newLogger(t, logging.FormatJSON, &lv, logging.Config)
	l.Debug("hidden")
	l.Info("shown")
	recs := jsonLines(t, buf)
	if len(recs) != 1 || recs[0]["msg"] != "shown" {
		t.Errorf("got %v, want only the info record", recs)
	}
}

// AC-4: Enabled agrees with the configured level.
func TestHandlerEnabled(t *testing.T) {
	var buf syncBuffer
	h := logging.NewHandler(&buf, logging.FormatJSON, slog.LevelWarn)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info enabled at warn")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("warn/error not enabled at warn")
	}
}

type blockingWriter struct {
	release chan struct{}
	written atomic.Int32
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.written.Add(1)
	return len(p), nil
}

// FR-38: records are written synchronously; a blocked writer blocks the
// logging call.
func TestWritesAreSynchronousAndBlocking(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	fac := logging.NewFactory(logging.NewHandler(w, logging.FormatJSON, slog.LevelInfo))
	l, err := fac.Logger(logging.Lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		l.Info("synthetic blocked")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("logging call returned while the writer was blocked")
	case <-time.After(150 * time.Millisecond):
	}
	close(w.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging call did not return after the writer was released")
	}
	if w.written.Load() != 1 {
		t.Errorf("writes = %d, want 1 by the time Info returned", w.written.Load())
	}
}

// FR-38: the line is in the sink as soon as the call returns (no buffering).
func TestNoBufferingAfterReturn(t *testing.T) {
	l, buf := newLogger(t, logging.FormatJSON, slog.LevelInfo, logging.Lifecycle)
	l.Info("synthetic one")
	if n := len(buf.Lines()); n != 1 {
		t.Fatalf("lines after return = %d, want 1", n)
	}
	l.Info("synthetic two")
	if n := len(buf.Lines()); n != 2 {
		t.Fatalf("lines after return = %d, want 2", n)
	}
}
