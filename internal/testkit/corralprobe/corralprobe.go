// Package corralprobe is the client of the corraltest probe server (ADR-0022).
// The probe address is read from probe.addr in the hold directory and must be
// loopback. Test infrastructure only.
package corralprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Constants are the buffer constants reported by the build (FR-64).
type Constants struct {
	L int64 `json:"L"`
	K int64 `json:"K"`
}

// ConnStats are per-connection byte counters.
type ConnStats struct {
	CurrentPreLeaseBytes int64 `json:"current_pre_lease_bytes"`
	MaxPreLeaseBytes     int64 `json:"max_pre_lease_bytes"`
	PeakReplyBytes       int64 `json:"peak_reply_bytes"`
}

// Client talks to the probe server.
type Client struct {
	base string
	http *http.Client
}

// TryOpen waits up to timeout for probe.addr in dir and returns a client.
func TryOpen(dir string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "probe.addr"))
		if err == nil && strings.TrimSpace(string(b)) != "" {
			addr := strings.TrimSpace(string(b))
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, errors.New("corralprobe: malformed probe address")
			}
			ip := net.ParseIP(host)
			if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return nil, errors.New("corralprobe: probe address is not loopback")
			}
			return &Client{base: "http://" + addr, http: &http.Client{Timeout: 5 * time.Second}}, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("corralprobe: probe.addr did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Open is TryOpen with a 10 s wait that fails the test.
func Open(tb testing.TB, dir string) *Client {
	tb.Helper()
	c, err := TryOpen(dir, 10*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

func (c *Client) get(path string, out any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("corralprobe: %s returned status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Constants returns /constants.
func (c *Client) Constants() (Constants, error) {
	var k Constants
	err := c.get("/constants", &k)
	return k, err
}

// Conns returns /conns keyed by connection id.
func (c *Client) Conns() (map[string]ConnStats, error) {
	m := map[string]ConnStats{}
	err := c.get("/conns", &m)
	return m, err
}
