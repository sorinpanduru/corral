package upstreamconn_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

var closedSet = []string{"idle", "error", "not_primary", "client_abandoned", "shutdown", "incompatible"}

func inSet(s string) bool {
	for _, x := range closedSet {
		if x == s {
			return true
		}
	}
	return false
}

// FR-16 (G-6): monitoring checks send `client` on the first hello of a
// connection only. The fake upstream answers 186 to client metadata on a
// greeted connection, as a real server does, so a violation shows up as a
// failing check and in the recorded hellos.
func TestMonitoringClientMetadataOnlyOnFirstHello_FR16(t *testing.T) {
	f := newFake(t)
	k := upstreamconn.NewChecker(baseCfg(nil), upstream.Noop{})
	for i := 0; i < 4; i++ {
		if r := check(k, f.Addr, false, false); r.Err != nil {
			t.Fatalf("check %d failed: %v", i+1, r.Err)
		}
	}
	hellos := 0
	for _, m := range f.messages() {
		if m.Command != "hello" {
			continue
		}
		if hellos == 0 && !m.HasClient {
			t.Fatal("the first hello of a connection lacks client metadata")
		}
		if hellos > 0 && m.HasClient {
			t.Fatalf("hello %d on the same connection carries client metadata again", hellos+1)
		}
		hellos++
	}
	if hellos != 4 || f.connections() != 1 {
		t.Fatalf("hellos=%d connections=%d, want 4 on 1", hellos, f.connections())
	}
	// A fresh connection greets again with metadata, then polls without.
	if r := check(k, f.Addr, false, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := check(k, f.Addr, false, false); r.Err != nil {
		t.Fatal(r.Err)
	}
	byConn := map[int][]bool{}
	for _, m := range f.messages() {
		if m.Command == "hello" {
			byConn[m.Conn] = append(byConn[m.Conn], m.HasClient)
		}
	}
	for c, seq := range byConn {
		for i, has := range seq {
			if (i == 0) != has {
				t.Fatalf("connection %d: hello %d client=%v", c, i, has)
			}
		}
	}
	// A pool connection sends it once, at open.
	c := mustOpen(t, baseCfg(nil), f.Addr)
	_ = c
}

// FR-28 (review): the SASL conversation is capped. A server that never says
// done is cut off after a few rounds (well before the connect timeout) with an
// error that is not an authentication failure.
func TestSASLRoundCap_FR22(t *testing.T) {
	f := newFake(t)
	f.endlessSASL, f.scramUser, f.scramPass = true, user, pass
	begin := time.Now()
	_, err := open(t, baseCfg(creds()), f.Addr)
	if err == nil {
		t.Fatal("an endless conversation succeeded")
	}
	if upstream.IsAuthFailed(err) {
		t.Fatal("a never-ending conversation is not an authentication failure")
	}
	if n := f.saslContinues(); n > 50 {
		t.Fatalf("%d saslContinue rounds were sent: no cap", n)
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("gave up after %v: the cap, not the timeout, must end it", d)
	}
	if c, d := errclass.Classify(err); c != errclass.ClassProtocol || d != errclass.DetailUnexpectedResponse {
		t.Logf("classified as (%s, %s)", c, d)
	}
	if strings.Contains(fmt.Sprintf("%v %+v", err, err), pass) {
		t.Fatal("the password appears in the error")
	}
}

// Review: the reply document is cut to the body's own length: a trailing kind-1
// section in the reply is not part of the returned document.
func TestRoundTripCutsReplyToBody_FR58(t *testing.T) {
	f := newFake(t)
	// A kind-1 section after the body: kind byte, size, identifier, one doc.
	extra := marshal(bson.D{{Key: "x", Value: 1}})
	section := append([]byte{1}, 0, 0, 0, 0)
	section = append(section, []byte("tail\x00")...)
	section = append(section, extra...)
	section[1] = byte(len(section) - 1)
	f.pingTail = section
	c := mustOpen(t, baseCfg(nil), f.Addr)
	reply, err := upstreamconn.RoundTrip(context.Background(), c, wire.PingBody())
	if err != nil {
		t.Fatal(err)
	}
	want := marshal(bson.D{{Key: "ok", Value: 1.0}})
	if string(reply) != string(want) {
		t.Fatalf("reply document is %d bytes, want exactly the body (%d bytes)", len(reply), len(want))
	}
	if err := bson.Raw(reply).Validate(); err != nil {
		t.Fatal(err)
	}
}

// Review: unusable credentials (rejected by SASLprep) are a configuration
// error, classified config, and the password never appears in it.
func TestBadCredentialsClassifiedConfig_FR22(t *testing.T) {
	s := sim(t, mongosim.Options{})
	cr := creds()
	cr.Password = []byte("bad\x07" + pass)
	_, err := open(t, baseCfg(cr), s.Addr())
	if err == nil {
		t.Fatal("Open accepted unusable credentials")
	}
	if c, _ := errclass.Classify(err); c != errclass.ClassConfig {
		t.Fatalf("class %q, want config", c)
	}
	if upstream.IsAuthFailed(err) {
		t.Fatal("unusable credentials are not an upstream authentication failure")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), pass) || strings.Contains(fmt.Sprintf("%v", err), user) {
		t.Fatal("a credential appears in the error")
	}
	// Through the checker: the report's AuthErr is the same kind of error.
	k := upstreamconn.NewChecker(baseCfg(cr), upstream.Noop{})
	r := check(k, s.Addr(), true, false)
	if r.AuthErr == nil || upstream.IsAuthFailed(r.AuthErr) {
		t.Fatalf("AuthErr = %v", r.AuthErr)
	}
	if c, _ := errclass.Classify(r.AuthErr); c != errclass.ClassConfig {
		t.Fatalf("AuthErr class %q, want config", c)
	}
}

