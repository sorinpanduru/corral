package earlyslog

import (
	"context"
	"log"
	"log/slog"
	"testing"
)

func TestInitInstalledCountingDefault(t *testing.T) {
	Drain()
	slog.Warn("secret-value-1", "k", "secret-value-2")
	slog.Error("x")
	slog.Info("y")
	slog.Debug("z")
	got := Drain()
	if got != (Counts{Debug: 1, Info: 1, Warn: 1, Error: 1}) {
		t.Fatalf("counts = %+v", got)
	}
	if again := Drain(); again != (Counts{}) {
		t.Fatalf("Drain did not reset: %+v", again)
	}
}

func TestDefaultsAreEarlyHandlers(t *testing.T) {
	if _, ok := slog.Default().Handler().(handler); !ok {
		t.Fatalf("slog default handler is %T, want earlyslog handler", slog.Default().Handler())
	}
	if _, ok := log.Writer().(stdWriter); !ok {
		t.Fatalf("log writer is %T, want earlyslog writer", log.Writer())
	}
}

func TestStdLogCountedSeparately(t *testing.T) {
	Drain()
	log.Print("secret")
	if got := Drain(); got != (Counts{StdLog: 1}) {
		t.Fatalf("counts = %+v", got)
	}
}

type rec struct{ n int }

func (r *rec) Enabled(context.Context, slog.Level) bool  { return true }
func (r *rec) Handle(context.Context, slog.Record) error { r.n++; return nil }
func (r *rec) WithAttrs([]slog.Attr) slog.Handler        { return r }
func (r *rec) WithGroup(string) slog.Handler             { return r }

func TestHandoffForwardsCapturedLogger(t *testing.T) {
	Drain()
	captured := slog.Default()
	captured.Warn("before")
	r := &rec{}
	if got := Handoff(r); got.Warn != 1 {
		t.Fatalf("counts = %+v", got)
	}
	captured.Warn("after")
	if r.n != 1 {
		t.Fatalf("forwarded = %d, want 1", r.n)
	}
	if got := Drain(); got != (Counts{}) {
		t.Fatalf("counts = %+v", got)
	}
}
