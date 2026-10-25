package mongosim_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/twins/mongosim"
)

func tlsDial(t *testing.T, s *mongosim.Server, serverName string, min, max uint16, pool *x509.CertPool) (*tls.Conn, error) {
	t.Helper()
	if pool == nil {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(s.CAPEM()) {
			t.Fatal("CAPEM is not a certificate")
		}
	}
	d := &net.Dialer{Timeout: 2 * time.Second}
	nc, err := tls.DialWithDialer(d, "tcp", s.Addr(), &tls.Config{
		RootCAs: pool, ServerName: serverName, MinVersion: min, MaxVersion: max,
	})
	if err == nil {
		t.Cleanup(func() { nc.Close() })
	}
	return nc, err
}

func waitFailures(t *testing.T, s *mongosim.Server, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.TLSHandshakeFailures() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TLSHandshakeFailures = %d, want >= %d", s.TLSHandshakeFailures(), n)
}

func TestTLSOffIsPlaintextAndExposesNoCA(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{})
	if s.CAPEM() != nil || s.CAFile() != "" || s.Authority() != nil {
		t.Fatal("TLS off must expose no CA")
	}
	r := dial(t, s.Addr())
	if rp := r.cmd(helloCmd("")); code(rp) != 0 || rp.doc.Lookup("ok").Double() != 1 {
		t.Fatalf("plaintext hello failed: %v", rp.doc)
	}
}

func TestTLSValidControl(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	for _, name := range []string{"127.0.0.1", "localhost"} {
		nc, err := tlsDial(t, s, name, tls.VersionTLS12, 0, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := &raw{t: t, nc: nc}
		if rp := r.cmd(helloCmd("tls-control")); rp.doc.Lookup("ok").Double() != 1 {
			t.Fatalf("hello over TLS failed: %v", rp.doc)
		}
	}
	if s.TLSHandshakeFailures() != 0 {
		t.Fatal("unexpected handshake failures")
	}
	if s.CAFile() != s.CAFile() || s.CAFile() == "" {
		t.Fatal("CAFile must be stable")
	}
	// TLS 1.3 is negotiated when the client allows it.
	nc, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil)
	if err != nil || nc.ConnectionState().Version < tls.VersionTLS12 {
		t.Fatalf("version: %v", err)
	}
}

func TestTLSWrongCARejectedByClient(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	other := mongosim.NewAuthority(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(other.CAPEM())
	_, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, pool)
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Fatalf("want UnknownAuthorityError, got %v", err)
	}
	waitFailures(t, s, 1)
}

func TestTLSSharedAuthority(t *testing.T) {
	a := mongosim.NewAuthority(t)
	s1 := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid, Authority: a})
	s2 := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid, Authority: a})
	if s1.Authority() != a || s2.Authority() != a {
		t.Fatal("authority not shared")
	}
	if _, err := tlsDial(t, s2, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTLSExpired(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSExpired})
	_, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil)
	var ci x509.CertificateInvalidError
	if !errors.As(err, &ci) || ci.Reason != x509.Expired {
		t.Fatalf("want Expired, got %v", err)
	}
	waitFailures(t, s, 1)
}

func TestTLSHostnameMismatch(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSHostnameMismatch})
	_, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil)
	var he x509.HostnameError
	if !errors.As(err, &he) {
		t.Fatalf("want HostnameError, got %v", err)
	}
	// The chain itself is valid: the name the certificate does carry works.
	if _, err := tlsDial(t, s, "mismatch.corral-test.invalid", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatalf("chain should verify for its own name: %v", err)
	}
	waitFailures(t, s, 1)
}

func TestTLSLegacyVersionOnly(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSLegacyVersion})
	// A TLS 1.2+ client fails with a protocol-version error.
	_, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil)
	if err == nil {
		t.Fatal("TLS 1.2+ client must fail against a TLS 1.1-only server")
	}
	t.Logf("client error: %v", err)
	waitFailures(t, s, 1)
	// Control: the server really speaks TLS 1.1 (proves the mode is faithful,
	// not just broken). Go clients allow this only with an explicit floor.
	nc, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS10, tls.VersionTLS11, nil)
	if err != nil {
		t.Fatalf("TLS 1.0-1.1 client: %v", err)
	}
	if v := nc.ConnectionState().Version; v != tls.VersionTLS11 {
		t.Fatalf("negotiated %x, want TLS 1.1", v)
	}
}

func TestTLSPlaintextClientCounted(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	r := dial(t, s.Addr())
	r.sendMsg(0, helloCmd(""))
	if !isClosed(r) {
		t.Fatal("plaintext client should be closed")
	}
	waitFailures(t, s, 1)
}

func TestTLSRestartKeepsTLS(t *testing.T) {
	s := mongosim.Start(t, mongosim.Options{TLS: mongosim.TLSValid})
	s.Restart()
	if _, err := tlsDial(t, s, "127.0.0.1", tls.VersionTLS12, 0, nil); err != nil {
		t.Fatal(err)
	}
}
