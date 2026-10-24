package earlyslog

import (
	"bytes"
	"log/slog"
	"os"
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

func TestNothingWrittenToStderr(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	slog.Warn("T-MARKER")
	os.Stderr = old
	w.Close()
	var b bytes.Buffer
	b.ReadFrom(r)
	if b.Len() != 0 {
		t.Fatalf("stderr got %d bytes", b.Len())
	}
	Drain()
}
