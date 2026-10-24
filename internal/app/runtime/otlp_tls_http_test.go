package runtime_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// t23Certs writes a synthetic self-signed certificate and key into t.TempDir().
func t23Certs(t *testing.T) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corral-test.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "ca.crt"), filepath.Join(dir, "client.k")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// T23 (FR-20, Owner decision, SPEC-001 Amendment 2 pending): when the
// effective traces endpoint is http://, it is plaintext for BOTH protocols.
// TLS material (CA, client cert/key) is withheld from the exporter, so a span
// reaches the plaintext testkit collector (its gRPC server has no TLS; a TLS
// client would fail the handshake and deliver nothing). Exactly one WARN
// component=config record is logged, with no file path or PEM content. The
// OTLP gRPC channel connects lazily, so delivery of a real span is the only
// observable proof of plaintext; connection count alone is not.
func TestT23_HTTPEndpointIsPlaintextForBothProtocols(t *testing.T) {
	cert, key := t23Certs(t)
	const p = "OTEL_EXPORTER_OTLP_"
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, v := range []string{"ca", "traces-ca", "client", "ca+client"} {
			t.Run(proto+"/"+v, func(t *testing.T) {
				col := otlpcollector.Start(t)
				ep := col.HTTPEndpoint()
				if proto == "grpc" {
					ep = col.GRPCEndpoint()
				}
				env := map[string]string{p + "PROTOCOL": proto, p + "TRACES_ENDPOINT": "http://" + ep}
				if proto == "http/protobuf" {
					env[p+"TRACES_ENDPOINT"] += "/v1/traces"
				}
				switch v {
				case "ca":
					env[p+"CERTIFICATE"] = cert
				case "traces-ca":
					env[p+"TRACES_CERTIFICATE"] = cert
				case "client":
					env[p+"CLIENT_CERTIFICATE"], env[p+"CLIENT_KEY"] = cert, key
				case "ca+client":
					env[p+"CERTIFICATE"] = cert
					env[p+"CLIENT_CERTIFICATE"], env[p+"CLIENT_KEY"] = cert, key
				}
				h := run(t, runOpts{
					args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
					env:  env,
				})
				hd := h.waitStarted()
				_, sp := hd.TracerProvider.Tracer("t23").Start(context.Background(), "t23-span")
				sp.End()
				if code := h.stop(); code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
				if len(col.Spans()) == 0 {
					t.Errorf("plaintext collector received no span: TLS material was not withheld for an http:// endpoint")
				}
				warns := 0
				for _, r := range h.out.records(t) {
					if r["component"] == "config" && r["level"] == "WARN" {
						warns++
					}
				}
				if warns != 1 {
					t.Errorf("WARN component=config records = %d, want 1", warns)
				}
				for _, l := range h.out.lines() {
					if strings.Contains(l, cert) || strings.Contains(l, key) || strings.Contains(l, filepath.Dir(cert)) ||
						strings.Contains(l, "BEGIN CERTIFICATE") || strings.Contains(l, "PRIVATE KEY") {
						t.Errorf("log line contains a TLS file path or PEM content (FR-24)")
					}
				}
			})
		}
	}
}
