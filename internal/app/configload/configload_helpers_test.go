package configload_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/app/configload"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// SPEC-003 T11: the loader contract as one app-layer function: loader.Load
// (T5) plus OTEL_* validation (T6) when the effective tracing.enabled is
// known, with the four error groups in FR-13 order.
//
// Compliance (SPEC-003 FR-11, FR-19): values, paths and file contents are
// synthetic; markers are random per test. Failure messages print setting
// names and counts, never raw values.
//
// Interface decisions (see the test-writer report): Outcome.Errors holds
// groups 1 to 4 in order; TracingEnabled, Tracing and TLSIgnored are asserted
// only on outcomes without errors (tracing.Resolve returns enabled=false and
// a zero config on any error); help and version return before any file or
// environment access.

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// reg is the real SPEC-001 group plus a test group: t.x (optional string),
// t.n (int, default 5, with a cross rule n <= 100 as primary), t.secret
// (optional text secret).
func reg(tb testing.TB) *registry.Registry {
	tb.Helper()
	tg := registry.Group{
		Spec: "T", Namespaces: []string{"t"},
		Decls: []registry.Decl{
			{Name: "t.x", Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceOptional, Description: "synthetic"},
			{Name: "t.n", Type: types.Spec{Kind: types.KindInt}, Presence: registry.PresenceDefault, Default: "5", Description: "synthetic"},
			{Name: "t.secret", Type: types.Spec{Kind: types.KindText}, Presence: registry.PresenceOptional, Secret: true, Description: "synthetic"},
		},
		Rules: []registry.CrossRule{{
			ID: "n-le-100", Involves: []string{"t.n"}, Primary: "t.n",
			Check: func(get func(string) (any, bool)) error {
				v, _ := get("t.n")
				if n, _ := v.(int64); n > 100 {
					return errors.New("too big")
				}
				return nil
			},
		}},
	}
	r, err := registry.New(append(settings.Groups(), tg)...)
	if err != nil {
		tb.Fatalf("registry: %v", err)
	}
	return r
}

func run(tb testing.TB, args, env []string, fs *memfs.FS) configload.Outcome {
	tb.Helper()
	if fs == nil {
		fs = memfs.New()
	}
	return configload.Load(reg(tb), configload.Inputs{Args: args, Env: env, ReadFile: fs.Read})
}

func names(errs []error) []string {
	out := []string{}
	for _, e := range errs {
		var ce *config.Error
		if errors.As(e, &ce) {
			out = append(out, ce.Setting)
		} else {
			out = append(out, fmt.Sprintf("<%T>", e))
		}
	}
	return out
}

func requireNames(tb testing.TB, out configload.Outcome, want ...string) {
	tb.Helper()
	got := names(out.Errors)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		tb.Fatalf("error settings = %q, want %q", got, want)
	}
}

func render(out configload.Outcome) string {
	var buf bytes.Buffer
	l := logging.BootstrapLogger(&buf)
	config.LogErrors(l, out.Errors)
	config.LogUnknownEnv(l, out.Result.UnknownEnv)
	for _, e := range out.Errors {
		buf.WriteString(e.Error())
		fmt.Fprintf(&buf, "%+v %#v", e, e)
	}
	buf.WriteString(strings.Join(out.Result.Supplied, ","))
	buf.WriteString(strings.Join(out.TLSIgnored, ","))
	return buf.String()
}

func requireNoMarker(tb testing.TB, out configload.Outcome, markers ...string) {
	tb.Helper()
	r := render(out)
	for _, m := range markers {
		if strings.Contains(r, m) {
			tb.Errorf("output carries a marker (value, path or content)")
		}
	}
}

// pemPair generates a synthetic self-signed certificate and its key as PEM.
func pemPair(tb testing.TB) (certPEM, keyPEM []byte) {
	tb.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corral-test.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		tb.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
}

// pemFS holds a valid pair at /pem/ca.crt and /pem/client.key.
func pemFS(tb testing.TB) *memfs.FS {
	tb.Helper()
	c, k := pemPair(tb)
	fs := memfs.New()
	fs.Put("/pem/ca.crt", c)
	fs.Put("/pem/client.key", k)
	return fs
}

var _ = source.MaxPEMFileBytes

// onlyTGroup is a registry that lacks the SPEC-001 tracing settings.
func onlyTGroup(tb testing.TB) *registry.Registry {
	tb.Helper()
	r, err := registry.New(registry.Group{Spec: "T", Namespaces: []string{"t"}, Decls: []registry.Decl{
		{Name: "t.x", Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceOptional, Description: "synthetic"},
	}})
	if err != nil {
		tb.Fatal(err)
	}
	return r
}
