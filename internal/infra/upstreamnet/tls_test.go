package upstreamnet_test

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// tlsServer runs a TLS echo-once server on loopback with the given config and
// returns its address. Handshake errors are reported on errc.
func tlsServer(t *testing.T, cfg *tls.Config) (addr string, errc chan error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	errc = make(chan error, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				if err := c.(*tls.Conn).Handshake(); err != nil {
					errc <- err
					return
				}
				errc <- nil
				buf := make([]byte, 4)
				if n, _ := c.Read(buf); n > 0 {
					c.Write(buf[:n])
				}
			}()
		}
	}()
	return ln.Addr().String(), errc
}

func dialTLS(addr string, cfg *tls.Config) (*tls.Conn, error) {
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(raw, cfg)
	if err := c.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// AC-24, FR-41: ClientTLS enforces TLS 1.2+, always verifies and sets the
// server name.
func TestClientTLSConfig_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	cfg, err := upstreamnet.ClientTLS(ca.CAPEM, "mongo.test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want >= TLS 1.2", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set")
	}
	if cfg.VerifyPeerCertificate != nil || cfg.VerifyConnection != nil {
		t.Error("custom verification hooks must not replace standard verification")
	}
	if cfg.ServerName != "mongo.test" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs not set from the CA bytes")
	}
}

// caPEM == nil means the system roots (RootCAs nil), still verifying.
func TestClientTLSSystemRoots_AC24(t *testing.T) {
	cfg, err := upstreamnet.ClientTLS(nil, "mongo.test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs != nil || cfg.InsecureSkipVerify || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("system-roots config wrong: RootCAs nil=%v insecure=%v min=%#x", cfg.RootCAs == nil, cfg.InsecureSkipVerify, cfg.MinVersion)
	}
}

// Unusable CA bytes are an error that does not echo the input.
func TestClientTLSBadCA_AC24(t *testing.T) {
	marker := "SYNTHETIC-NOT-PEM-91c2"
	for name, b := range map[string][]byte{"garbage": []byte(marker), "empty non-nil": {}, "wrong block": []byte("-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----\n")} {
		t.Run(name, func(t *testing.T) {
			_, err := upstreamnet.ClientTLS(b, "x")
			if err == nil {
				t.Fatal("no error for an unusable CA")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("error contains input bytes")
			}
		})
	}
}

// AC-24: a server with a certificate from the right CA connects.
func TestTLSHandshakeSucceeds_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	l := ca.issue(t, 2, "mongo.test")
	scfg, err := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := tlsServer(t, scfg)
	ccfg, _ := upstreamnet.ClientTLS(ca.CAPEM, "mongo.test")
	c, err := dialTLS(addr, ccfg)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer c.Close()
	if v := c.ConnectionState().Version; v < tls.VersionTLS12 {
		t.Fatalf("negotiated version %#x", v)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if n, _ := c.Read(buf); string(buf[:n]) != "ping" {
		t.Fatal("echo failed")
	}
}

// AC-24: the wrong CA fails and classifies as network/tls_handshake.
func TestTLSWrongCA_AC24(t *testing.T) {
	ca, other := newPKI(t, "ca"), newPKI(t, "other-ca")
	l := ca.issue(t, 2, "mongo.test")
	scfg, _ := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	addr, _ := tlsServer(t, scfg)
	ccfg, _ := upstreamnet.ClientTLS(other.CAPEM, "mongo.test")
	_, err := dialTLS(addr, ccfg)
	if err == nil {
		t.Fatal("handshake with the wrong CA succeeded")
	}
	if c, d := errclass.Classify(err); c != errclass.ClassNetwork || d != errclass.DetailTLSHandshake {
		t.Fatalf("Classify = (%s, %s), want network/tls_handshake", c, d)
	}
}

