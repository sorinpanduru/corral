package proxyserver_test

import (
	"context"
	"crypto/tls"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// FR-1: Bind failure on a port in use: a config error for proxy.listen, no
// address in its text, detail EADDRINUSE.
func TestBindFailure_FR1(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	srv := proxyserver.New(proxyserver.Options{Addr: addr, MaxConns: 1, IOTimeout: time.Second, Shutdown: proxy.NewShutdown(0, time.Second, clock.System()), IDs: proxy.NewConnIDs()})
	err = srv.Bind()
	if err == nil {
		t.Fatal("Bind on a used port succeeded")
	}
	var ce errclass.ConfigError
	if !errors.As(err, &ce) || ce.ConfigSetting() != "proxy.listen" {
		t.Fatalf("error %T is not a config error for proxy.listen", err)
	}
	if strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the error text carries the address: %q", err.Error())
	}
	if c, d := errclass.Classify(err); c != errclass.ClassConfig || d != errclass.DetailEADDRINUSE {
		t.Fatalf("Classify = (%s, %s), want config/EADDRINUSE", c, d)
	}
}

// AC-2: with MaxConns=3 a fourth connection gets no bytes and is closed, and
// ConnRejected fires; a slot freed by a close is usable again.
func TestConnectionLimit_AC2(t *testing.T) {
	r := newRig(t, opts{maxConns: 3})
	var held []*rawwire.Conn
	for i := 0; i < 3; i++ {
		c := r.dial()
		roundTrip(t, c, hello(int32(i+1)))
		held = append(held, c)
	}
	over := r.dial()
	if err := over.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("over-limit connection: %v", err)
	}
	eventually(t, "ConnRejected", func() bool { _, rej := r.obs.counts(); return rej == 1 })
	if r.val("corral_client_connections_closed_total", "reason", "limit_exceeded") != 1 {
		t.Fatal("closed_total{limit_exceeded} not 1")
	}
	held[0].Close()
	eventually(t, "a slot to free", func() bool { return r.val("corral_client_connections") == 2 })
	c := r.dial()
	if m := roundTrip(t, c, hello(9)); m.Header.ResponseTo != 9 {
		t.Fatal("a connection after the slot freed was not served")
	}
}

// AC-39 (unit): with MaxConns=1, three connections give accepted +3 and the
// gauge never above 1.
func TestAcceptedCountsOverLimit_AC39(t *testing.T) {
	r := newRig(t, opts{maxConns: 1})
	first := r.dial()
	roundTrip(t, first, hello(1))
	for i := 0; i < 2; i++ {
		c := r.dial()
		if err := c.ExpectClosedNoBytes(5 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "accepted 3", func() bool { return r.val("corral_client_connections_accepted_total") == 3 })
	if g := r.val("corral_client_connections"); g != 1 {
		t.Fatalf("gauge = %v, want 1", g)
	}
	first.Close()
	eventually(t, "gauge 0", func() bool { return r.val("corral_client_connections") == 0 })
}

// FR-3, AC-2: connection IDs are positive, unique among open connections, and
// reported as connectionId; they wrap after 2,147,483,647.
func TestConnectionIDs_FR3(t *testing.T) {
	r := newRig(t, opts{ids: proxy.NewConnIDsAt(math.MaxInt32 - 1)})
	var got []int64
	var conns []*rawwire.Conn
	for i := 0; i < 3; i++ {
		c := r.dial()
		got = append(got, connID(roundTrip(t, c, hello(int32(i+1)))))
		conns = append(conns, c)
	}
	want := []int64{math.MaxInt32 - 1, math.MaxInt32, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
	_ = conns
}

// FR-4 (AC-3 unit): an idle connection is closed after the idle timeout; one
// with an exchange in progress is not.
func TestIdleTimeout_AC3(t *testing.T) {
	r := newRig(t, opts{idleTimeout: 300 * time.Millisecond})
	idle := r.dial()
	busy := r.dial()
	h := r.twin.AddRule(mongosim.Match{Marker: "slow"}, mongosim.Delay(900*time.Millisecond))
	_ = h
	busy.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "d"}, {Key: "corraltestMarker", Value: "slow"}}}))
	if err := idle.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("idle connection not closed: %v", err)
	}
	m, err := busy.ReadMessage(10 * time.Second)
	if err != nil || m.Header.ResponseTo != 1 {
		t.Fatalf("the connection with an exchange in progress was cut: %v", err)
	}
	eventually(t, "idle_timeout reason", func() bool {
		for _, x := range r.obs.reasons() {
			if x == proxy.CloseIdleTimeout {
				return true
			}
		}
		return false
	})
}

