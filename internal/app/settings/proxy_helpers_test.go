package settings_test

// Helpers for the SPEC-002 settings tests (T17). All certificates and keys are
// generated at runtime; nothing is committed. Paths and contents carry random
// markers so a leak into a record is detectable without printing the value.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

func marker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// keyMaterial is one certificate with its private key in the three accepted
// encodings.
type keyMaterial struct {
	CertPEM  []byte
	PKCS8    []byte
	SEC1     []byte
	PKCS1    []byte // empty for EC
	Describe string
}

func certFor(tb testing.TB, pub, priv any, serial int64) []byte {
	tb.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "synthetic"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newEC(tb testing.TB, serial int64) keyMaterial {
	tb.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	s1, _ := x509.MarshalECPrivateKey(k)
	return keyMaterial{
		CertPEM:  certFor(tb, &k.PublicKey, k, serial),
		PKCS8:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}),
		SEC1:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: s1}),
		Describe: "ec",
	}
}

func newRSA(tb testing.TB, serial int64) keyMaterial {
	tb.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	return keyMaterial{
		CertPEM:  certFor(tb, &k.PublicKey, k, serial),
		PKCS8:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}),
		PKCS1:    pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}),
		Describe: "rsa",
	}
}

// env bundles one load: registry, args, environment, file system.
type env struct {
	tb   testing.TB
	m    string
	fs   *memfs.FS
	args []string
	env  []string
}

func newEnv(tb testing.TB) *env { return &env{tb: tb, m: marker(tb), fs: memfs.New()} }

func (e *env) path(name string) string { return "/synthetic/" + e.m + "/" + name }

func (e *env) put(name string, content []byte) string {
	p := e.path(name)
	e.fs.Put(p, content)
	return p
}

// minimal adds the arguments every valid configuration needs (ADR-0021).
func (e *env) minimal() *env {
	e.args = append(e.args, "--upstream.hosts=a.test:27017", "--upstream.auth.mechanism=none")
	return e
}

func (e *env) arg(a ...string) *env     { e.args = append(e.args, a...); return e }
func (e *env) setenv(kv ...string) *env { e.env = append(e.env, kv...); return e }

func (e *env) load() loader.Result {
	e.tb.Helper()
	r, err := settings.Registry()
	if err != nil {
		e.tb.Fatalf("settings.Registry: %v", err)
	}
	return loader.Load(r, loader.Inputs{Args: e.args, Env: source.NewEnv(e.env), ReadFile: e.fs.Read})
}

// tls adds a valid certificate and key (through files) and enables proxy TLS.
func (e *env) tls(km keyMaterial) *env {
	cp := e.put("cert.pem", km.CertPEM)
	kp := e.put("key.pem", km.PKCS8)
	return e.arg("--proxy.tls.enabled=true", "--proxy.tls.cert_path="+cp, "--proxy.tls.key_file="+kp)
}

func records(res loader.Result) []string {
	var out []string
	for _, e := range res.Errors {
		var ce *config.Error
		if errors.As(e, &ce) {
			out = append(out, ce.Setting)
		}
	}
	return out
}

func requireRecordsFor(tb testing.TB, res loader.Result, want ...string) {
	tb.Helper()
	got := records(res)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		tb.Fatalf("records = %v, want %v", got, want)
	}
}

func requireClean(tb testing.TB, res loader.Result) {
	tb.Helper()
	if got := records(res); len(got) != 0 {
		tb.Fatalf("unexpected records: %v", got)
	}
}

// requireOneOf asserts exactly one record, under one of the accepted names
// (a secret file reference may be reported under its _file spelling).
func requireOneOf(tb testing.TB, res loader.Result, names ...string) {
	tb.Helper()
	got := records(res)
	if len(got) != 1 {
		tb.Fatalf("records = %v, want exactly one of %v", got, names)
	}
	for _, n := range names {
		if got[0] == n {
			return
		}
	}
	tb.Fatalf("record under %q, want one of %v", got[0], names)
}

// render prints every record the way the runtime does, plus error text.
func render(res loader.Result) string {
	var b strings.Builder
	l := logging.BootstrapLogger(&b)
	config.LogErrors(l, res.Errors)
	for _, e := range res.Errors {
		fmt.Fprintf(&b, "%v %+v %#v", e, e, e)
	}
	return b.String()
}

func requireNoMarkers(tb testing.TB, res loader.Result, ms ...string) {
	tb.Helper()
	out := render(res)
	for _, m := range ms {
		if m != "" && strings.Contains(out, m) {
			tb.Fatal("a record carries a path, value or content marker")
		}
	}
}

func fmtS(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func slogS(v any) string {
	var b bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&b, nil))
	l.Info("synthetic", "v", v)
	t := slog.New(slog.NewTextHandler(&b, nil))
	t.Info("synthetic", "v", v)
	return b.String()
}

func readGolden(tb testing.TB, path string) string {
	tb.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return string(b)
}
