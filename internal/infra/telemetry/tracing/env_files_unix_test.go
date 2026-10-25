//go:build unix

package tracing_test

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

var envFileVars = []string{
	"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_KEY", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
}

// envResolveWithin runs Resolve and fails the test if it does not return
// within the deadline (a blocking open must not hang startup, FR-27).
func envResolveWithin(tb testing.TB, m map[string]string) error {
	tb.Helper()
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		_, _, err := envResolve(envOn(1.0), envMap(m))
		ch <- res{err}
	}()
	select {
	case r := <-ch:
		return r.err
	case <-time.After(2 * time.Second):
		tb.Fatalf("Resolve did not return within 2s (blocked reading %v)", envKeys(m))
		return nil
	}
}

// mkfifo creates a FIFO and, on cleanup, releases any reader blocked on it so
// a failing (hanging) implementation does not leak a stuck goroutine.
func mkfifo(tb testing.TB, path string) {
	tb.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		tb.Skipf("mkfifo unsupported: %v", err)
	}
	tb.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

// FR-27, FR-20: a certificate or key path that is a FIFO, a symlink to a FIFO,
// /dev/zero or a directory is rejected promptly as a config error naming the
// variable, with no echo of the path (the marker is part of the path).
func TestResolve_NonRegularFilesRejectedPromptly(t *testing.T) {
	certs := envGenCerts(t)
	for _, v := range envFileVars {
		t.Run(v, func(t *testing.T) {
			marker := envMarker(t)
			dir := filepath.Join(t.TempDir(), marker)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			fifo := filepath.Join(dir, "pipe")
			mkfifo(t, fifo)
			link := filepath.Join(dir, "link-to-pipe")
			if err := os.Symlink(fifo, link); err != nil {
				t.Fatal(err)
			}
			for name, path := range map[string]string{
				"fifo":            fifo,
				"symlink to fifo": link,
				"/dev/zero":       "/dev/zero",
				"directory":       dir,
			} {
				t.Run(name, func(t *testing.T) {
					err := envResolveWithin(t, envMerge(map[string]string{v: path}, envPairExtra(v, certs)))
					envRequireRejected(t, err, v, marker)
				})
			}
		})
	}
}

// FR-27: a regular file above the 4 MiB cap is rejected even if it starts
// with a valid PEM block; contents and path are not echoed.
func TestResolve_OversizedFileRejected(t *testing.T) {
	certs := envGenCerts(t)
	valid, err := os.ReadFile(certs.Cert)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range envFileVars {
		t.Run(v, func(t *testing.T) {
			marker := envMarker(t)
			p := filepath.Join(t.TempDir(), marker+".big")
			body := append(append([]byte{}, valid...), bytes.Repeat([]byte(marker+"\n"), (4<<20)/len(marker)+2)...)
			if len(body) <= 4<<20 {
				t.Fatalf("fixture too small: %d", len(body))
			}
			envWrite(t, p, body)
			err := envResolveWithin(t, envMerge(map[string]string{v: p}, envPairExtra(v, certs)))
			envRequireRejected(t, err, v, marker)
		})
	}
}
