//go:build functional

package functional

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sorinpanduru/corral/internal/testkit/mongofx"
)

// tlsDir writes the PEM files under a directory whose name is a path marker.
type tlsFiles struct {
	dir, ca, cert, key, clientCA string
	marker                       string
}

func writeTLS(t *testing.T, m *mongofx.TLSMaterial) tlsFiles {
	t.Helper()
	marker := "pathmk-" + pxRand(t, 8)
	dir := filepath.Join(t.TempDir(), marker)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := tlsFiles{dir: dir, marker: marker, ca: filepath.Join(dir, "ca.pem"), cert: filepath.Join(dir, "server.pem"), key: filepath.Join(dir, "server.key")}
	for p, b := range map[string][]byte{f.ca: m.CAPEM, f.cert: m.ServerCertPEM, f.key: m.ServerKeyPEM} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func clientTLS(t *testing.T, m *mongofx.TLSMaterial, withCert bool) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.CAPEM) {
		t.Fatal("bad CA")
	}
	cfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	if withCert {
		cert, err := tls.X509KeyPair(m.ClientCertPEM, m.ClientKeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg
}

func certSubject(t *testing.T, certPEM []byte) string {
	blk, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.Subject.CommonName
}

func driverOK(t *testing.T, uri string, mod func(*options.ClientOptions)) error {
	t.Helper()
	cl := pxGoClient(t, uri, func(o *options.ClientOptions) {
		o.SetServerSelectionTimeout(5 * time.Second).SetRetryReads(false)
		if mod != nil {
			mod(o)
		}
	})
	return cl.Database("corraltest").Collection("c").FindOne(context.Background(), bson.D{}).Err()
}

// AC-24 (FR-44, FR-45): client TLS, mutual TLS, upstream TLS with the right and
// the wrong CA; nothing identifying leaks.
func TestTLS_AC24(t *testing.T) {
	forVersions(t, func(t *testing.T, v string) {
		m := mongofx.NewTLSMaterial(t)
		files := writeTLS(t, m)
		rs := mongofx.StartReplicaSet(t, mongofx.Opts{Version: v, Members: 1, TLS: m})
		keyEnv := []string{"CORRAL_PROXY_TLS_KEY_FILE=" + files.key}
		up := map[string]string{"upstream.tls.enabled": "true", "upstream.tls.ca_path": files.ca}

		t.Run("client TLS and plaintext", func(t *testing.T) {
			set := map[string]string{"proxy.tls.enabled": "true", "proxy.tls.cert_path": files.cert}
			for k, val := range up {
				set[k] = val
			}
			c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", set: set, env: keyEnv})
			c.waitReady()
			uri := "mongodb://" + c.proxy
			if err := driverOK(t, uri+"/?tls=true", func(o *options.ClientOptions) { o.SetTLSConfig(clientTLS(t, m, false)) }); err != nil && err != mongoNoDocs() {
				t.Fatalf("a TLS client: %v", err)
			}
			if err := driverOK(t, uri, nil); err == nil || err == mongoNoDocs() {
				t.Fatal("a plaintext client succeeded against a TLS listener")
			}
			// Nothing identifying in any output.
			subject := certSubject(t, m.ServerCertPEM)
			keyLine := ""
			for _, l := range strings.Split(string(m.ServerKeyPEM), "\n") {
				if len(l) > 40 && !strings.HasPrefix(l, "-----") {
					keyLine = l
					break
				}
			}
			for name, out := range map[string][]byte{"stdout": c.p.Stdout(), "stderr": c.p.Stderr()} {
				for what, needle := range map[string]string{"path marker": files.marker, "subject": subject, "key bytes": keyLine} {
					if needle != "" && bytes.Contains(out, []byte(needle)) {
						t.Errorf("%s contains the %s", name, what)
					}
				}
			}
		})

		t.Run("mutual TLS", func(t *testing.T) {
			caFile := filepath.Join(files.dir, "client-ca.pem")
			os.WriteFile(caFile, m.CAPEM, 0o600)
			set := map[string]string{"proxy.tls.enabled": "true", "proxy.tls.cert_path": files.cert, "proxy.tls.client_ca_path": caFile}
			for k, val := range up {
				set[k] = val
			}
			c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", set: set, env: keyEnv})
			c.waitReady()
			uri := "mongodb://" + c.proxy + "/?tls=true"
			if err := driverOK(t, uri, func(o *options.ClientOptions) { o.SetTLSConfig(clientTLS(t, m, false)) }); err == nil || err == mongoNoDocs() {
				t.Fatal("a client without a certificate succeeded under mutual TLS")
			}
			if err := driverOK(t, uri, func(o *options.ClientOptions) { o.SetTLSConfig(clientTLS(t, m, true)) }); err != nil && err != mongoNoDocs() {
				t.Fatalf("a client with a valid certificate: %v", err)
			}
		})

		t.Run("wrong upstream CA", func(t *testing.T) {
			other := mongofx.NewTLSMaterial(t)
			wrong := filepath.Join(files.dir, "wrong-ca.pem")
			os.WriteFile(wrong, other.CAPEM, 0o600)
			c := pxStart(t, pxOpts{upstream: rs.Addrs(), rs: "rs0", set: map[string]string{"upstream.tls.enabled": "true", "upstream.tls.ca_path": wrong}})
			c.waitReadyz(503, "upstream_unavailable")
			c.waitMetricAtLeast("a network connect error", 1, "corral_upstream_connect_errors_total", "error_class", "network")
			pxEventually(t, 30*time.Second, "a tls_handshake record", func() bool {
				for _, r := range c.records("upstream connection failed") {
					if r["corral.error.detail"] == "tls_handshake" {
						return true
					}
				}
				return false
			})
		})
	})
}

func mongoNoDocs() error { return mongo.ErrNoDocuments }
