// Package mongosim is a simulated MongoDB upstream for tests: a wire-level
// server with a scriptable hello topology, a SCRAM-SHA-256 server, per-command
// fault rules and full recording of messages and connections.
//
// It encodes and decodes BSON with the official Go driver's bson package and
// its own framing. It never imports corral's own code. All data is synthetic.
//
// See README.md for the source contract and known differences from a real
// mongod.
package mongosim

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Options configures a Server.
type Options struct {
	// Addr is the listen address; empty means 127.0.0.1:0.
	Addr string
	// Hello is the initial hello spec (zero value: Standalone).
	Hello HelloSpec
	// Users maps synthetic usernames to synthetic passwords for SCRAM-SHA-256.
	Users map[string]string
	// RequireAuth makes commands other than the handshake family, ping and
	// endSessions fail with code 13 on unauthenticated connections.
	RequireAuth bool
	// TLS selects the upstream TLS mode; the zero value (TLSOff) serves
	// plaintext exactly as before. See tls.go.
	TLS TLSMode
	// Authority signs the server certificate in the TLS modes; when nil a new
	// one is generated. Pass one Authority to several servers to share a CA.
	Authority *Authority
}

// Message is one request the server received.
type Message struct {
	Seq           int // global receive order, from 0
	ConnIndex     int
	AppName       string // handshake appName of the connection (as known at receipt)
	Authenticated bool   // whether the connection had authenticated at receipt
	OpCode        int32
	Flags         uint32
	RequestID     int32
	ResponseTo    int32
	Command       string
	DB            string
	HasLsid       bool
	HasTxnNumber  bool
	Marker        string // corraltestMarker when it is a string
	Body          bson.Raw
	Sequences     map[string][]bson.Raw // OP_MSG kind-1 sections by identifier
	Raw           []byte                // the complete frame as received
	Time          time.Time
}

// ConnInfo describes one accepted connection.
type ConnInfo struct {
	Index         int
	Remote        string
	AppName       string
	DriverName    string
	DriverVersion string
	Authenticated bool
	AuthMechanism string
	AuthUser      string // synthetic
	Open          bool
	// InFlight is true while a received request has not been fully handled
	// (held, delayed or being answered). An open, non-InFlight connection is idle.
	InFlight bool
	// PartialBytes is how many bytes of a not-yet-complete message have been
	// received; BytesReceived counts all bytes read on the connection.
	PartialBytes  int
	BytesReceived int64
	// SignatureForged is set when BadServerSignature was scripted on it.
	SignatureForged bool
	Blackholed      bool
	Opened          time.Time
	Closed          time.Time
	Messages        int
}

// Server is a simulated MongoDB upstream.
type Server struct {
	tb   testing.TB
	opts Options

	mu               sync.Mutex
	ln               net.Listener
	addr             string
	running          bool
	refuse           bool
	blackhole        bool
	spec             HelloSpec
	counter          int64
	topoCh           chan struct{}
	processID        bson.ObjectID
	conns            []*conn
	attempts         int
	attemptsBy       map[string]int
	msgs             []Message
	rules            []*RuleHandle
	checks           []*checkFault
	checkGate        chan struct{}
	refuseAfterHello bool
	refuseApps       map[string]bool
	authSticky       AuthOutcome
	authQueue        []AuthOutcome
	nonceSeq         int64
	reqSeq           int32
	wg               sync.WaitGroup
	tlsCfg           *tls.Config
	auth             *Authority
	tlsFailures      int
	tlsMode          TLSMode
	hsLog            []TLSHandshake
}