// FR-44: TLS. A trusted client connects; a plaintext client is rejected with
// tls_failure; with client certificates required, a client without one is
// refused and one with a valid certificate is served. No certificate data
// reaches the records.
func TestTLS_FR44(t *testing.T) {
	p := newPKI(t)
	r := newRig(t, opts{tlsCfg: p.serverConfig(false)})
	c := rawwire.DialTLS(t, r.addr(), p.clientConfig(nil))
	if m := roundTrip(t, c, hello(1)); m.Header.ResponseTo != 1 {
		t.Fatal("TLS client not served")
	}
	plain := r.dial()
	plain.Send(hello(2))
	if err := plain.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("plaintext client: %v", err)
	}
	eventually(t, "tls_failure", func() bool {
		for _, x := range r.obs.reasons() {
			if x == proxy.CloseTLSFailure {
				return true
			}
		}
		return false
	})
	r.obs.mu.Lock()
	for _, e := range r.obs.tlsErrs {
		if strings.Contains(e.Error(), "SYNTHETIC") {
			t.Error("certificate data in a TLS failure")
		}
	}
	r.obs.mu.Unlock()
	for _, rec := range r.lh.Records() {
		for _, a := range rec.Attrs {
			if strings.Contains(a.Value.String(), "SYNTHETIC") {
				t.Fatalf("certificate identity in record %q", rec.Msg)
			}
		}
	}
}

func TestMutualTLS_FR44(t *testing.T) {
	p := newPKI(t)
	r := newRig(t, opts{tlsCfg: p.serverConfig(true)})
	good := p.issue(t, 10, "synthetic-client", 2 /* ExtKeyUsageClientAuth */)
	c := rawwire.DialTLS(t, r.addr(), p.clientConfig(&good))
	if m := roundTrip(t, c, hello(1)); m.Header.ResponseTo != 1 {
		t.Fatal("client with a valid certificate not served")
	}
	// No certificate: the handshake (or the first read, on TLS 1.3) fails.
	raw, err := net.DialTimeout("tcp", r.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, p.clientConfig(nil))
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		tc.Write(hello(5))
		if _, err := tc.Read(make([]byte, 1)); err == nil {
			t.Fatal("a client without a certificate was served")
		}
	}
	eventually(t, "tls_failure", func() bool {
		for _, x := range r.obs.reasons() {
			if x == proxy.CloseTLSFailure {
				return true
			}
		}
		return false
	})
}

// countConn counts bytes read from the server.
type countConn struct {
	net.Conn
	mu sync.Mutex
	n  int
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
	return n, err
}

// AC-45 (unit): over the limit with TLS on, the client receives zero bytes (no
// ServerHello): the limit is checked before the handshake.
func TestOverLimitWithTLSSendsNothing_AC45(t *testing.T) {
	p := newPKI(t)
	r := newRig(t, opts{maxConns: 1, tlsCfg: p.serverConfig(false)})
	first := rawwire.DialTLS(t, r.addr(), p.clientConfig(nil))
	roundTrip(t, first, hello(1))
	raw, err := net.DialTimeout("tcp", r.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cc := &countConn{Conn: raw}
	tc := tls.Client(cc, p.clientConfig(nil))
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatal("the handshake of an over-limit connection succeeded")
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.n != 0 {
		t.Fatalf("the over-limit connection received %d bytes, want 0", cc.n)
	}
	eventually(t, "rejected", func() bool { _, rej := r.obs.counts(); return rej == 1 })
}

// AC-45 (unit): a partial ClientHello that stalls is closed after the I/O
// timeout, with tls_failure, class timeout, detail deadline_exceeded.
func TestStalledClientHello_AC45(t *testing.T) {
	p := newPKI(t)
	r := newRig(t, opts{tlsCfg: p.serverConfig(false), ioTimeout: 300 * time.Millisecond})
	conn := rawwire.DialStalledClientHello(t, r.addr())
	defer conn.Close()
	begin := time.Now()
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("bytes received from a stalled handshake")
	}
	if d := time.Since(begin); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("closed after %v", d)
	}
	eventually(t, "tls_failure with a timeout error", func() bool {
		r.obs.mu.Lock()
		defer r.obs.mu.Unlock()
		if len(r.obs.tlsErrs) == 0 {
			return false
		}
		c, d := errclass.Classify(r.obs.tlsErrs[0])
		return c == errclass.ClassTimeout && d == errclass.DetailDeadlineExceeded
	})
	eventually(t, "close reason", func() bool {
		for _, x := range r.obs.reasons() {
			if x == proxy.CloseTLSFailure {
				return true
			}
		}
		return false
	})
	// A TLS failure counts as accepted and leaves the gauge at zero.
	if r.val("corral_client_connections_accepted_total") < 1 {
		t.Fatal("accepted_total not incremented")
	}
	eventually(t, "gauge 0", func() bool { return r.val("corral_client_connections") == 0 })
}