// AC-24: a host-name mismatch fails.
func TestTLSHostnameMismatch_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	l := ca.issue(t, 2, "mongo.test")
	scfg, _ := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	addr, _ := tlsServer(t, scfg)
	ccfg, _ := upstreamnet.ClientTLS(ca.CAPEM, "other.test")
	_, err := dialTLS(addr, ccfg)
	if err == nil {
		t.Fatal("handshake with a mismatched host name succeeded")
	}
	if c, d := errclass.Classify(err); c != errclass.ClassNetwork || d != errclass.DetailTLSHandshake {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// AC-24: TLS 1.1 is refused (client and server sides).
func TestTLSRefusesOldVersions_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	l := ca.issue(t, 2, "mongo.test")
	scfg, err := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("server MinVersion = %#x", scfg.MinVersion)
	}
	addr, _ := tlsServer(t, scfg)
	old, _ := upstreamnet.ClientTLS(ca.CAPEM, "mongo.test")
	old = old.Clone()
	old.MinVersion = tls.VersionTLS10
	old.MaxVersion = tls.VersionTLS11
	if _, err := dialTLS(addr, old); err == nil {
		t.Fatal("TLS 1.1 handshake against ServerTLS succeeded")
	}
}

// ServerTLS with client CA bytes requires and verifies client certificates.
func TestServerTLSClientCerts_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	other := newPKI(t, "other")
	srv := ca.issue(t, 2, "mongo.test")
	good := ca.issue(t, 3, "client.test")
	bad := other.issue(t, 4, "client.test")
	scfg, err := upstreamnet.ServerTLS(srv.CertPEM, srv.KeyPEM, ca.CAPEM)
	if err != nil {
		t.Fatal(err)
	}
	if scfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v", scfg.ClientAuth)
	}
	addr, _ := tlsServer(t, scfg)
	base, _ := upstreamnet.ClientTLS(ca.CAPEM, "mongo.test")

	// No client certificate: refused. TLS 1.3 reports the failure on first
	// read, so exercise a round trip.
	c, err := dialTLS(addr, base)
	if err == nil {
		c.Write([]byte("ping"))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if n, rerr := c.Read(make([]byte, 4)); rerr == nil {
			t.Fatalf("server served a client without a certificate (%d bytes)", n)
		}
		c.Close()
	}

	withBad := base.Clone()
	kp, err := tls.X509KeyPair(bad.CertPEM, bad.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	withBad.Certificates = []tls.Certificate{kp}
	c, err = dialTLS(addr, withBad)
	if err == nil {
		c.Write([]byte("ping"))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, rerr := c.Read(make([]byte, 4)); rerr == nil {
			t.Fatal("server accepted a client certificate from another CA")
		}
		c.Close()
	}

	withGood := base.Clone()
	kp, err = tls.X509KeyPair(good.CertPEM, good.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	withGood.Certificates = []tls.Certificate{kp}
	c, err = dialTLS(addr, withGood)
	if err != nil {
		t.Fatalf("valid client certificate refused: %v", err)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := c.Read(buf); string(buf[:n]) != "ping" {
		t.Fatal("round trip with a valid client certificate failed")
	}
}

// Without client CA bytes no client certificate is requested.
func TestServerTLSNoClientCA_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	l := ca.issue(t, 2, "mongo.test")
	cfg, err := upstreamnet.ServerTLS(l.CertPEM, l.KeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
}

// Bad key pairs and bad client CA bytes are errors that do not echo input.
func TestServerTLSBadInputs_AC24(t *testing.T) {
	ca := newPKI(t, "ca")
	a, b := ca.issue(t, 2, "a.test"), ca.issue(t, 3, "b.test")
	marker := "SYNTHETIC-KEY-MARKER-55e1"
	cases := map[string]struct{ cert, key, ca []byte }{
		"mismatched pair":   {a.CertPEM, b.KeyPEM, nil},
		"garbage key":       {a.CertPEM, []byte(marker), nil},
		"garbage cert":      {[]byte(marker), a.KeyPEM, nil},
		"nil inputs":        {nil, nil, nil},
		"garbage client ca": {a.CertPEM, a.KeyPEM, []byte(marker)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := upstreamnet.ServerTLS(c.cert, c.key, c.ca)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Fatal("error contains input bytes")
			}
		})
	}
}
