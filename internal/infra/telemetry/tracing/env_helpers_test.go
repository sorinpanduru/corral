package tracing_test

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/osfile"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

// envMarker returns a random synthetic marker (never real data) used to prove
// that rejected values are not echoed (FR-27, AC-21).
func envMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// envMap builds an injected environment lookup (no os env mutation).
func envMap(m map[string]string) tracing.LookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// envOn returns resolve inputs with tracing enabled and the given ratio.
func envOn(ratio float64) tracing.ResolveInputs {
	return tracing.ResolveInputs{Enabled: true, SampleRatio: ratio}
}

// envOff returns resolve inputs with tracing disabled (tracing.enabled=false,
// the default ratio).
func envOff() tracing.ResolveInputs {
	return tracing.ResolveInputs{Enabled: false, SampleRatio: 1.0}
}

// envResolveAll runs Resolve with the real file reader (osfile.Read), for the
// cases that exercise real files, FIFOs and devices.
func envResolveAll(in tracing.ResolveInputs, env tracing.LookupEnv) (bool, tracing.EnvConfig, []error) {
	return tracing.Resolve(in, env, osfile.Read)
}

// envResolve is the single-error view used by the parity tests migrated from
// SPEC-001: it returns the one error or nil. More than one error is a test
// failure (those tests assert "exactly one error"); it is returned as a non-config error naming the settings only. Tests that expect several errors call envResolveAll or
// tracing.Resolve.
func envResolve(in tracing.ResolveInputs, env tracing.LookupEnv) (bool, tracing.EnvConfig, error) {
	on, cfg, errs := envResolveAll(in, env)
	switch len(errs) {
	case 0:
		return on, cfg, nil
	case 1:
		return on, cfg, errs[0]
	}
	var names []string
	for _, e := range errs {
		var ce *config.Error
		if errors.As(e, &ce) {
			names = append(names, ce.Setting)
		}
	}
	// Not a *config.Error, so envRequireRejected and envMustResolve fail on it.
	return on, cfg, fmt.Errorf("envResolve: Resolve returned %d errors, want at most 1: %v", len(errs), names)
}

// envResolveOn resolves with tracing enabled (ratio 1.0).
func envResolveOn(m map[string]string) (bool, tracing.EnvConfig, error) {
	return envResolve(envOn(1.0), envMap(m))
}

// envMustResolve fails the test unless Resolve succeeds with tracing on.
func envMustResolve(tb testing.TB, m map[string]string) tracing.EnvConfig {
	tb.Helper()
	on, cfg, err := envResolveOn(m)
	if err != nil {
		tb.Fatalf("Resolve(%v) error = %v (type %T), want nil", envKeys(m), err, err)
	}
	if !on {
		tb.Fatalf("Resolve(%v) enabled = false, want true", envKeys(m))
	}
	return cfg
}

// envKeys lists variable names only (never values) for failure messages.
func envKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// envLogger builds a component=config logger through the logging factory,
// behind the logcheck handler (AC-5).
func envLogger(tb testing.TB, buf *bytes.Buffer) *slog.Logger {
	tb.Helper()
	l, err := logging.NewFactory(logcheck.New(tb, logcheck.Tee(buf, logging.FormatJSON))).Logger(logging.Config)
	if err != nil {
		tb.Fatalf("factory: %v", err)
	}
	return l
}

func envNoMarker(tb testing.TB, what, marker, s string) {
	tb.Helper()
	if marker != "" && strings.Contains(s, marker) {
		tb.Errorf("%s leaks the supplied value (marker found): %q", what, s)
	}
}

