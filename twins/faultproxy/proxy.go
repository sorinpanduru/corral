// Package faultproxy is a TCP intermediary placed in front of a real mongod
// (or any MongoDB-protocol upstream) for tests. It relays frames unchanged,
// records every command per upstream connection with its own OP_MSG parser,
// injects scripted faults, and can rewrite member addresses in hello replies
// so that a fronted replica set is discovered through the proxy.
//
// It uses the official Go driver's bson package and never corral's own code.
// See README.md for the contract and known differences.
package faultproxy

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Options configures a Proxy.
type Options struct {
	// RewriteHello maps real member addresses to the addresses to advertise
	// in hello / isMaster replies (fields hosts, passives, arbiters, primary,
	// me). An empty value means the proxy's own address.
	RewriteHello map[string]string
	// RewriteHelloSelf adds upstream -> proxy address to RewriteHello.
	RewriteHelloSelf bool
	// DropCompression removes the "compression" field from hello replies so
	// that no client negotiates OP_COMPRESSED (which the proxy cannot parse).
	DropCompression bool
}

// Direction of a recorded frame.
type Direction int

const (
	// ToUpstream is a request travelling client to upstream.
	ToUpstream Direction = iota
	// ToClient is a reply travelling upstream to client.
	ToClient
)

// Record is one frame the proxy parsed.
type Record struct {
	Seq       int
	ConnIndex int
	AppName   string // handshake appName of the connection, as known then
	Direction Direction
	// Command and DB are those of the request (for a reply, the request it answers).
	Command      string
	DB           string
	LsidID       []byte // lsid.id bytes (requests only; test-side data)
	HasTxnNumber bool
	TxnNumber    int64
	StartTxn     bool
	RequestID    int32
	ResponseTo   int32
	Flags        uint32
	OK           bool // replies: ok field is 1
	Time         time.Time
}

// ConnInfo describes one proxied connection.
type ConnInfo struct {
	Index      int
	Client     string // the client's address as seen by the proxy
	AppName    string
	Open       bool
	Blackholed bool
}

// Proxy is a running fault-injecting intermediary.
type Proxy struct {
	tb       testing.TB
	upstream string
	ln       net.Listener
	addr     string
	opts     Options

	mu       sync.Mutex
	rewrite  map[string]string
	conns    []*pconn
	recs     []Record
	faults   []*FaultHandle
	paused   bool
	pauseCh  chan struct{} // closed when resumed
	blackNew bool
	closed   bool
	wg       sync.WaitGroup
}

// Start listens on 127.0.0.1:0 and relays to upstream (host:port). It stops
// through tb.Cleanup.
func Start(tb testing.TB, upstream string, opts Options) *Proxy {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("faultproxy: listen: %v", err)
	}
	p := &Proxy{tb: tb, upstream: upstream, ln: ln, addr: ln.Addr().String(), opts: opts}
	p.rewrite = map[string]string{}
	for k, v := range opts.RewriteHello {
		p.rewrite[k] = v
	}
	if opts.RewriteHelloSelf {
		p.rewrite[upstream] = ""
	}
	p.wg.Add(1)
	go p.acceptLoop()
	tb.Cleanup(p.Stop)
	return p
}

// Addr is the proxy's listen address.
func (p *Proxy) Addr() string { return p.addr }

// Upstream is the address being fronted.
func (p *Proxy) Upstream() string { return p.upstream }

// SetRewrite replaces the hello rewrite map (empty value = proxy address).
func (p *Proxy) SetRewrite(m map[string]string) {
	p.mu.Lock()
	p.rewrite = map[string]string{}
	for k, v := range m {
		p.rewrite[k] = v
	}
	p.mu.Unlock()
}

// Stop closes the listener and every connection. Idempotent.
func (p *Proxy) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.wg.Wait()
		return
	}
	p.closed = true
	conns := append([]*pconn(nil), p.conns...)
	if p.paused {
		p.paused = false
		close(p.pauseCh)
	}
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, c := range conns {
		c.close(false)
	}
	p.wg.Wait()
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		cc, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		c := &pconn{p: p, idx: len(p.conns), client: cc, done: make(chan struct{}), pending: map[int32]pendingReq{}, open: true}
		p.conns = append(p.conns, c)
		black := p.blackNew
		c.blackholed = black
		p.mu.Unlock()
		p.wg.Add(1)
		go func() { defer p.wg.Done(); c.run(black) }()
	}
}

// ---- recording ----

func (p *Proxy) record(r Record) {
	p.mu.Lock()
	r.Seq = len(p.recs)
	p.recs = append(p.recs, r)
	p.mu.Unlock()
}

// Commands returns every recorded frame in order (both directions).
func (p *Proxy) Commands() []Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Record(nil), p.recs...)
}

// Requests returns only the client-to-upstream records.
func (p *Proxy) Requests() []Record {
	var out []Record
	for _, r := range p.Commands() {
		if r.Direction == ToUpstream {
			out = append(out, r)
		}
	}
	return out
}

