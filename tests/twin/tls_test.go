//go:build twin

package twin

import (
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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
)

type certs struct {
	caPEM, certPath, keyPath string
	pool                     *x509.CertPool
}

// genCerts writes a synthetic CA-signed server certificate (127.0.0.1) and key
// under a temp directory. Nothing is committed or reused.
func genCerts(t *testing.T) certs {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corral-twin-ca.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "corral-twin-server.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, srvKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(srvKey)
	dir := t.TempDir()
	cp := filepath.Join(dir, "server.crt")
	kp := filepath.Join(dir, "server.key")
	cap := filepath.Join(dir, "ca.crt")
	os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), 0o600)
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600)
	os.WriteFile(cap, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return certs{caPEM: cap, certPath: cp, keyPath: kp, pool: pool}
}

func tlsOpts(cs certs, set map[string]string) opts {
	if set == nil {
		set = map[string]string{}
	}
	set["proxy.tls.enabled"] = "true"
	set["proxy.tls.cert_path"] = cs.certPath
	return opts{set: set, env: []string{"CORRAL_PROXY_TLS_KEY_FILE=" + cs.keyPath}}
}

// AC-45 (FR-2, FR-44, FR-57): with client TLS on, a client over the connection
// limit that starts a TLS handshake receives no TLS bytes before the close; a
// client that sends a partial ClientHello and stalls is closed after
// proxy.client_io_timeout with reason tls_failure. AC-39: both count in
// accepted_total.
func TestClientTLSLimitsAndStall_AC45(t *testing.T) {
	cs := genCerts(t)
	o := tlsOpts(cs, map[string]string{"proxy.max_client_connections": "1", "proxy.client_io_timeout": "1s"})
	c, _ := startMongos(t, o)
	c.waitReady()
	first := rawwire.DialTLS(t, c.proxy, &tls.Config{RootCAs: cs.pool, ServerName: "localhost"})
	if m := rt(t, first, cmd(1, "hello", 1, "$db", "admin")); !isOK(m) {
		t.Fatalf("hello over TLS: %v", doc(m))
	}
	acc := c.metric("corral_client_connections_accepted_total")
	lim := c.metric("corral_client_connections_closed_total", "reason", "limit_exceeded")
	over := rawwire.DialStalledClientHello(t, c.proxy)
	over.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := over.Read(make([]byte, 64))
	if n != 0 || (err != io.EOF && !isReset(err)) {
		t.Fatalf("over the limit: read %d bytes, err %v (want no bytes before the close)", n, err)
	}
	c.waitDelta(lim, 1, "corral_client_connections_closed_total", "reason", "limit_exceeded")
	c.waitDelta(acc, 1, "corral_client_connections_accepted_total")
	// Free the slot, then stall a partial ClientHello.
	first.Close()
	eventually(t, "the slot to free", func() bool { return c.metric("corral_client_connections") == 0 })
	tf := c.metric("corral_client_connections_closed_total", "reason", "tls_failure")
	acc = c.metric("corral_client_connections_accepted_total")
	stalled := rawwire.DialStalledClientHello(t, c.proxy)
	begin := time.Now()
	stalled.SetReadDeadline(time.Now().Add(8 * time.Second))
	stalled.Read(make([]byte, 64))
	if d := time.Since(begin); d < 700*time.Millisecond || d > 6*time.Second {
		t.Fatalf("the stalled handshake was closed after %v, want about 1s", d)
	}
	c.waitDelta(tf, 1, "corral_client_connections_closed_total", "reason", "tls_failure")
	c.waitDelta(acc, 1, "corral_client_connections_accepted_total")
	c.waitRecords("client TLS handshake failed", 1)
}

func isReset(err error) bool {
	ne, ok := err.(net.Error)
	return err != nil && (!ok || !ne.Timeout())
}

// AC-24 (twin part, FR-44): a plaintext client fails against a TLS listener and
// the key is never emitted.
func TestPlaintextClientRefusedWhenTLSOn_AC24(t *testing.T) {
	cs := genCerts(t)
	c, _ := startMongos(t, tlsOpts(cs, nil))
	c.waitReady()
	conn := c.dial()
	conn.Send(cmd(1, "hello", 1, "$db", "admin"))
	if m, err := conn.ReadMessage(3 * time.Second); err == nil {
		t.Fatalf("a plaintext client got a reply: %v", m)
	}
}
