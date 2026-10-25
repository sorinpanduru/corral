package upstreamconn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func attrs(err error) map[string]string {
	m := map[string]string{}
	for _, a := range errclass.LogAttrs(err) {
		m[a.Key] = a.Value.String()
	}
	return m
}

// FR-28: corral's own handshake presents appName corral, driver corral and
// the version; mechanism none sends no SASL.
func TestOpenerHandshakeMetadata_FR28(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := mustOpen(t, baseCfg(nil), s.Addr())
	if c.Host() != s.Addr() {
		t.Fatalf("Host() = %q", c.Host())
	}
	ci := s.Conns()
	if len(ci) != 1 || ci[0].AppName != "corral" || ci[0].DriverName != "corral" || ci[0].DriverVersion != "9.8.7" {
		t.Fatalf("handshake metadata: %+v", ci)
	}
	if ci[0].Authenticated || len(s.ReceivedCommand("saslStart")) != 0 {
		t.Fatal("authentication ran with no credentials")
	}
	if c.Correlator() == nil || c.NetConn() == nil {
		t.Fatal("Correlator or NetConn is nil")
	}
	if c.MaxMessageSize() != mongosim.DefaultMaxMessageSizeBytes {
		t.Fatalf("MaxMessageSize() = %d", c.MaxMessageSize())
	}
}

// The mechanism "none" is the same as no credentials.
func TestOpenerMechanismNone_FR22(t *testing.T) {
	s := sim(t, mongosim.Options{})
	mustOpen(t, baseCfg(&upstreamconn.Credentials{Mechanism: "none"}), s.Addr())
	if len(s.ReceivedCommand("saslStart")) != 0 || len(s.ReceivedCommand("saslContinue")) != 0 {
		t.Fatal("SASL was sent with mechanism none")
	}
}

// FR-28: the per-connection maxMessageSizeBytes comes from the upstream's hello.
func TestOpenerMaxMessageSize_FR58(t *testing.T) {
	s := sim(t, mongosim.Options{Hello: mongosim.HelloSpec{Role: mongosim.Mongos, MaxMessageSizeBytes: 1_000_000}})
	if c := mustOpen(t, baseCfg(nil), s.Addr()); c.MaxMessageSize() != 1_000_000 {
		t.Fatalf("MaxMessageSize() = %d", c.MaxMessageSize())
	}
}

// IDs are unique and increasing; Close closes the socket.
func TestOpenerIDsAndClose_FR28(t *testing.T) {
	s := sim(t, mongosim.Options{})
	a := mustOpen(t, baseCfg(nil), s.Addr())
	b := mustOpen(t, baseCfg(nil), s.Addr())
	if a.ID() == b.ID() || b.ID() < a.ID() {
		t.Fatalf("IDs %d %d", a.ID(), b.ID())
	}
	eventually(t, "2 open", func() bool { return s.OpenConns("corral") == 2 })
	a.Close("idle")
	eventually(t, "1 open", func() bool { return s.OpenConns("corral") == 1 })
}

// FR-22: SCRAM-SHA-256 succeeds against the twin; the source is used as $db.
func TestOpenerSCRAM_FR22(t *testing.T) {
	s := sim(t, mongosim.Options{RequireAuth: true})
	c := mustOpen(t, baseCfg(creds()), s.Addr())
	ci := s.Conns()[0]
	if !ci.Authenticated || ci.AuthMechanism != "SCRAM-SHA-256" {
		t.Fatalf("not authenticated: %+v", ci)
	}
	st := s.ReceivedCommand("saslStart")
	if len(st) != 1 || st[0].DB != "admin" {
		t.Fatalf("saslStart: %+v", st)
	}
	// The connection is usable: a ping works on an authenticated connection.
	reply, err := upstreamconn.RoundTrip(context.Background(), c, wire.PingBody())
	if err != nil {
		t.Fatal(err)
	}
	if bson.Raw(reply).Lookup("ok").AsFloat64() != 1 {
		t.Fatal("ping not ok")
	}
	// A custom source database.
	cr := creds()
	cr.Source = "authdb"
	s2 := sim(t, mongosim.Options{})
	mustOpen(t, baseCfg(cr), s2.Addr())
	if m := s2.ReceivedCommand("saslStart"); len(m) != 1 || m[0].DB != "authdb" {
		t.Fatalf("saslStart db: %+v", m)
	}
}

// FR-22: a wrong password is an authentication failure (code 18 from the
// genuine twin server).
func TestOpenerWrongPassword_AC25(t *testing.T) {
	s := sim(t, mongosim.Options{})
	cr := creds()
	cr.Password = []byte("another-synthetic-password")
	_, err := open(t, baseCfg(cr), s.Addr())
	if err == nil || !upstream.IsAuthFailed(err) {
		t.Fatalf("err = %v, want AuthFailed", err)
	}
}