// Start launches a server on 127.0.0.1:0 (or opts.Addr) and stops it through
// tb.Cleanup.
func Start(tb testing.TB, opts Options) *Server {
	tb.Helper()
	s := &Server{
		tb:         tb,
		opts:       opts,
		spec:       opts.Hello,
		counter:    1,
		topoCh:     make(chan struct{}),
		checkGate:  make(chan struct{}),
		attemptsBy: map[string]int{},
		authSticky: Succeed,
		processID:  bson.ObjectID{0x6d, 0x6f, 0x6e, 0x67, 0x6f, 0x73, 0x69, 0x6d, 0, 0, 0, 1},
	}
	s.auth = opts.Authority
	if opts.TLS != TLSOff {
		if s.auth == nil {
			s.auth = NewAuthority(tb)
		}
		s.tlsMode = opts.TLS
		s.tlsCfg = s.auth.serverConfig(tb, opts.TLS)
	}
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		tb.Fatalf("mongosim: listen: %v", err)
	}
	s.addr = ln.Addr().String()
	s.listen(ln)
	tb.Cleanup(s.Stop)
	return s
}

func (s *Server) listen(ln net.Listener) {
	s.mu.Lock()
	s.ln = ln
	s.running = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ln)
}

// Addr is the host:port the server listens on (stable across Stop/Restart).
func (s *Server) Addr() string { return s.addr }

// Stop closes the listener and every connection. The address becomes
// unreachable. It is idempotent.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.running = false
	conns := append([]*conn(nil), s.conns...)
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		c.close(false)
	}
	s.wg.Wait()
}

// Restart listens again on the same port (stopping first if still running).
func (s *Server) Restart() {
	s.tb.Helper()
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		s.Stop()
	}
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ {
		ln, err = net.Listen("tcp", s.addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		s.tb.Fatalf("mongosim: restart: %v", err)
	}
	s.listen(ln)
}

// RefuseNew makes new connections be accepted and closed at once.
func (s *Server) RefuseNew(b bool) { s.mu.Lock(); s.refuse = b; s.mu.Unlock() }

// Blackhole makes new connections be accepted but never read or answered.
func (s *Server) Blackhole(b bool) { s.mu.Lock(); s.blackhole = b; s.mu.Unlock() }

// CloseConns closes every open connection (TCP reset), leaving the listener up.
func (s *Server) CloseConns() {
	s.mu.Lock()
	conns := append([]*conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.close(true)
	}
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if !s.running || s.ln != ln {
			// Stop raced with Accept: do not register a connection.
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.attempts++
		var cmode TLSMode
		if s.tlsCfg != nil {
			nc = tls.Server(nc, s.tlsCfg)
			cmode = s.tlsMode
		}
		c := &conn{
			tlsMode: cmode,
			s:       s,
			idx:     len(s.conns),
			nc:      nc,
			in:      make(chan []byte, 256),
			done:    make(chan struct{}),
			opened:  time.Now(),
			open:    true,
		}
		s.conns = append(s.conns, c)
		refuse, hole := s.refuse, s.blackhole
		c.blackholed = hole && !refuse
		s.mu.Unlock()
		switch {
		case refuse:
			c.close(false)
		case hole:
			// Hold the socket open; Stop closes it.
		default:
			s.wg.Add(2)
			go func() { defer s.wg.Done(); c.readLoop() }()
			go func() { defer s.wg.Done(); c.serve() }()
		}
	}
}

// ---- recording ----

// Received returns a copy of every message received so far, in order.
func (s *Server) Received() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// ReceivedCommand returns the received messages whose command matches name
// (case-insensitive).
func (s *Server) ReceivedCommand(name string) []Message {
	var out []Message
	for _, m := range s.Received() {
		if strings.EqualFold(m.Command, name) {
			out = append(out, m)
		}
	}
	return out
}

// ResetRecording forgets recorded messages (connections and attempt counts stay).
func (s *Server) ResetRecording() { s.mu.Lock(); s.msgs = nil; s.mu.Unlock() }

// WaitReceived waits until pred holds over the recorded messages.
func (s *Server) WaitReceived(timeout time.Duration, pred func([]Message) bool) bool {
	return poll(timeout, func() bool { return pred(s.Received()) })
}