// FR-42 (AC-22 unit): a panic in one connection closes that one only; a
// concurrent client completes; no panic value appears.
func TestPanicIsolation_AC22(t *testing.T) {
	r := newRig(t, opts{panicCmd: "boom"})
	good := r.dial()
	roundTrip(t, good, ping(1))
	bad := r.dial()
	bad.Send(rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: 1, Body: bson.D{{Key: "boom", Value: "SYNTHETIC-PANIC-MARKER"}, {Key: "$db", Value: "d"}}}))
	if err := bad.ExpectClosedNoBytes(5 * time.Second); err != nil {
		t.Fatalf("panicking connection not closed: %v", err)
	}
	if m := roundTrip(t, good, ping(2)); m.Header.ResponseTo != 2 {
		t.Fatal("a concurrent client was affected")
	}
	eventually(t, "internal_error", func() bool {
		for _, x := range r.obs.reasons() {
			if x == proxy.CloseInternalError {
				return true
			}
		}
		return false
	})
	r.obs.mu.Lock()
	n := len(r.obs.panics)
	r.obs.mu.Unlock()
	if n != 1 {
		t.Fatalf("PanicRecovered calls = %d", n)
	}
	for _, rec := range r.lh.Records() {
		for _, a := range rec.Attrs {
			if strings.Contains(a.Value.String(), "SYNTHETIC-PANIC-MARKER") {
				t.Fatal("the panic value reached a record")
			}
		}
	}
}

// ADR-0018: the accounting invariant: accepted = opened-within-limit +
// rejected, and every accepted connection ends in exactly one close reason.
func TestAccountingInvariant_ADR0018(t *testing.T) {
	p := newPKI(t)
	r := newRig(t, opts{maxConns: 2, tlsCfg: p.serverConfig(false), ioTimeout: 500 * time.Millisecond, panicCmd: "boom"})
	a := rawwire.DialTLS(t, r.addr(), p.clientConfig(nil))
	b := rawwire.DialTLS(t, r.addr(), p.clientConfig(nil))
	roundTrip(t, a, hello(1))
	roundTrip(t, b, hello(1))
	over := r.dial() // over the limit
	over.ExpectClosedNoBytes(5 * time.Second)
	a.Close()
	eventually(t, "a closed", func() bool { return r.val("corral_client_connections") == 1 })
	plain := r.dial() // TLS failure
	plain.Send(hello(2))
	plain.ExpectClosedNoBytes(5 * time.Second)
	b.Close()
	eventually(t, "all closed", func() bool { return r.val("corral_client_connections") == 0 })
	time.Sleep(100 * time.Millisecond)
	accepted := r.val("corral_client_connections_accepted_total")
	opened, rejected := r.obs.counts()
	if int(accepted) != opened+rejected {
		t.Fatalf("accepted=%v opened=%d rejected=%d", accepted, opened, rejected)
	}
	if closed := r.sum("corral_client_connections_closed_total"); closed != accepted {
		t.Fatalf("closed_total=%v, accepted=%v: every accepted connection ends in one close reason", closed, accepted)
	}
	if r.val("corral_client_connections") != 0 {
		t.Fatal("gauge not back to 0")
	}
}

var _ = context.Background