// envRequireRejected asserts err is a config.Error naming the variable (the
// setting is the env var name), classified config, and that no rendering of it
// nor the FR-27 log record carries the marker.
func envRequireRejected(tb testing.TB, err error, setting, marker string) {
	tb.Helper()
	if err == nil {
		tb.Fatalf("expected a config error for %s, got nil", setting)
	}
	var ce *config.Error
	if !errors.As(err, &ce) {
		tb.Fatalf("error %T is not a *config.Error", err)
	}
	if ce.Setting != setting {
		tb.Errorf("config.Error.Setting = %q, want %q", ce.Setting, setting)
	}
	var cs interface{ ConfigSetting() string }
	if !errors.As(err, &cs) || cs.ConfigSetting() != setting {
		tb.Errorf("ConfigSetting() does not name %q", setting)
	}
	if !strings.Contains(err.Error(), setting) {
		tb.Errorf("Error() = %q, want it to contain %q", err.Error(), setting)
	}
	envNoMarker(tb, "Error()", marker, err.Error())
	envNoMarker(tb, "%v", marker, fmt.Sprintf("%v", err))
	envNoMarker(tb, "%+v", marker, fmt.Sprintf("%+v", err))
	envNoMarker(tb, "%#v", marker, fmt.Sprintf("%#v", err))
	for _, a := range errclass.LogAttrs(err) {
		envNoMarker(tb, "errclass attr "+a.Key, marker, a.Key+"="+a.Value.String())
	}
	if c, _ := errclass.Classify(err); c != errclass.ClassConfig {
		tb.Errorf("errclass class = %q, want %q", c, errclass.ClassConfig)
	}

	var buf bytes.Buffer
	config.LogError(envLogger(tb, &buf), err)
	envNoMarker(tb, "log record", marker, buf.String())
	var rec map[string]any
	if jerr := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); jerr != nil {
		tb.Fatalf("log record is not one JSON object: %v", jerr)
	}
	if rec["setting"] != setting {
		tb.Errorf("log record setting = %v, want %q", rec["setting"], setting)
	}
	if rec["component"] != "config" {
		tb.Errorf("log record component = %v, want config", rec["component"])
	}
}

// envCerts holds paths of synthetic, self-signed material generated at test
// time into t.TempDir(). Nothing is committed; keys are throwaway.
type envCerts struct{ Cert, Key string }

// envKeyKind selects the private-key encoding of a generated pair.
type envKeyKind int

const (
	keyECPKCS8 envKeyKind = iota
	keyECSEC1
	keyRSAPKCS1
	keyRSAPKCS8
)

func envGenCerts(tb testing.TB) envCerts { return envGenPair(tb, keyECPKCS8) }

// envGenPair generates a self-signed certificate and its matching key.
func envGenPair(tb testing.TB, kind envKeyKind) envCerts {
	tb.Helper()
	dir := tb.TempDir()
	var priv crypto.Signer
	var err error
	if kind == keyRSAPKCS1 || kind == keyRSAPKCS8 {
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	} else {
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		tb.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "corral-test.invalid"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		tb.Fatalf("cert: %v", err)
	}
	var block *pem.Block
	switch kind {
	case keyECSEC1:
		b, err := x509.MarshalECPrivateKey(priv.(*ecdsa.PrivateKey))
		if err != nil {
			tb.Fatalf("marshal key: %v", err)
		}
		block = &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	case keyRSAPKCS1:
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv.(*rsa.PrivateKey))}
	default:
		b, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			tb.Fatalf("marshal key: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: b}
	}
	c := envCerts{Cert: filepath.Join(dir, "ca.crt"), Key: filepath.Join(dir, "client.k")}
	envWrite(tb, c.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	envWrite(tb, c.Key, pem.EncodeToMemory(block))
	return c
}

// envPairExtra returns the valid counterpart (same prefix) of a client
// certificate or key variable, or nil for any other variable.
func envPairExtra(v string, c envCerts) map[string]string {
	switch {
	case strings.HasSuffix(v, "_CLIENT_CERTIFICATE"):
		return map[string]string{strings.TrimSuffix(v, "CERTIFICATE") + "KEY": c.Key}
	case strings.HasSuffix(v, "_CLIENT_KEY"):
		return map[string]string{strings.TrimSuffix(v, "KEY") + "CERTIFICATE": c.Cert}
	}
	return nil
}

// envMerge returns m plus the entries of extra.
func envMerge(m, extra map[string]string) map[string]string {
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func envWrite(tb testing.TB, path string, b []byte) {
	tb.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		tb.Fatalf("write: %v", err)
	}
}