// ConnAttempts is the number of TCP connections accepted (including refused
// and black-holed ones).
func (s *Server) ConnAttempts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.attempts }

// ConnAttemptsBy is the number of connections whose first hello carried
// appName. A connection's appName is only known after that hello, so refused
// or black-holed connections are counted by ConnAttempts only.
func (s *Server) ConnAttemptsBy(appName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attemptsBy[appName]
}

// OpenConns counts open connections with the given handshake appName.
func (s *Server) OpenConns(appName string) int {
	n := 0
	for _, ci := range s.Conns() {
		if ci.Open && ci.AppName == appName {
			n++
		}
	}
	return n
}

// Conns lists every connection ever accepted, by index.
func (s *Server) Conns() []ConnInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ConnInfo, len(s.conns))
	for i, c := range s.conns {
		out[i] = c.infoLocked()
	}
	return out
}

// ---- topology ----

// SetHello switches the hello answer live and bumps the topologyVersion
// counter (waking any awaitable hello).
func (s *Server) SetHello(h HelloSpec) {
	s.mu.Lock()
	s.spec = h
	s.counter++
	ch := s.topoCh
	s.topoCh = make(chan struct{})
	s.mu.Unlock()
	close(ch)
}

// Hello returns the current spec.
func (s *Server) Hello() HelloSpec { s.mu.Lock(); defer s.mu.Unlock(); return s.spec }

// ---- monitoring-check failures ----

// CheckMode is how a failing monitoring check misbehaves.
type CheckMode struct {
	kind int
	d    time.Duration
	code int32
	name string
}

var (
	// CheckReset closes the connection with a TCP reset instead of answering.
	CheckReset = CheckMode{kind: 1}
	// CheckNoReply does not answer until ReleaseChecks or the connection closes.
	CheckNoReply = CheckMode{kind: 2}
	// CheckMalformed answers with an invalid BSON body.
	CheckMalformed = CheckMode{kind: 4}
)

// CheckDelay answers normally after d.
func CheckDelay(d time.Duration) CheckMode { return CheckMode{kind: 3, d: d} }

// CheckError answers {ok: 0, code, codeName}.
func CheckError(code int32, name string) CheckMode { return CheckMode{kind: 5, code: code, name: name} }

type checkFault struct {
	appName   string
	remaining int
	mode      CheckMode
}

// MonitorAppName is the handshake appName of corral's monitoring connections.
const MonitorAppName = "corral-monitor"

// FailChecks fails the next n hello / isMaster messages on connections whose
// handshake appName is "corral-monitor". n < 0 fails every one until cleared.
func (s *Server) FailChecks(n int, mode CheckMode) { s.FailChecksOn(MonitorAppName, n, mode) }

// FailChecksAll is FailChecks for connections of any appName.
func (s *Server) FailChecksAll(n int, mode CheckMode) { s.FailChecksOn("", n, mode) }

// FailChecksOn fails the next n hello messages on connections with the given
// appName ("" matches all connections).
func (s *Server) FailChecksOn(appName string, n int, mode CheckMode) {
	s.mu.Lock()
	s.checks = append(s.checks, &checkFault{appName: appName, remaining: n, mode: mode})
	s.mu.Unlock()
}

// ClearChecks discards pending check failures.
func (s *Server) ClearChecks() { s.mu.Lock(); s.checks = nil; s.mu.Unlock() }

// ReleaseChecks ends every CheckNoReply wait; the checks then get a normal reply.
func (s *Server) ReleaseChecks() {
	s.mu.Lock()
	g := s.checkGate
	s.checkGate = make(chan struct{})
	s.mu.Unlock()
	close(g)
}