// RequestsBy returns the requests whose command matches name (case-insensitive).
func (p *Proxy) RequestsBy(name string) []Record {
	var out []Record
	for _, r := range p.Requests() {
		if strings.EqualFold(r.Command, name) {
			out = append(out, r)
		}
	}
	return out
}

// ResetRecording forgets recorded frames.
func (p *Proxy) ResetRecording() { p.mu.Lock(); p.recs = nil; p.mu.Unlock() }

// WaitCommands waits until pred holds over the recorded frames.
func (p *Proxy) WaitCommands(timeout time.Duration, pred func([]Record) bool) bool {
	return poll(timeout, func() bool { return pred(p.Commands()) })
}

// Conns lists every connection accepted so far.
func (p *Proxy) Conns() []ConnInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ConnInfo, len(p.conns))
	for i, c := range p.conns {
		out[i] = ConnInfo{Index: c.idx, Client: c.client.RemoteAddr().String(), AppName: c.appName, Open: c.open, Blackholed: c.blackholed}
	}
	return out
}

// ConnCount is the number of connections accepted so far.
func (p *Proxy) ConnCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.conns) }

// OpenConns is the number of connections currently open.
func (p *Proxy) OpenConns() int {
	n := 0
	for _, c := range p.Conns() {
		if c.Open {
			n++
		}
	}
	return n
}

func poll(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- global faults ----

// PauseAll stops forwarding in both directions (frames are held, not lost)
// until ResumeAll.
func (p *Proxy) PauseAll() {
	p.mu.Lock()
	if !p.paused {
		p.paused = true
		p.pauseCh = make(chan struct{})
	}
	p.mu.Unlock()
}

// ResumeAll resumes forwarding.
func (p *Proxy) ResumeAll() {
	p.mu.Lock()
	if p.paused {
		p.paused = false
		close(p.pauseCh)
	}
	p.mu.Unlock()
}

// KillAll closes every connection (client side with a TCP reset).
func (p *Proxy) KillAll() {
	p.mu.Lock()
	conns := append([]*pconn(nil), p.conns...)
	p.mu.Unlock()
	for _, c := range conns {
		c.close(true)
	}
}

// Kill closes connection i.
func (p *Proxy) Kill(i int) {
	p.mu.Lock()
	var c *pconn
	if i >= 0 && i < len(p.conns) {
		c = p.conns[i]
	}
	p.mu.Unlock()
	if c != nil {
		c.close(true)
	}
}

// Blackhole stops forwarding on connection i in both directions: requests are
// read and discarded, upstream replies are discarded. The connection stays open.
func (p *Proxy) Blackhole(i int) {
	p.mu.Lock()
	if i >= 0 && i < len(p.conns) {
		p.conns[i].black = true
		p.conns[i].blackholed = true
	}
	p.mu.Unlock()
}

// BlackholeNew makes connections accepted from now on be black-holed (nothing
// is relayed and no upstream connection is opened).
func (p *Proxy) BlackholeNew(b bool) { p.mu.Lock(); p.blackNew = b; p.mu.Unlock() }

func (p *Proxy) waitResumed(done <-chan struct{}) bool {
	for {
		p.mu.Lock()
		paused, ch := p.paused, p.pauseCh
		p.mu.Unlock()
		if !paused {
			return true
		}
		select {
		case <-ch:
		case <-done:
			return false
		}
	}
}

// ---- hello rewriting ----

func (p *Proxy) rewriteHello(raw []byte, in info, selfAddr string) []byte {
	p.mu.Lock()
	rw := make(map[string]string, len(p.rewrite))
	for k, v := range p.rewrite {
		if v == "" {
			v = selfAddr
		}
		rw[k] = v
	}
	p.mu.Unlock()
	if len(rw) == 0 && !p.opts.DropCompression {
		return raw
	}
	var d bson.D
	if err := bson.Unmarshal(in.body, &d); err != nil {
		return raw
	}
	changed := false
	mapOne := func(s string) string {
		if v, ok := rw[s]; ok {
			if v != s {
				changed = true
			}
			return v
		}
		return s
	}
	out := d[:0:0]
	for _, e := range d {
		switch e.Key {
		case "hosts", "passives", "arbiters":
			if a, ok := e.Value.(bson.A); ok {
				na := make(bson.A, len(a))
				for i, v := range a {
					if s, ok := v.(string); ok {
						na[i] = mapOne(s)
					} else {
						na[i] = v
					}
				}
				e.Value = na
			}
		case "primary", "me":
			if s, ok := e.Value.(string); ok {
				e.Value = mapOne(s)
			}
		case "compression":
			if p.opts.DropCompression {
				changed = true
				continue
			}
		}
		out = append(out, e)
	}
	if !changed {
		return raw
	}
	nb, err := bson.Marshal(out)
	if err != nil {
		return raw
	}
	nr, err := rewriteReply(raw, nb)
	if err != nil {
		return raw
	}
	return nr
}

func isHello(cmd string) bool {
	l := strings.ToLower(cmd)
	return l == "hello" || l == "ismaster"
}
