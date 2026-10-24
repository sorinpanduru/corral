package corralprobe_test

// Self-test for corralprobe, the client of the corraltest probe server
// (ADR-0022). The JSON field names for /conns are fixed here (the plan leaves
// them to the corraltest hook implementation, which must match this test):
//
//	{"<connection id>": {"current_pre_lease_bytes": N,
//	                      "max_pre_lease_bytes": N,
//	                      "peak_reply_bytes": N}}
//
// /constants is {"L": 65536, "K": 65536} (SPEC-002 Amendment 1, FR-64).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/corralprobe"
)

func probeServer(t *testing.T, h http.Handler) (holdDir string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := os.WriteFile(filepath.Join(dir, "probe.addr"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConstants_FR64(t *testing.T) {
	dir := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/constants" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"L": 65536, "K": 65536}`))
	}))
	c := corralprobe.Open(t, dir)
	k, err := c.Constants()
	if err != nil {
		t.Fatal(err)
	}
	if k.L != 65536 || k.K != 65536 {
		t.Fatalf("constants = %+v", k)
	}
}

func TestConns_FR64(t *testing.T) {
	dir := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conns" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"c-1":{"current_pre_lease_bytes":10,"max_pre_lease_bytes":65536,"peak_reply_bytes":4096},
		                 "c-2":{"current_pre_lease_bytes":0,"max_pre_lease_bytes":120,"peak_reply_bytes":0}}`))
	}))
	c := corralprobe.Open(t, dir)
	m, err := c.Conns()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("conns = %v", m)
	}
	if s := m["c-1"]; s.CurrentPreLeaseBytes != 10 || s.MaxPreLeaseBytes != 65536 || s.PeakReplyBytes != 4096 {
		t.Fatalf("c-1 = %+v", s)
	}
	if s := m["c-2"]; s.MaxPreLeaseBytes != 120 {
		t.Fatalf("c-2 = %+v", s)
	}
}

// An empty connection table is an empty map, not an error.
func TestConnsEmpty(t *testing.T) {
	dir := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	m, err := corralprobe.Open(t, dir).Conns()
	if err != nil || len(m) != 0 {
		t.Fatalf("conns = %v err=%v", m, err)
	}
}

// Non-200 and malformed bodies are errors.
func TestErrors(t *testing.T) {
	dir := probeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conns" {
			http.Error(w, "boom", 500)
			return
		}
		w.Write([]byte("not json"))
	}))
	c := corralprobe.Open(t, dir)
	if _, err := c.Conns(); err == nil {
		t.Fatal("500 not reported")
	}
	if _, err := c.Constants(); err == nil {
		t.Fatal("malformed JSON not reported")
	}
}

// Open waits for probe.addr to appear (the process writes it after start).
func TestOpenWaitsForAddrFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"L":1,"K":2}`)) }))
	defer srv.Close()
	dir := t.TempDir()
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "probe.addr"), []byte(strings.TrimPrefix(srv.URL, "http://")), 0o600)
	}()
	k, err := corralprobe.Open(t, dir).Constants()
	if err != nil || k.L != 1 || k.K != 2 {
		t.Fatalf("constants = %+v err=%v", k, err)
	}
}

// The probe is loopback only: Open refuses a non-loopback address in
// probe.addr (a misconfigured build must not be probed over the network).
func TestOpenRefusesNonLoopback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "probe.addr"), []byte("192.0.2.1:9999\n"), 0o600)
	if _, err := corralprobe.TryOpen(dir, time.Second); err == nil {
		t.Fatal("non-loopback probe address accepted")
	}
	if _, err := corralprobe.TryOpen(t.TempDir(), 100*time.Millisecond); err == nil {
		t.Fatal("missing probe.addr did not time out")
	}
}
