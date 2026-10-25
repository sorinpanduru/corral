// Package corralproc starts the corral binary (or the corraltest build) for
// tests: it captures stdout JSON records and stderr, waits for records, sends
// signals, reads the admin endpoints and drives CORRALTEST_HOLD_DIR hold files
// (ADR-0022). Test infrastructure only.
package corralproc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Opts configures Start.
type Opts struct {
	Binary    string   // path to the binary
	Args      []string // command-line arguments
	Env       []string // extra KEY=VALUE entries added to the environment
	AdminAddr string   // host:port of the admin listener, for Readyz and Metrics
}

// Record is one JSON log record from stdout.
type Record map[string]any

// Msg returns the msg field.
func (r Record) Msg() string { s, _ := r["msg"].(string); return s }

// Component returns the component field.
func (r Record) Component() string { s, _ := r["component"].(string); return s }

// capture splits a stream into lines and keeps the raw bytes too.
type capture struct {
	mu   sync.Mutex
	raw  bytes.Buffer
	line []byte
	recs []Record
	json bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw.Write(p)
	if !c.json {
		return len(p), nil
	}
	for _, b := range p {
		if b != '\n' {
			c.line = append(c.line, b)
			continue
		}
		if len(c.line) > 0 && c.line[0] == '{' {
			var r Record
			if json.Unmarshal(c.line, &r) == nil {
				c.recs = append(c.recs, r)
			}
		}
		c.line = c.line[:0]
	}
	return len(p), nil
}

// Proc is a running process.
type Proc struct {
	tb      testing.TB
	cmd     *exec.Cmd
	out     *capture
	errc    *capture
	holdDir string
	admin   string
	done    chan struct{}
	code    int
	http    *http.Client
}

// Start launches the process. The test's cleanup kills it if it is still
// running. CORRALTEST_HOLD_DIR is set to a private temporary directory.
func Start(tb testing.TB, o Opts) *Proc {
	tb.Helper()
	p := &Proc{tb: tb, out: &capture{json: true}, errc: &capture{}, admin: o.AdminAddr,
		holdDir: tb.TempDir(), done: make(chan struct{}), http: &http.Client{Timeout: 5 * time.Second}}
	if err := os.Chmod(p.holdDir, 0o700); err != nil {
		tb.Fatal(err)
	}
	p.cmd = exec.Command(o.Binary, o.Args...)
	p.cmd.Env = append(append(cleanEnv(), o.Env...), "CORRALTEST_HOLD_DIR="+p.holdDir)
	p.cmd.Stdout = p.out
	p.cmd.Stderr = p.errc
	if err := p.cmd.Start(); err != nil {
		tb.Fatalf("corralproc: start failed: %v", err)
	}
	go func() {
		_ = p.cmd.Wait()
		p.code = p.cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	tb.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// Wait waits for exit and returns the exit code (-1 if killed by a signal).
func (p *Proc) Wait(timeout time.Duration) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-time.After(timeout):
		return 0, errors.New("corralproc: process still running after timeout")
	}
}

// Records returns the JSON records captured so far.
func (p *Proc) Records() []Record {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return append([]Record(nil), p.out.recs...)
}

// Stdout returns all stdout bytes captured so far.
func (p *Proc) Stdout() []byte {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return append([]byte(nil), p.out.raw.Bytes()...)
}

// Stderr returns all stderr bytes captured so far.
func (p *Proc) Stderr() []byte {
	p.errc.mu.Lock()
	defer p.errc.mu.Unlock()
	return append([]byte(nil), p.errc.raw.Bytes()...)
}

// WaitRecord waits for a record with the given msg (and component, unless
// empty). Records that arrived earlier match too.
func (p *Proc) WaitRecord(msg, component string, timeout time.Duration) (Record, error) {
	deadline := time.Now().Add(timeout)
	for {
		for _, r := range p.Records() {
			if r.Msg() == msg && (component == "" || r.Component() == component) {
				return r, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("corralproc: record did not appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Signal sends sig to the process.
func (p *Proc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// SignalRepeatedly sends sig n times, interval apart.
func (p *Proc) SignalRepeatedly(sig syscall.Signal, n int, interval time.Duration) error {
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(interval)
		}
		if err := p.Signal(sig); err != nil {
			return err
		}
	}
	return nil
}

func (p *Proc) get(path string) (int, string, error) {
	if p.admin == "" {
		return 0, "", errors.New("corralproc: AdminAddr not set")
	}
	resp, err := p.http.Get("http://" + p.admin + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// Readyz returns the /readyz status code and body.
func (p *Proc) Readyz() (int, string, error) { return p.get("/readyz") }

// Metrics returns the /metrics text.
func (p *Proc) Metrics() (string, error) {
	code, body, err := p.get("/metrics")
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("corralproc: /metrics returned status %d", code)
	}
	return body, err
}

// HoldDir returns the CORRALTEST_HOLD_DIR of the process.
func (p *Proc) HoldDir() string { return p.holdDir }

// cleanEnv returns the inherited environment without CORRAL_* and CORRALTEST_*
// variables, so a developer's shell cannot change the process under test.
// Variables the test wants are passed explicitly in Opts.Env.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CORRAL_") || strings.HasPrefix(kv, "CORRALTEST_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// The SPEC-003 hold points keep the plain held/release file names (ADR-0014).
// cmd/corral has no exported constant for them yet, so the names are listed
// here; keep in step with cmd/corral/testbuild_on.go.
func holdNames(point string) (held, release string) {
	if point == "logger" || point == "checkconfig" {
		return "held", "release"
	}
	return "held-" + point, "release-" + point
}

// Held reports whether the process is held at point.
func (p *Proc) Held(point string) bool {
	held, _ := holdNames(point)
	_, err := os.Stat(filepath.Join(p.holdDir, held))
	return err == nil
}

// WaitHeld waits until the process is held at point.
func (p *Proc) WaitHeld(point string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !p.Held(point) {
		if time.Now().After(deadline) {
			return errors.New("corralproc: hold point not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// Release lets the process continue past point.
func (p *Proc) Release(point string) error {
	_, rel := holdNames(point)
	return os.WriteFile(filepath.Join(p.holdDir, rel), nil, 0o600)
}
