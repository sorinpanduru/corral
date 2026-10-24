//go:build unix

package markerscan_test

// fd-level capture (SPEC-001 AC-33, orchestrator decision D1). Capture is by
// dup2 on fds 1 and 2, so it sees writes through loggers that grabbed
// os.Stderr before capture and through raw fd writes. Not parallel-safe: it
// swaps process-wide file descriptors, so these tests never call t.Parallel().

import (
	"bytes"
	"log"
	"os"
	"syscall"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

func TestCaptureStdioIsFDLevel(t *testing.T) {
	defer markerscan.SetMaxSourceBytesForTest(1 << 20)()
	m := newMarker(t)
	early := log.New(os.Stderr, "early ", 0) // grabbed os.Stderr BEFORE capture
	earlyOut := log.New(os.Stdout, "earlyout ", 0)

	stop := markerscan.CaptureStdio(t)
	log.Print("stdlogger " + m)           // std default logger (created at init)
	early.Print("earlylogger " + m)       // logger built before capture
	earlyOut.Print("earlyoutlogger " + m) // same, stdout
	if _, err := syscall.Write(2, []byte("rawfd2 "+m+"\n")); err != nil {
		t.Errorf("raw write to fd 2: %v", err)
	}
	if _, err := syscall.Write(1, []byte("rawfd1 "+m+"\n")); err != nil {
		t.Errorf("raw write to fd 1: %v", err)
	}
	out, errb := stop()

	for _, w := range []string{"stdlogger " + m, "earlylogger " + m, "rawfd2 " + m} {
		if !bytes.Contains(errb, []byte(w)) {
			t.Errorf("stderr capture missed %q (got %q)", w, errb)
		}
	}
	for _, w := range []string{"earlyoutlogger " + m, "rawfd1 " + m} {
		if !bytes.Contains(out, []byte(w)) {
			t.Errorf("stdout capture missed %q (got %q)", w, out)
		}
	}

	// After stop, output goes back to the original descriptors and not into
	// the returned (already final) slices.
	log.Print("after-stop")
	if bytes.Contains(errb, []byte("after-stop")) {
		t.Error("capture still active after stop")
	}
}