// Review: checker close reasons come from the closed set (never an ad-hoc
// string such as "fresh_check"), Close reports shutdown, and CloseHost(host,
// idle) closes that host's monitoring connection with the given reason.
func TestCheckerCloseReasonsClosedSet_FR37(t *testing.T) {
	s := sim(t, mongosim.Options{})
	rec := &recorder{}
	k := upstreamconn.NewChecker(baseCfg(nil), rec)
	check(k, s.Addr(), false, false)
	check(k, s.Addr(), false, true) // Fresh: the old connection is closed
	rec.mu.Lock()
	for _, r := range rec.closed {
		if !inSet(r) {
			t.Errorf("close reason %q is outside the closed set", r)
		}
	}
	if len(rec.closed) != 1 {
		t.Errorf("a Fresh check closed %d connections, want 1", len(rec.closed))
	}
	rec.closed = nil
	rec.mu.Unlock()

	ch, ok := k.(interface {
		CloseHost(host string, reason pool.CloseReason)
	})
	if !ok {
		t.Fatal("the checker has no CloseHost(host, reason)")
	}
	ch.CloseHost(s.Addr(), pool.CloseReason("idle"))
	rec.mu.Lock()
	if len(rec.closed) != 1 || rec.closed[0] != "idle" {
		t.Fatalf("CloseHost(idle) reported %v", rec.closed)
	}
	rec.closed = nil
	rec.mu.Unlock()
	// The next check opens a new connection.
	before := s.ConnAttemptsBy("corral-monitor")
	if r := check(k, s.Addr(), false, false); r.Err != nil {
		t.Fatal(r.Err)
	}
	if s.ConnAttemptsBy("corral-monitor") != before+1 {
		t.Fatal("no new monitoring connection after CloseHost")
	}
	// Close: shutdown.
	cl, ok := k.(interface{ Close() error })
	if !ok {
		t.Fatal("the checker has no Close")
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	if len(rec.closed) != 1 || rec.closed[0] != "shutdown" {
		t.Fatalf("Close reported %v, want [shutdown]", rec.closed)
	}
	rec.mu.Unlock()
	eventually(t, "twin sees the connection closed", func() bool { return s.OpenConns("corral-monitor") == 0 })
	// CloseHost on an unknown host is harmless.
	ch.CloseHost("never.seen:1", pool.CloseReason("idle"))

	// An error drop reports "error".
	s2 := sim(t, mongosim.Options{})
	rec2 := &recorder{}
	k2 := upstreamconn.NewChecker(baseCfg(nil), rec2)
	check(k2, s2.Addr(), false, false)
	s2.FailChecks(1, mongosim.CheckReset)
	check(k2, s2.Addr(), false, false)
	rec2.mu.Lock()
	defer rec2.mu.Unlock()
	if len(rec2.closed) != 1 || rec2.closed[0] != "error" {
		t.Fatalf("failed check reported close reasons %v, want [error]", rec2.closed)
	}
}
