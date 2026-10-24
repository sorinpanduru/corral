//go:build linux

package logbridge_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"
)

// captureStderr redirects fd 2 into a pipe (so writes by any package are seen,
// not only through os.Stderr) and returns a stop function giving the bytes.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(int(w.Fd()), 2, 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(&buf, r) }()
	stopped := false
	stop := func() string {
		if stopped {
			return buf.String()
		}
		stopped = true
		syscall.Dup3(saved, 2, 0)
		syscall.Close(saved)
		w.Close()
		wg.Wait()
		r.Close()
		return buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

// AC-26 (FR-28): every source produces slog records and nothing at all is
// written to stderr, with marker-seeded third-party text.
func TestNothingWrittenToStderr(t *testing.T) {
	restoreGlobals(t)
	stop := captureStderr(t)
	e := newEnv(t, slog.LevelDebug)
	m := marker(t)

	// individual constructors
	logbridge.NewOTelErrorHandler(e.f, e.clk.Now).Handle(markerErr(m))
	logr.New(logbridge.NewOTelLogSink(e.f, e.clk.Now)).Error(errors.New(m), m, m, m)
	g := logbridge.NewGRPCLogger(e.f, e.lv, e.clk.Now)
	g.Warning(m)
	g.Info(m)
	g.Error(m)
	logbridge.NewHTTPErrorLog(e.f, e.clk.Now).Print(m)
	log.New(logbridge.NewStdLogWriter(e.f, e.lv, e.clk.Now), m, 0).Print(m)
	//constmsg:allow simulated third-party caller
	slog.New(logbridge.NewSlogDefaultHandler(e.f, e.lv, e.clk.Now)).Warn(m, m, m)

	// through the process globals
	logbridge.Install(e.f, e.lv, e.clk.Now)
	otel.Handle(markerErr(m))
	grpclog.Warning(m)
	grpclog.Error(m)
	//constmsg:allow simulated third-party caller
	slog.Info(m)
	log.Print(m)
	log.Printf("%s %s", m, m)

	if got := stop(); got != "" {
		t.Errorf("stderr is not empty (%d bytes); marker present: %v", len(got), strings.Contains(got, m))
	}
	if e.count() < 10 {
		t.Errorf("only %d records, sources are not routed", e.count())
	}
	e.noMarker(m)
}
