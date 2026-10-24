package tracing_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logbridge"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// T24.4 (FR-7, FR-24, FR-28, FR-29): when TLS files that Resolve validated
// become unreadable before tracing.Start, the OTel SDK's envconfig logs
// through its global logger with the file path as a key/value argument. The
// logr sink installed by logbridge must keep the path (marker) out of every
// scanned output and nothing may reach stderr. Whether the exporter then
// fails or exports is deliberately not asserted. All key material is
// generated at runtime and is synthetic.

// writeSyntheticPEM writes a self-signed certificate (used as CA, client
// certificate) and its private key under dir.
func writeSyntheticPEM(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "synthetic-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestT24_UnreadableTLSFilesNeverLeakPath(t *testing.T) {
	variants := []struct {
		name   string
		break_ func(t *testing.T, paths ...string)
	}{
		{"removed", func(t *testing.T, paths ...string) {
			for _, p := range paths {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"chmod000", func(t *testing.T, paths ...string) {
			if os.Geteuid() == 0 {
				t.Skip("chmod 000 does not make files unreadable for root")
			}
			for _, p := range paths {
				if err := os.Chmod(p, 0); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, v := range variants {
			t.Run(proto+"/"+v.name, func(t *testing.T) {
				m := newMarker(t)
				dir := filepath.Join(t.TempDir(), m)
				cert, key := writeSyntheticPEM(t, dir)
				ep := otlpcollector.UnreachableEndpoint(t)
				env := failureEnv(proto, "https://"+ep) // https: FR-41 does not apply
				env["OTEL_EXPORTER_OTLP_CERTIFICATE"] = cert
				env["OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"] = cert
				env["OTEL_EXPORTER_OTLP_CLIENT_KEY"] = key
				h := boot(t, opts{on: true, level: slog.LevelDebug, env: env,
					afterResolve: func() { v.break_(t, cert, key) }})
				failFlushes(h, 2)
				scanAll(t, h, nil, m) // finish() asserts empty stderr and JSON stdout
				// Non-vacuity: the SDK's global.Error path reached the logr sink
				// (MsgOTelError records come from the export-failure handler and
				// do not prove this).
				sinkWarns := 0
				for _, r := range h.otelRecords() {
					if r.Msg == logbridge.MsgOTelLogWarn {
						sinkWarns++
					}
				}
				if sinkWarns == 0 {
					t.Error("precondition: no record with the logr sink's fixed warn message; the SDK global.Error path was not exercised")
				}
			})
		}
	}
}
