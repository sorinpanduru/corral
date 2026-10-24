package markerscan

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// maxCapture bounds each captured stream; excess output is drained and
// discarded and the test is failed, since the scan would be incomplete.
const maxCapture = MaxSourceBytes

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := maxCapture - b.buf.Len()
	if len(p) > room {
		b.truncated = true
		p = p[:room]
		b.buf.Write(p)
		return len(p), nil // report full length so writers keep draining
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...), b.truncated
}

// CaptureStdio replaces os.Stdout and os.Stderr with pipes drained in the
// background. The returned stop restores them and returns what was written.
// stop is idempotent and is also registered with tb.Cleanup. Loggers that
// captured the original *os.File before the call are not redirected.
func CaptureStdio(tb testing.TB) (stop func() (stdout, stderr []byte)) {
	tb.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		tb.Fatalf("markerscan: pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		tb.Fatalf("markerscan: pipe: %v", err)
	}
	var outB, errB boundedBuffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outB, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errB, errR) }()
	os.Stdout, os.Stderr = outW, errW

	var once sync.Once
	var so, se []byte
	stop = func() ([]byte, []byte) {
		once.Do(func() {
			os.Stdout, os.Stderr = origOut, origErr
			outW.Close()
			errW.Close()
			wg.Wait()
			outR.Close()
			errR.Close()
			var t1, t2 bool
			so, t1 = outB.snapshot()
			se, t2 = errB.snapshot()
			if t1 || t2 {
				tb.Errorf("markerscan: captured output exceeded %d bytes and was truncated", maxCapture)
			}
		})
		return so, se
	}
	tb.Cleanup(func() { stop() })
	return stop
}

// RunCapture runs cmd (a child process) and returns its stdout and stderr,
// each bounded. cmd.Stdout and cmd.Stderr must be unset.
func RunCapture(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, nil, errors.New("markerscan: cmd.Stdout/Stderr must be nil")
	}
	var o, e boundedBuffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	so, t1 := o.snapshot()
	se, t2 := e.snapshot()
	if (t1 || t2) && err == nil {
		err = errors.New("markerscan: child output truncated")
	}
	return so, se, err
}

// FetchBody GETs url (for /metrics or /readyz) and returns the body, bounded,
// regardless of the status code.
func FetchBody(tb testing.TB, url string) []byte {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		tb.Fatalf("markerscan: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("markerscan: fetch: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCapture+1))
	if err != nil {
		tb.Fatalf("markerscan: read body: %v", err)
	}
	if len(body) > maxCapture {
		tb.Errorf("markerscan: body exceeded %d bytes", maxCapture)
		body = body[:maxCapture]
	}
	return body
}
