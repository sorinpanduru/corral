//go:build functional

package functional

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	plainUTF8 = "text/plain; charset=utf-8"
	waitFor   = 20 * time.Second
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (s *safeBuf) Lines() []string {
	var out []string
	for _, l := range strings.Split(string(s.Bytes()), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// proc is a running corral binary with stdout and stderr captured separately.
type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout safeBuf
	stderr safeBuf
	done   chan struct{}
	exit   int
}

// baseEnv is a minimal environment: no inherited OTEL_* or other variables.
func baseEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}, extra...)
}

func startProc(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, done: make(chan struct{})}
	p.cmd = exec.Command(binPath, args...)
	p.cmd.Env = env
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start corral: %v", err)
	}
	go func() {
		err := p.cmd.Wait()
		p.exit = p.cmd.ProcessState.ExitCode()
		_ = err
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// waitExit waits for the process to exit by itself and returns its exit code
// (-1 if it was killed by a signal).
func (p *proc) waitExit() int {
	p.t.Helper()
	select {
	case <-p.done:
		return p.exit
	case <-time.After(waitFor):
		p.t.Fatalf("corral did not exit in time")
	}
	return -1
}

// sigterm requests graceful shutdown and returns the exit code.
func (p *proc) sigterm() int {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.t.Fatalf("SIGTERM: %v", err)
	}
	return p.waitExit()
}

// waitReady polls /readyz on addr until it returns 200.
func (p *proc) waitReady(addr string) {
	p.t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			p.t.Fatalf("corral exited (%d) before becoming ready; stdout lines: %d", p.exit, len(p.stdout.Lines()))
		default:
		}
		if r, err := httpDo("GET", addr, "/readyz"); err == nil && r.status == 200 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.t.Fatalf("corral not ready in time")
}

func (p *proc) records() []map[string]any {
	p.t.Helper()
	var out []map[string]any
	for _, l := range p.stdout.Lines() {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			p.t.Errorf("stdout line is not a JSON object (len %d)", len(l))
			continue
		}
		out = append(out, m)
	}
	return out
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

type response struct {
	status int
	body   string
	ctype  string
	allow  string
}

var client = &http.Client{
	Timeout:       5 * time.Second,
	Transport:     &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func httpDo(method, addr, path string) (response, error) {
	req, err := http.NewRequest(method, "http://"+addr+path, nil)
	if err != nil {
		return response{}, err
	}
	r, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	return response{r.StatusCode, string(b), r.Header.Get("Content-Type"), r.Header.Get("Allow")}, nil
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	a := ln.Addr().String()
	_ = ln.Close()
	return a
}

// nonLoopbackIPv4 returns a local non-loopback IPv4 address, or "".
func nonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String()
			}
		}
	}
	return ""
}

func dials(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func lifecycleMsgs(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		if str(r, "component") == "lifecycle" {
			out = append(out, str(r, "msg"))
		}
	}
	return out
}
