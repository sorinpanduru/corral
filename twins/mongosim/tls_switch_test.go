package mongosim_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func TestSetTLSAppliesToNewConnectionsOnly(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	nc, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := &raw{t: t, nc: nc}
	if old.cmd(helloCmd("old")).doc.Lookup("ok").Double() != 1 {
		t.Fatal("hello")
	}

	for _, tc := range []struct {
		mode mongosim.TLSMode
		name string
		chk  func(error) bool
	}{
		{mongosim.TLSExpired, "127.0.0.1", func(e error) bool {
			var ci x509.CertificateInvalidError
			return errors.As(e, &ci) && ci.Reason == x509.Expired
		}},
		{mongosim.TLSHostnameMismatch, "127.0.0.1", func(e error) bool { var h x509.HostnameError; return errors.As(e, &h) }},
		{mongosim.TLSLegacyVersion, "127.0.0.1", func(e error) bool { return e != nil }},
	} {
		s.SetTLS(tc.mode)
		if s.TLSMode() != tc.mode {
			t.Fatal("TLSMode not updated")
		}
		if _, err := tlsDial(t, s, tc.name, tls.VersionTLS12, 0, nil); !tc.chk(err) {
			t.Fatalf("mode %v: unexpected %v", tc.mode, err)
		}
		// The connection opened before the switch still works.
		if old.cmd(helloCmd("")).doc.Lookup("ok").Double() != 1 {
			t.Fatalf("existing connection broken by switch to %v", tc.mode)
		}
	}
	s.SetTLS(mongosim.TLSValid)
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatalf("back to valid: %v", err)
	}
}

func TestSetTLSPlaintextToTLSAndBack(t *testing.T) {
	a := mongosim.NewAuthority(t)
	s := mongosim.Start(t, mongosim.Options{Authority: a})
	if s.CAPEM() == nil || s.TLSMode() != mongosim.TLSOff {
		t.Fatal("authority should be available while plaintext")
	}
	plain := dial(t, s.Addr())
	if plain.cmd(helloCmd("")).doc.Lookup("ok").Double() != 1 {
		t.Fatal("plaintext hello")
	}
	s.SetTLS(mongosim.TLSValid)
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatal(err)
	}
	if plain.cmd(helloCmd("")).doc.Lookup("ok").Double() != 1 {
		t.Fatal("existing plaintext connection must survive")
	}
	s.SetTLS(mongosim.TLSOff)
	if dial(t, s.Addr()).cmd(helloCmd("")).doc.Lookup("ok").Double() != 1 {
		t.Fatal("plaintext after TLSOff")
	}
}

func TestSetTLSCreatesAuthorityWhenStartedWithout(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	s.SetTLS(mongosim.TLSValid)
	if s.CAFile() == "" {
		t.Fatal("no CA after SetTLS")
	}
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTLSHandshakeRecord(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	before := time.Now()
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatal(err)
	}
	s.SetTLS(mongosim.TLSExpired)
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err == nil {
		t.Fatal("want failure")
	}
	dial(t, s.Addr()).sendMsg(0, helloCmd("")) // plaintext against TLS: failed handshake
	if !s.WaitTLSHandshakes(2*time.Second, func(h []mongosim.TLSHandshake) bool { return len(h) == 3 }) {
		t.Fatalf("records: %+v", s.TLSHandshakes())
	}
	h := s.TLSHandshakes()
	if !h[0].OK || h[0].Err != "" || h[0].Version < tls.VersionTLS12 || h[0].Mode != mongosim.TLSValid {
		t.Fatalf("first: %+v", h[0])
	}
	if h[0].Start.Before(before) || h[0].End.Before(h[0].Start) || h[0].Remote == "" {
		t.Fatalf("times/remote: %+v", h[0])
	}
	// Order of the two failures depends on completion; both must be failures.
	for _, r := range h[1:] {
		if r.OK || r.Err == "" || r.End.Before(r.Start) {
			t.Fatalf("failure record: %+v", r)
		}
	}
	if s.TLSHandshakeFailures() != 2 {
		t.Fatalf("failures = %d", s.TLSHandshakeFailures())
	}
	s.ResetTLSHandshakes()
	if len(s.TLSHandshakes()) != 0 {
		t.Fatal("reset")
	}
}

func TestStallNextHelloIsPerConnectionAndOneShot(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	a, b := dial(t, s.Addr()), dial(t, s.Addr())
	a.cmd(helloCmd("a"))
	b.cmd(helloCmd("b"))
	if err := s.StallNextHello(0, 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	b.cmd(helloCmd("b"))
	if time.Since(t0) > 200*time.Millisecond {
		t.Fatal("other connection was stalled")
	}
	t0 = time.Now()
	a.cmd(helloCmd("a"))
	if d := time.Since(t0); d < 350*time.Millisecond {
		t.Fatalf("stall too short: %v", d)
	}
	t0 = time.Now()
	a.cmd(helloCmd("a"))
	if time.Since(t0) > 200*time.Millisecond {
		t.Fatal("stall must be one-shot")
	}
	// A timed-out client can still stall: read with a short deadline.
	if err := s.StallNextHello(0, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	a.sendMsg(0, helloCmd("a"))
	a.short = 100 * time.Millisecond
	if _, err := a.read(); err == nil {
		t.Fatal("expected read timeout while stalled")
	}
	if s.StallNextHello(99, time.Second) == nil {
		t.Fatal("unknown connection must error")
	}
}
