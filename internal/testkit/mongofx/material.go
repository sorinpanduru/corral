package mongofx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// Auth holds synthetic credentials generated at runtime. Nothing is committed
// or reused between calls.
type Auth struct {
	RootUser, RootPassword       string
	ServiceUser, ServicePassword string
	Keyfile                      []byte
}

func randHex(tb testing.TB, n int) string {
	tb.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// NewAuth returns random synthetic credentials: a root user, a service user
// (readWrite on the test databases only) and a replica-set keyfile.
func NewAuth(tb testing.TB) *Auth {
	tb.Helper()
	kf := make([]byte, 48)
	if _, err := rand.Read(kf); err != nil {
		tb.Fatal(err)
	}
	return &Auth{
		RootUser:        "fx-root-" + randHex(tb, 4),
		RootPassword:    randHex(tb, 16),
		ServiceUser:     "fx-svc-" + randHex(tb, 4),
		ServicePassword: randHex(tb, 16),
		Keyfile:         []byte(base64.StdEncoding.EncodeToString(kf)), // 64 chars, within mongod's 6..1024
	}
}

// TLSMaterial is a runtime-generated CA with a server and a client certificate.
type TLSMaterial struct {
	CAPEM         []byte
	ServerPEM     []byte // certificate and key, for mongod's certificate key file
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
}

// NewTLSMaterial generates a CA, a server certificate for 127.0.0.1 and
// localhost, and a client certificate. All are short-lived and synthetic.
func NewTLSMaterial(tb testing.TB) *TLSMaterial {
	tb.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corral-fx-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		tb.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		tb.Fatal(err)
	}
	issue := func(serial int64, cn string) (certPEM, keyPEM []byte) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			tb.Fatal(err)
		}
		t := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			DNSNames:    []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, t, caCert, &k.PublicKey, caKey)
		if err != nil {
			tb.Fatal(err)
		}
		kb, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			tb.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
	}
	m := &TLSMaterial{CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
	m.ServerCertPEM, m.ServerKeyPEM = issue(2, "corral-fx-server")
	m.ServerPEM = append(append([]byte(nil), m.ServerCertPEM...), m.ServerKeyPEM...)
	m.ClientCertPEM, m.ClientKeyPEM = issue(3, "corral-fx-client")
	return m
}
