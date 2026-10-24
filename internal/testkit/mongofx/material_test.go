package mongofx_test

// Unit self-tests for the parts of mongofx that need no Docker: pinned images,
// runtime-generated credentials and TLS material. The API fixed here:
//
//	mongofx.Versions() []string
//	mongofx.Image(version string) string      // pinned by digest
//	mongofx.NewAuth(t) *Auth                  // RootUser, RootPassword, ServiceUser,
//	                                          // ServicePassword, Keyfile []byte
//	mongofx.NewTLSMaterial(t) *TLSMaterial    // CAPEM, ServerPEM (cert+key, for
//	                                          // mongod), ServerCertPEM, ServerKeyPEM,
//	                                          // ClientCertPEM, ClientKeyPEM
//
// All values are synthetic and generated per call. Nothing is committed.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
)

var digestRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// ADR-0022: images are pinned by digest for MongoDB 7.0 and 8.0.
func TestImagesPinnedByDigest_ADR0022(t *testing.T) {
	vs := mongofx.Versions()
	if len(vs) != 2 || vs[0] != "7.0" || vs[1] != "8.0" {
		t.Fatalf("Versions() = %v, want [7.0 8.0]", vs)
	}
	seen := map[string]bool{}
	for _, v := range vs {
		img := mongofx.Image(v)
		if !digestRE.MatchString(img) {
			t.Errorf("Image(%q) = %q is not pinned by digest", v, img)
		}
		if seen[img] {
			t.Errorf("versions share one image")
		}
		seen[img] = true
	}
}

func TestImageUnknownVersionPanicsOrEmpty(t *testing.T) {
	defer func() { _ = recover() }()
	if img := mongofx.Image("3.6"); img != "" {
		t.Fatalf("unknown version returned image %q", img)
	}
}

func TestNewAuth_FR54(t *testing.T) {
	a, b := mongofx.NewAuth(t), mongofx.NewAuth(t)
	for _, x := range []*mongofx.Auth{a, b} {
		if x.RootUser == "" || x.ServiceUser == "" || x.RootUser == x.ServiceUser {
			t.Fatalf("users unusable: root=%q service=%q", x.RootUser, x.ServiceUser)
		}
		if len(x.RootPassword) < 16 || len(x.ServicePassword) < 16 || x.RootPassword == x.ServicePassword {
			t.Fatal("passwords too short or equal")
		}
		if n := len(x.Keyfile); n < 6 || n > 1024 {
			t.Fatalf("keyfile length %d outside mongod's 6..1024", n)
		}
	}
	if a.RootPassword == b.RootPassword || string(a.Keyfile) == string(b.Keyfile) || a.ServicePassword == b.ServicePassword {
		t.Fatal("credentials are not random per call")
	}
}

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// FR-54, AC-24: a CA, a server certificate valid for 127.0.0.1 and localhost,
// and a client certificate, all chaining to the CA.
func TestNewTLSMaterial_AC24(t *testing.T) {
	m := mongofx.NewTLSMaterial(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.CAPEM) {
		t.Fatal("CAPEM unusable")
	}
	srv := parseCert(t, m.ServerCertPEM)
	for _, name := range []string{"127.0.0.1", "localhost"} {
		if err := srv.VerifyHostname(name); err != nil {
			t.Errorf("server certificate does not cover %s: %v", name, err)
		}
	}
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server certificate does not verify: %v", err)
	}
	cli := parseCert(t, m.ClientCertPEM)
	if _, err := cli.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate does not verify: %v", err)
	}
	for _, c := range []*x509.Certificate{srv, cli} {
		if time.Now().Before(c.NotBefore) || time.Now().After(c.NotAfter) {
			t.Error("certificate not currently valid")
		}
		if c.NotAfter.Sub(c.NotBefore) > 48*time.Hour {
			t.Error("synthetic certificate valid for more than 48 hours")
		}
	}
	if _, err := tls.X509KeyPair(m.ServerCertPEM, m.ServerKeyPEM); err != nil {
		t.Errorf("server cert and key do not pair: %v", err)
	}
	if _, err := tls.X509KeyPair(m.ClientCertPEM, m.ClientKeyPEM); err != nil {
		t.Errorf("client cert and key do not pair: %v", err)
	}
	// mongod's PEMKeyFile holds certificate and key together.
	if _, err := tls.X509KeyPair(m.ServerPEM, m.ServerPEM); err != nil {
		t.Errorf("ServerPEM is not a combined cert+key file: %v", err)
	}
}

// Two materials are independent: the other CA does not verify this server.
func TestTLSMaterialIndependent(t *testing.T) {
	a, b := mongofx.NewTLSMaterial(t), mongofx.NewTLSMaterial(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b.CAPEM)
	if _, err := parseCert(t, a.ServerCertPEM).Verify(x509.VerifyOptions{Roots: pool}); err == nil {
		t.Fatal("certificates from separate materials share a CA")
	}
}
