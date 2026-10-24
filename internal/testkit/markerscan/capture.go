package markerscan

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// boundedBuffer keeps the head of a stream up to the cap and discards the
// rest. Write always reports len(p) so writers keep draining and never block.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := maxBytes() - b.buf.Len()
	if room < 0 {
		room = 0
	}
	if len(p) > room {
		b.truncated = true
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *boundedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...), b.truncated
}

// CaptureStdio redirects the process stdout and stderr into memory. On unix
// it works at the file-descriptor level: fds 1 and 2 are saved, the pipe write
// ends are dup2'd over them, and os.Stdout/os.Stderr point at the pipes, so
// loggers that grabbed os.Stderr earlier and raw fd writes are captured too.
// On other platforms only os.Stdout/os.Stderr are swapped.
//
// It is NOT parallel-safe: it swaps process-wide state, so tests using it
// must not call t.Parallel(). The stop func restores everything, is
// idempotent, and is also registered with tb.Cleanup. Each stream keeps at
// most the per-source cap; if exceeded, stop reports it through tb, and the
// writers are never blocked because the pipes are always drained.
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
	restoreFDs, err := redirectFDs(outW, errW)
	if err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		tb.Fatalf("markerscan: redirect fds: %v", err)
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
			restoreFDs()
			outW.Close()
			errW.Close()
			wg.Wait()
			outR.Close()
			errR.Close()
			var t1, t2 bool
			so, t1 = outB.snapshot()
			se, t2 = errB.snapshot()
			if t1 || t2 {
				tb.Errorf("markerscan: captured stdio exceeded the %d byte cap and was truncated", maxBytes())
			}
		})
		return so, se
	}
	tb.Cleanup(func() { stop() })
	return stop
}

// RunCapture runs cmd (a child process) and returns its stdout and stderr,
// each limited to the per-source cap (excess is drained and discarded, and
// truncated is true). cmd.Stdout and cmd.Stderr must be unset. A start
// failure fails tb; a non-zero exit does not.
func RunCapture(tb testing.TB, cmd *exec.Cmd) (stdout, stderr []byte, truncated bool) {
	tb.Helper()
	if cmd.Stdout != nil || cmd.Stderr != nil {
		tb.Fatalf("markerscan: cmd.Stdout/Stderr must be nil")
	}
	var o, e boundedBuffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Start(); err != nil {
		tb.Fatalf("markerscan: start child: %v", err)
	}
	_ = cmd.Wait() // exit status is the caller's concern
	so, t1 := o.snapshot()
	se, t2 := e.snapshot()
	return so, se, t1 || t2
}

// FetchBody GETs url (for /metrics or /readyz) and returns the body, bounded
// by the per-source cap, regardless of the status code. Over-cap bodies fail tb.
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
	limit := maxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		tb.Fatalf("markerscan: read body: %v", err)
	}
	if len(body) > limit {
		tb.Errorf("markerscan: body exceeded the %d byte cap", limit)
		body = body[:limit]
	}
	return body
}
