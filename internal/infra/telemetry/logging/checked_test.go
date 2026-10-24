package logging_test

import (
	"log/slog"
	"runtime"
	"sync"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

type captureTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
}

func (c *captureTB) mark()                 { c.mu.Lock(); c.failed = true; c.mu.Unlock() }
func (c *captureTB) Error(...any)          { c.mark() }
func (c *captureTB) Errorf(string, ...any) { c.mark() }
func (c *captureTB) Fatal(...any)          { c.mark(); runtime.Goexit() }
func (c *captureTB) Fatalf(string, ...any) { c.mark(); runtime.Goexit() }
func (c *captureTB) Fail()                 { c.mark() }
func (c *captureTB) FailNow()              { c.mark(); runtime.Goexit() }
func (c *captureTB) Failed() bool          { c.mu.Lock(); defer c.mu.Unlock(); return c.failed }

// AC-5: logcheck sees records below the output level. A Debug record with a
// disallowed key fails the test even when the output handler is at Info, and
// is not written to the output.
func TestCheckerSeesRecordsBelowOutputLevel(t *testing.T) {
	var buf syncBuffer
	ctb := &captureTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fac := logging.NewFactory(checked(ctb, logging.NewHandler(&buf, logging.FormatJSON, slog.LevelInfo), false))
		l, err := fac.Logger(logging.Config)
		if err != nil {
			t.Error(err)
			return
		}
		l.Debug("synthetic hidden debug", "bad_key", "x")
	}()
	<-done
	if !ctb.Failed() {
		t.Error("a Debug record with a bad key did not reach logcheck while output was at Info")
	}
	if buf.String() != "" {
		t.Errorf("debug record reached the output at Info: %q", buf.String())
	}
}
