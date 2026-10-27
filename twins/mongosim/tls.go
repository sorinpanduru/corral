package mongosim

// Optional upstream TLS for the simulated server. All key material is
// generated in memory at test time with synthetic names and is never written
// anywhere except a per-test temporary directory (CAFile). Nothing is
// committed. No real credentials or production data are involved.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TLSMode selects what the server presents on its listener.
type TLSMode int

const (
	// TLSOff serves plaintext (the default, unchanged behaviour).
	TLSOff TLSMode = iota
	// TLSValid presents a certificate signed by the Authority, valid now,
	// with SANs for 127.0.0.1, ::1 and "localhost". TLS 1.2 and 1.3.
	TLSValid
	// TLSExpired presents a certificate signed by the Authority whose
	// validity ended 24 hours ago. Otherwise as TLSValid.
	TLSExpired
	// TLSHostnameMismatch presents a currently valid certificate signed by
	// the Authority whose only SAN is "mismatch.corral-test.invalid", so
	// dialling 127.0.0.1 or localhost fails host-name verification.
	TLSHostnameMismatch
	// TLSLegacyVersion presents a valid certificate but negotiates only
	// TLS 1.0 and 1.1 (MaxVersion TLS 1.1). A client demanding TLS 1.2 or
	// later fails the handshake with a protocol-version alert.
	TLSLegacyVersion
)

// Authority is a throwaway test CA with a synthetic name.
type Authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	dir  string
	file string
	tb   testing.TB
}

// NewAuthority generates a fresh CA ("corral-test-ca").
func NewAuthority(tb testing.TB) *Authority {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("mongosim: ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(tb),
		Subject:               pkix.Name{CommonName: "corral-test-ca", Organization: []string{"corral-test"}},
		NotBefore:             time.Now().Add(-72 * time.Hour),
		NotAfter:              time.Now().Add(72 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("mongosim: ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("mongosim: ca parse: %v", err)
	}
	return &Authority{
		cert: cert, key: key, tb: tb,
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func serial(tb testing.TB) *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		tb.Fatalf("mongosim: serial: %v", err)
	}
	return n
}

// CAPEM is the CA certificate in PEM (public; the CA key is never exposed).
func (a *Authority) CAPEM() []byte { return append([]byte(nil), a.pem...) }

// CAFile writes the CA certificate to a file in a per-test temporary
// directory (removed by the test framework) and returns its path, suitable
// for upstream.tls.ca_path (CORRAL_UPSTREAM_TLS_CA_PATH). Repeated calls
// return the same file.
func (a *Authority) CAFile() string {
	a.tb.Helper()
	if a.file == "" {
		a.dir = a.tb.TempDir()
		a.file = filepath.Join(a.dir, "corral-test-ca.pem")
		if err := os.WriteFile(a.file, a.pem, 0o600); err != nil {
			a.tb.Fatalf("mongosim: write ca: %v", err)
		}
	}
	return a.file
}

func (a *Authority) serverConfig(tb testing.TB, mode TLSMode) *tls.Config {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("mongosim: server key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(tb),
		Subject:      pkix.Name{CommonName: "corral-test-mongosim"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	switch mode {
	case TLSExpired:
		tmpl.NotBefore = time.Now().Add(-72 * time.Hour)
		tmpl.NotAfter = time.Now().Add(-24 * time.Hour)
	case TLSHostnameMismatch:
		tmpl.DNSNames = []string{"mismatch.corral-test.invalid"}
		tmpl.IPAddresses = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		tb.Fatalf("mongosim: server cert: %v", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
	if mode == TLSLegacyVersion {
		// Go still honours an explicit MinVersion below 1.2 on the server.
		cfg.MinVersion = tls.VersionTLS10
		cfg.MaxVersion = tls.VersionTLS11
	}
	return cfg
}

// Authority returns the CA that signs this server's certificate, or nil when
// TLS is off.
func (s *Server) Authority() *Authority { return s.auth }

// CAPEM is shorthand for Authority().CAPEM(); nil when TLS is off.
func (s *Server) CAPEM() []byte {
	if s.auth == nil {
		return nil
	}
	return s.auth.CAPEM()
}

// CAFile is shorthand for Authority().CAFile(); "" when TLS is off.
func (s *Server) CAFile() string {
	if s.auth == nil {
		return ""
	}
	return s.auth.CAFile()
}

// TLSHandshakeFailures counts server-side handshakes that did not complete
// (client rejected the certificate or version, plaintext client, stall > 5s).
func (s *Server) TLSHandshakeFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsFailures
}

// SetTLS switches the TLS mode for connections accepted from now on; existing
// connections are unaffected. TLSOff goes back to plaintext, and a server that
// started plaintext can move to any TLS mode (the Authority is created on first
// need, or the one passed in Options.Authority is used). Certificates are
// generated in memory when the mode is set.
func (s *Server) SetTLS(mode TLSMode) {
	s.tb.Helper()
	if mode == TLSOff {
		s.mu.Lock()
		s.tlsCfg, s.tlsMode = nil, TLSOff
		s.mu.Unlock()
		return
	}
	// Key generation can call tb.Fatal, so do it before taking s.mu.
	s.mu.Lock()
	auth := s.auth
	s.mu.Unlock()
	if auth == nil {
		auth = NewAuthority(s.tb)
	}
	cfg := auth.serverConfig(s.tb, mode)
	s.mu.Lock()
	if s.auth == nil {
		s.auth = auth
	} else if s.auth != auth {
		// Another SetTLS installed an authority meanwhile; keep one CA.
		auth = s.auth
		s.mu.Unlock()
		cfg = auth.serverConfig(s.tb, mode)
		s.mu.Lock()
	}
	s.tlsCfg, s.tlsMode = cfg, mode
	s.mu.Unlock()
}

// TLSMode returns the mode applied to new connections.
func (s *Server) TLSMode() TLSMode { s.mu.Lock(); defer s.mu.Unlock(); return s.tlsMode }

// TLSHandshake is one server-side TLS handshake attempt. Plaintext
// connections have no handshake and are not recorded.
type TLSHandshake struct {
	ConnIndex int
	Remote    string    // client address (loopback, synthetic)
	Start     time.Time // handshake began (first read on the connection)
	End       time.Time // handshake finished or failed
	OK        bool
	Err       string // empty when OK; the server-side error text
	Version   uint16 // negotiated tls.Version* when OK
	Mode      TLSMode
}

// TLSHandshakes returns a copy of every handshake attempt so far, in the order
// they finished, including failed ones.
func (s *Server) TLSHandshakes() []TLSHandshake {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TLSHandshake(nil), s.hsLog...)
}

// WaitTLSHandshakes polls until pred is true for the recorded handshakes or the
// timeout passes.
func (s *Server) WaitTLSHandshakes(timeout time.Duration, pred func([]TLSHandshake) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if pred(s.TLSHandshakes()) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ResetTLSHandshakes clears the handshake record (the failure counter stays).
func (s *Server) ResetTLSHandshakes() { s.mu.Lock(); s.hsLog = nil; s.mu.Unlock() }
