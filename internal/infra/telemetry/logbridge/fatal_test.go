package logbridge_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

const (
	childEnv   = "CORRAL_T8_FATAL_CHILD"
	markerEnv  = "CORRAL_T8_MARKER"
	variantEnv = "CORRAL_T8_VARIANT"
)

// TestGRPCFatalChild is the subprocess body; it is a no-op in the parent run.
func TestGRPCFatalChild(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		t.Skip("subprocess helper")
	}
	m := os.Getenv(markerEnv)
	f := logging.NewFactory(logging.NewHandler(os.Stdout, logging.FormatJSON, slog.LevelInfo))
	now := func() time.Time { return start } // frozen clock: the limiter never reopens
	g := logbridge.NewGRPCLogger(f, slog.LevelInfo, now)
	g.Error("consume the error slot " + m) // emitted; fatal must still be emitted after it
	switch os.Getenv(variantEnv) {
	case "Fatal":
		g.Fatal(m, m)
	case "Fatalln":
		g.Fatalln(m, m)
	default:
		g.Fatalf("%s %d", m, 1)
	}
	os.Stdout.WriteString("UNREACHABLE\n") // Fatal must not return
}

// FR-28: grpclog fatal writes an error record, flushes it to stdout, exits 1,
// is exempt from the rate limit, forwards no text, writes nothing to stderr.
func TestGRPCFatalExitsAfterFlushedErrorRecord(t *testing.T) {
	for _, variant := range []string{"Fatal", "Fatalln", "Fatalf"} {
		t.Run(variant, func(t *testing.T) {
			m := marker(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestGRPCFatalChild$")
			cmd.Env = append(os.Environ(), childEnv+"=1", markerEnv+"="+m, variantEnv+"="+variant)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			err := cmd.Run()
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 1 {
				t.Fatalf("exit = %v, want exit status 1", err)
			}
			if errb.Len() != 0 {
				t.Errorf("stderr not empty: %d bytes", errb.Len())
			}
			if strings.Contains(out.String(), m) {
				t.Errorf("marker forwarded to stdout: %s", out.String())
			}
			if strings.Contains(out.String(), "UNREACHABLE") {
				t.Error("Fatal returned")
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("stdout lines = %d, want 2 (limited error + exempt fatal): %q", len(lines), out.String())
			}
			for _, ln := range lines {
				var rec map[string]any
				if err := json.Unmarshal([]byte(ln), &rec); err != nil {
					t.Fatalf("line is not JSON: %q", ln)
				}
				if rec["level"] != "ERROR" || rec["component"] != "grpc" {
					t.Errorf("record = %v, want level ERROR component grpc", rec)
				}
			}
		})
	}
}