func (s *Server) takeCheck(appName string) (CheckMode, chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.checks {
		if f.remaining == 0 || (f.appName != "" && f.appName != appName) {
			continue
		}
		if f.remaining > 0 {
			f.remaining--
			if f.remaining == 0 {
				s.checks = append(s.checks[:i:i], s.checks[i+1:]...)
			}
		}
		return f.mode, s.checkGate, true
	}
	return CheckMode{}, nil, false
}

// ---- authentication ----

// SetAuthOutcome sets the outcome applied to every SCRAM conversation (until
// changed). Queued outcomes take precedence.
func (s *Server) SetAuthOutcome(o AuthOutcome) { s.mu.Lock(); s.authSticky = o; s.mu.Unlock() }

// QueueAuthOutcomes scripts the next conversations in order; once the queue is
// empty the sticky outcome applies again.
func (s *Server) QueueAuthOutcomes(os ...AuthOutcome) {
	s.mu.Lock()
	s.authQueue = append(s.authQueue, os...)
	s.mu.Unlock()
}

func (s *Server) nextAuthOutcome() AuthOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.authQueue) > 0 {
		o := s.authQueue[0]
		s.authQueue = s.authQueue[1:]
		return o
	}
	return s.authSticky
}

func (s *Server) nextRequestID() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqSeq++
	return s.reqSeq
}

func (s *Server) String() string { return fmt.Sprintf("mongosim(%s)", s.addr) }

// RefuseAfterHello makes every connection read its first hello (so its appName
// and the per-appName attempt count are recorded) and then close with a reset
// without answering.
func (s *Server) RefuseAfterHello(b bool) { s.mu.Lock(); s.refuseAfterHello = b; s.mu.Unlock() }

// RefuseApp makes connections whose handshake appName is appName read their
// hello (recording it) and then close with a reset, while other appNames (for
// example the monitor) are answered normally. Use it to refuse pool
// connections ("corral") while monitoring checks keep succeeding.
func (s *Server) RefuseApp(appName string, b bool) {
	s.mu.Lock()
	if s.refuseApps == nil {
		s.refuseApps = map[string]bool{}
	}
	s.refuseApps[appName] = b
	s.mu.Unlock()
}

func (s *Server) refusesApp(appName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuseAfterHello || s.refuseApps[appName]
}

// SendRaw writes unsolicited bytes to connection connIndex (AC-46: data on an
// idle pool connection). It fails if the connection is closed or unknown.
func (s *Server) SendRaw(connIndex int, b []byte) error {
	c, err := s.connAt(connIndex)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.nc.Write(b)
	return err
}

// CloseConn closes connection connIndex; reset sends a TCP RST instead of FIN.
func (s *Server) CloseConn(connIndex int, reset bool) error {
	c, err := s.connAt(connIndex)
	if err != nil {
		return err
	}
	c.close(reset)
	return nil
}

// IdleConns lists the indexes of open, not-in-flight connections whose
// handshake appName is appName (the idle pool connections for "corral").
func (s *Server) IdleConns(appName string) []int {
	var out []int
	for _, ci := range s.Conns() {
		if ci.Open && !ci.InFlight && !ci.Blackholed && ci.AppName == appName {
			out = append(out, ci.Index)
		}
	}
	return out
}

func (s *Server) connAt(i int) (*conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.conns) {
		return nil, fmt.Errorf("mongosim: no connection %d", i)
	}
	c := s.conns[i]
	if !c.open {
		return nil, fmt.Errorf("mongosim: connection %d is closed", i)
	}
	return c, nil
}

// StallNextHello delays the reply to the next hello / isMaster received on the
// given open connection by d, once. It applies to that connection only (unlike
// FailChecksOn(appName, n, CheckDelay(d)), which matches the next n hellos on
// any connection with an appName). The stall blocks the connection, as on mongod.
func (s *Server) StallNextHello(connIndex int, d time.Duration) error {
	c, err := s.connAt(connIndex)
	if err != nil {
		return err
	}
	s.mu.Lock()
	c.stallNext = d
	s.mu.Unlock()
	return nil
}