// FR-22 (AC-25 unit): codes 18 and 334 and a bad server signature are
// upstream_auth_failed; 18 and 334 expose the code through errclass.
func TestAuthFailedOutcomes_AC25(t *testing.T) {
	cases := []struct {
		name     string
		outcome  mongosim.AuthOutcome
		wantCode string
	}{
		{"code 18", mongosim.Fail18, "18"},
		{"code 334", mongosim.Fail334, "334"},
		{"code 18 at continue", mongosim.Fail18.AtContinue(), "18"},
		{"bad server signature", mongosim.BadServerSignature, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sim(t, mongosim.Options{})
			s.SetAuthOutcome(tc.outcome)
			_, err := open(t, baseCfg(creds()), s.Addr())
			if err == nil || !upstream.IsAuthFailed(err) {
				t.Fatalf("err = %v, want AuthFailed", err)
			}
			c, d := errclass.Classify(err)
			if c != errclass.ClassServer || d != errclass.DetailUpstreamAuthFailed {
				t.Fatalf("Classify = (%s, %s), want server/upstream_auth_failed", c, d)
			}
			a := attrs(err)
			if tc.wantCode == "" {
				if _, ok := a[attr.DBResponseStatusCode]; ok {
					t.Fatal("a signature failure carries a status code")
				}
			} else if a[attr.DBResponseStatusCode] != tc.wantCode {
				t.Fatalf("status code %q, want %q", a[attr.DBResponseStatusCode], tc.wantCode)
			}
		})
	}
}

// FR-22 (AC-25 unit): everything else is transient, with its own class.
func TestTransientAuthOutcomes_AC25(t *testing.T) {
	cases := []struct {
		name      string
		outcome   mongosim.AuthOutcome
		class     errclass.Class
		wantCode  string
		timeoutMs int
	}{
		{"10107", mongosim.ErrorCode(10107, "NotWritablePrimary"), errclass.ClassServer, "10107", 0},
		{"91", mongosim.ErrorCode(91, "ShutdownInProgress"), errclass.ClassServer, "91", 0},
		{"11600", mongosim.ErrorCode(11600, "InterruptedAtShutdown"), errclass.ClassServer, "11600", 0},
		{"13 at continue", mongosim.ErrorCode(13, "Unauthorized").AtContinue(), errclass.ClassServer, "13", 0},
		{"reset", mongosim.AuthReset, errclass.ClassNetwork, "", 0},
		{"delay past the timeout", mongosim.AuthDelay(2 * time.Second), errclass.ClassTimeout, "", 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sim(t, mongosim.Options{})
			s.SetAuthOutcome(tc.outcome)
			cfg := baseCfg(creds())
			if tc.timeoutMs > 0 {
				cfg.ConnectTimeout = time.Duration(tc.timeoutMs) * time.Millisecond
			}
			begin := time.Now()
			_, err := open(t, cfg, s.Addr())
			if err == nil {
				t.Fatal("Open succeeded")
			}
			if upstream.IsAuthFailed(err) {
				t.Fatalf("%s counted as an authentication failure", tc.name)
			}
			if c, _ := errclass.Classify(err); c != tc.class {
				t.Fatalf("class %q, want %q", c, tc.class)
			}
			if tc.class == errclass.ClassServer {
				if d := func() errclass.Detail { _, d := errclass.Classify(err); return d }(); d != errclass.DetailOther {
					t.Fatalf("detail %q, want other", d)
				}
				if got := attrs(err)[attr.DBResponseStatusCode]; got != tc.wantCode {
					t.Fatalf("status code %q, want %q", got, tc.wantCode)
				}
			}
			if tc.timeoutMs > 0 && time.Since(begin) > time.Duration(tc.timeoutMs)*time.Millisecond+1500*time.Millisecond {
				t.Fatalf("the connect timeout was not enforced (%v)", time.Since(begin))
			}
		})
	}
}

// FR-28: the connect timeout covers connect, hello and authentication.
func TestConnectTimeoutEnforced_FR28(t *testing.T) {
	t.Run("blackholed", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		s.Blackhole(true)
		cfg := baseCfg(nil)
		cfg.ConnectTimeout = 300 * time.Millisecond
		begin := time.Now()
		_, err := open(t, cfg, s.Addr())
		if err == nil {
			t.Fatal("Open succeeded against a black hole")
		}
		if c, _ := errclass.Classify(err); c != errclass.ClassTimeout {
			t.Fatalf("class %q, want timeout", c)
		}
		if time.Since(begin) > 2*time.Second {
			t.Fatalf("took %v", time.Since(begin))
		}
	})
	t.Run("slow hello", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		s.AddRule(mongosim.Match{Command: "hello", AppName: "corral"}, mongosim.Delay(3*time.Second))
		s.AddRule(mongosim.Match{Command: "isMaster", AppName: "corral"}, mongosim.Delay(3*time.Second))
		cfg := baseCfg(nil)
		cfg.ConnectTimeout = 300 * time.Millisecond
		_, err := open(t, cfg, s.Addr())
		if err == nil {
			t.Fatal("Open succeeded")
		}
		if c, _ := errclass.Classify(err); c != errclass.ClassTimeout {
			t.Fatalf("class %q, want timeout", c)
		}
	})
	t.Run("refused", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		addr := s.Addr()
		s.Stop()
		_, err := open(t, baseCfg(nil), addr)
		if c, d := errclass.Classify(err); err == nil || c != errclass.ClassNetwork || d != errclass.DetailECONNREFUSED {
			t.Fatalf("err = %v (%s/%s)", err, c, d)
		}
	})
}

