//go:build corraltest

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
)

// probe collects the byte counts the bounds checks need (FR-64, ADR-0022):
// per client connection ID, the bytes read for the current message before a
// lease, the maximum of those, and the peak bytes held for one reply. Counts
// only: it never sees message bytes.
type probe struct {
	mu    sync.Mutex
	conns map[int32]*connStats
}

type connStats struct {
	Current int64 `json:"current_pre_lease_bytes"`
	Max     int64 `json:"max_pre_lease_bytes"`
	Peak    int64 `json:"peak_reply_bytes"`
}

func (p *probe) stats(id int32) *connStats {
	s := p.conns[id]
	if s == nil {
		s = &connStats{}
		p.conns[id] = s
	}
	return s
}

func (p *probe) ReadBeforeLease(id int32, n int) {
	p.mu.Lock()
	s := p.stats(id)
	s.Current = int64(n)
	if s.Current > s.Max {
		s.Max = s.Current
	}
	p.mu.Unlock()
}

func (p *probe) ReplyHeld(id int32, n int) {
	p.mu.Lock()
	if s := p.stats(id); int64(n) > s.Peak {
		s.Peak = int64(n)
	}
	p.mu.Unlock()
}

var _ exchange.Probe = (*probe)(nil)

// startProbe serves the loopback-only probe endpoints and writes the address
// to <dir>/probe.addr. An address that is not loopback is refused.
func startProbe(addr, dir string) exchange.Probe {
	p := &probe{conns: map[int32]*connStats{}}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/conns", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		out := make(map[string]connStats, len(p.conns))
		for id, s := range p.conns {
			out[strconv.Itoa(int(id))] = *s
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/constants", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"L": proxy.RequestBufferLimit, "K": exchange.ReplyChunk})
	})
	go func() { _ = http.Serve(ln, mux) }()
	if dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "probe.addr"), []byte(ln.Addr().String()), 0o600)
	}
	return p
}
