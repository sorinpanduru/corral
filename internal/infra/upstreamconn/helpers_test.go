package upstreamconn_test

// In-process tests of the upstream connector against twins/mongosim.
//
// Interface assumptions beyond the plan's text (stated in the test-writer
// report):
//   - upstreamconn.ErrUnsolicited is the sentinel returned by CheckIdle for
//     unsolicited bytes; it has CorralErrorDetail() "unexpected_response".
//   - Idle watching is a method pair on the connection:
//     (*Conn).StartIdleWatch(onFault func(error)) and (*Conn).StopIdleWatch().
//   - RoundTrip takes and returns the BSON document of a single kind-0 section.
//   - Conn.Close takes a pool.CloseReason (a string type).
//   - upstream.Report.AuthErr carries authentication outcomes; transient
//     authentication errors are never upstream.AuthFailed.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

const (
	user = "svc-user-marker-7a1"
	pass = "svc-pass-marker-93c"
)

func creds() *upstreamconn.Credentials {
	return &upstreamconn.Credentials{Mechanism: "SCRAM-SHA-256", Source: "admin", Username: []byte(user), Password: []byte(pass)}
}

func baseCfg(c *upstreamconn.Credentials) upstreamconn.Config {
	return upstreamconn.Config{Version: "9.8.7", Credentials: c, ConnectTimeout: 2 * time.Second, Clock: clock.System()}
}

func sim(t *testing.T, o mongosim.Options) *mongosim.Server {
	t.Helper()
	if o.Users == nil {
		o.Users = map[string]string{user: pass}
	}
	return mongosim.Start(t, o)
}

func open(t *testing.T, cfg upstreamconn.Config, host string) (*upstreamconn.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := upstreamconn.NewOpener(cfg).Open(ctx, host)
	if err == nil {
		t.Cleanup(func() { c.Close("shutdown") })
	}
	return c, err
}

func mustOpen(t *testing.T, cfg upstreamconn.Config, host string) *upstreamconn.Conn {
	t.Helper()
	c, err := open(t, cfg, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- TLS front for the twin (the twin itself speaks plain TCP) ----

type tlsFront struct {
	Addr  string
	CAPEM []byte
}

func newTLSFront(t *testing.T, backend string) *tlsFront {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "synthetic-leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				b, err := net.Dial("tcp", backend)
				if err != nil {
					c.Close()
					return
				}
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); io.Copy(b, c); b.Close() }()
				go func() { defer wg.Done(); io.Copy(c, b); c.Close() }()
				wg.Wait()
			}()
		}
	}()
	return &tlsFront{Addr: ln.Addr().String(), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
}

func (f *tlsFront) cfg() upstreamconn.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.CAPEM)
	c := baseCfg(nil)
	c.TLS = func(host string) (*tls.Config, error) {
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "localhost"}, nil
	}
	return c
}

// recorder is an upstream.Observer that records monitoring events.
type recorder struct {
	upstream.Noop
	mu     sync.Mutex
	opened []string
	closed []string
	failed []error
	authOK []string
	authKO []string
}

func (r *recorder) MonitorConnOpened(ctx context.Context, host string, id uint64) {
	r.mu.Lock()
	r.opened = append(r.opened, host)
	r.mu.Unlock()
}
func (r *recorder) MonitorConnClosed(ctx context.Context, host string, reason string) {
	r.mu.Lock()
	r.closed = append(r.closed, reason)
	r.mu.Unlock()
}
func (r *recorder) MonitorConnFailed(ctx context.Context, host string, err error) {
	r.mu.Lock()
	r.failed = append(r.failed, err)
	r.mu.Unlock()
}
func (r *recorder) AuthSucceeded(ctx context.Context, kind, host string) {
	r.mu.Lock()
	r.authOK = append(r.authOK, kind)
	r.mu.Unlock()
}
func (r *recorder) AuthFailed(ctx context.Context, kind, host string, err error) {
	r.mu.Lock()
	r.authKO = append(r.authKO, kind)
	r.mu.Unlock()
}