// FR-11, FR-58: a reply that does not match the outstanding request is an
// upstream protocol error with the right detail.
func TestRoundTripProtocolErrors_FR11(t *testing.T) {
	cases := []struct {
		name   string
		action mongosim.Action
		detail errclass.Detail
	}{
		{"unknown responseTo", mongosim.ReplyUnknownResponseTo(), errclass.DetailUnexpectedResponse},
		{"moreToCome", mongosim.ReplyMoreToCome(), errclass.DetailUnexpectedResponse},
		{"oversize", mongosim.ReplyOversize(), errclass.DetailMessageTooLarge},
		{"bad checksum", mongosim.ReplyChecksum(false), errclass.DetailChecksumMismatch},
		{"malformed", mongosim.ReplyMalformed(), errclass.DetailMalformedMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sim(t, mongosim.Options{})
			c := mustOpen(t, baseCfg(nil), s.Addr())
			s.AddRule(mongosim.Match{Command: "ping"}, tc.action)
			_, err := upstreamconn.RoundTrip(context.Background(), c, wire.PingBody())
			if err == nil {
				t.Fatal("RoundTrip succeeded")
			}
			class, d := errclass.Classify(err)
			if class != errclass.ClassProtocol || d != tc.detail {
				t.Fatalf("Classify = (%s, %s), want protocol/%s", class, d, tc.detail)
			}
		})
	}
}

// A valid checksummed reply is accepted.
func TestRoundTripChecksumOK_FR58(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := mustOpen(t, baseCfg(nil), s.Addr())
	s.AddRule(mongosim.Match{Command: "ping"}, mongosim.ReplyChecksum(true))
	reply, err := upstreamconn.RoundTrip(context.Background(), c, wire.PingBody())
	if err != nil || bson.Raw(reply).Lookup("ok").AsFloat64() != 1 {
		t.Fatalf("err=%v", err)
	}
}

// RoundTrip honours the caller's context.
func TestRoundTripContext_FR28(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := mustOpen(t, baseCfg(nil), s.Addr())
	h := s.AddRule(mongosim.Match{Command: "ping"}, mongosim.Hold())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := upstreamconn.RoundTrip(ctx, c, wire.PingBody())
	h.Release()
	if err == nil {
		t.Fatal("RoundTrip returned no error for an expired context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		if cl, _ := errclass.Classify(err); cl != errclass.ClassTimeout {
			t.Fatalf("class %q", cl)
		}
	}
}

// FR-24, SPEC-001 FR-24: neither the username nor the password appears in any
// error text, for any failing outcome.
func TestNoCredentialsInErrors_FR24(t *testing.T) {
	outcomes := []mongosim.AuthOutcome{mongosim.Fail18, mongosim.Fail334, mongosim.BadServerSignature, mongosim.AuthReset,
		mongosim.ErrorCode(10107, "NotWritablePrimary"), mongosim.Fail18.AtContinue()}
	for i, o := range outcomes {
		s := sim(t, mongosim.Options{})
		s.SetAuthOutcome(o)
		_, err := open(t, baseCfg(creds()), s.Addr())
		if err == nil {
			t.Fatalf("outcome %d: no error", i)
		}
		txt := fmt.Sprintf("%v | %+v | %#v | %s", err, err, err, err)
		for _, m := range []string{user, pass} {
			if strings.Contains(txt, m) {
				t.Fatalf("outcome %d: a credential marker appears in the error text", i)
			}
		}
		for k, v := range attrs(err) {
			if strings.Contains(v, user) || strings.Contains(v, pass) {
				t.Fatalf("outcome %d: marker in log attribute %s", i, k)
			}
		}
	}
	// And on a wrong password.
	s := sim(t, mongosim.Options{})
	cr := creds()
	cr.Password = []byte(pass + "-wrong")
	_, err := open(t, baseCfg(cr), s.Addr())
	if err == nil || strings.Contains(fmt.Sprintf("%v %+v", err, err), pass) {
		t.Fatalf("err = %v", err)
	}
}
