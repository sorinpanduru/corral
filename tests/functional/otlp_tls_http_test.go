//go:build functional

package functional

import (
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

// genSyntheticCA writes a throwaway self-signed CA certificate and its key (PEM)
// into t.TempDir(). Synthetic data only; nothing is committed.
func genSyntheticCA(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
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
		t.Fatalf("cert: %v", err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "ca.crt"), filepath.Join(dir, "client.k")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// T23 (FR-20, FR-27, FR-28, AC-15): TLS material (CA, client cert+key) combined
// with a plain http:// OTLP endpoint is a valid configuration under standard
// SDK semantics (http:// means plaintext). corral must start, become ready,
// log no config or startup-failure record, keep stderr empty and exit 0 on
// SIGTERM. Both protocols, general and _TRACES_ variants.
//
// SDK reference: otlptracehttp v1.47.0 client.go Start returns
// errInsecureEndpointWithTLS when Insecure (set by an http:// scheme,
// otlpconfig/options.go WithEndpointURL) meets a TLS config built from the
// CERTIFICATE / CLIENT_CERTIFICATE variables. otlptracegrpc v1.47.0
// otlpconfig/options.go prioritises GRPCCredentials over Insecure, so grpc
// starts (and would use TLS, not plaintext, toward the http:// endpoint);
// for grpc this test therefore pins startup behaviour only.
func TestT23_TLSMaterialWithHTTPEndpoint(t *testing.T) {
	ca, key := genSyntheticCA(t)
	col := otlpcollector.Start(t)

	type variant struct {
		name string
		tls  func(ep string) []string
	}
	const p = "OTEL_EXPORTER_OTLP_"
	const tp = "OTEL_EXPORTER_OTLP_TRACES_"
	variants := []variant{
		{"ca/general", func(ep string) []string { return []string{p + "CERTIFICATE=" + ca, p + "ENDPOINT=" + ep} }},
		{"ca/traces-cert", func(ep string) []string { return []string{tp + "CERTIFICATE=" + ca, p + "ENDPOINT=" + ep} }},
		{"ca/traces-endpoint", func(ep string) []string { return []string{p + "CERTIFICATE=" + ca, tp + "ENDPOINT=" + ep} }},
		{"ca/traces-both", func(ep string) []string { return []string{tp + "CERTIFICATE=" + ca, tp + "ENDPOINT=" + ep} }},
		{"client/general", func(ep string) []string {
			return []string{p + "CLIENT_CERTIFICATE=" + ca, p + "CLIENT_KEY=" + key, p + "ENDPOINT=" + ep}
		}},
		{"client/traces", func(ep string) []string {
			return []string{tp + "CLIENT_CERTIFICATE=" + ca, tp + "CLIENT_KEY=" + key, tp + "ENDPOINT=" + ep}
		}},
		{"ca+client/general", func(ep string) []string {
			return []string{p + "CERTIFICATE=" + ca, p + "CLIENT_CERTIFICATE=" + ca, p + "CLIENT_KEY=" + key, p + "ENDPOINT=" + ep}
		}},
	}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		ep := "http://" + col.HTTPEndpoint()
		if proto == "grpc" {
			ep = "http://" + col.GRPCEndpoint()
		}
		for _, v := range variants {
			t.Run(proto+"/"+v.name, func(t *testing.T) {
				addr := freeAddr(t)
				env := append(v.tls(ep), p+"PROTOCOL="+proto)
				pr := startProc(t, baseEnv(env...), "-tracing.enabled=true", "-admin.listen="+addr)
				pr.waitReady(addr)
				if code := pr.sigterm(); code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
				if b := pr.stderr.Bytes(); len(b) != 0 {
					t.Errorf("stderr has %d bytes, want empty (FR-28)", len(b))
				}
				assertTLSIgnoredWarn(t, pr, 1, ca, key)
				for _, r := range pr.records() {
					if (str(r, "component") == "config" && str(r, "level") != "WARN") || str(r, "level") == "ERROR" ||
						strings.Contains(str(r, "msg"), "startup failed") {
						t.Errorf("unexpected error record: component=%q level=%q msg=%q error=%q detail=%q",
							str(r, "component"), str(r, "level"), str(r, "msg"), str(r, "error"), str(r, "corral.error.detail"))
					}
				}
			})
		}
	}
}

// assertTLSIgnoredWarn asserts the number of WARN records with component=config
// (Owner decision for T23, SPEC-001 Amendment 2 pending: exactly one fixed
// WARN when TLS settings are ignored for an http:// endpoint) and that no
// record or stderr contains a certificate path or PEM content. The exact
// message text is pinned by Amendment 2 and is deliberately not asserted yet.
func assertTLSIgnoredWarn(t *testing.T, p *proc, want int, secrets ...string) {
	t.Helper()
	n := 0
	for _, r := range p.records() {
		if str(r, "component") == "config" && str(r, "level") == "WARN" {
			n++
			if str(r, "msg") == "" {
				t.Errorf("WARN config record has empty msg")
			}
		}
	}
	if n != want {
		t.Errorf("WARN component=config records = %d, want %d", n, want)
	}
	all := string(p.stdout.Bytes()) + string(p.stderr.Bytes())
	for _, sct := range secrets {
		if strings.Contains(all, sct) || strings.Contains(all, filepath.Dir(sct)) {
			t.Errorf("output contains a TLS file path (FR-24)")
		}
	}
	if strings.Contains(all, "BEGIN CERTIFICATE") || strings.Contains(all, "PRIVATE KEY") {
		t.Errorf("output contains PEM content (FR-24)")
	}
}

// T23 controls: no WARN when TLS material is used (https://), absent (no TLS
// material) or irrelevant (no endpoint).
func TestT23_NoWarnControls(t *testing.T) {
	ca, key := genSyntheticCA(t)
	ep := otlpcollector.UnreachableEndpoint(t)
	const p = "OTEL_EXPORTER_OTLP_"
	cases := map[string][]string{
		"ca+https":       {p + "CERTIFICATE=" + ca, p + "ENDPOINT=https://" + ep},
		"ca+no-endpoint": {p + "CERTIFICATE=" + ca},
		"client+https":   {p + "CLIENT_CERTIFICATE=" + ca, p + "CLIENT_KEY=" + key, p + "TRACES_ENDPOINT=https://" + ep},
		"http-no-tls":    {p + "ENDPOINT=http://" + ep},
		// _TRACES_ precedence: effective endpoint is https, so TLS applies.
		"ca+http-general-https-traces": {p + "CERTIFICATE=" + ca, p + "ENDPOINT=http://" + ep, p + "TRACES_ENDPOINT=https://" + ep},
	}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for name, env := range cases {
			t.Run(proto+"/"+name, func(t *testing.T) {
				addr := freeAddr(t)
				pr := startProc(t, baseEnv(append(append([]string{}, env...), p+"PROTOCOL="+proto)...),
					"-tracing.enabled=true", "-admin.listen="+addr)
				pr.waitReady(addr)
				if code := pr.sigterm(); code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
				if b := pr.stderr.Bytes(); len(b) != 0 {
					t.Errorf("stderr has %d bytes, want empty", len(b))
				}
				assertTLSIgnoredWarn(t, pr, 0, ca, key)
			})
		}
	}
}

// T23 precedence: general endpoint is https but the effective _TRACES_
// endpoint is http://, so TLS material is ignored with exactly one WARN.
func TestT23_TracesEndpointPrecedenceHTTP(t *testing.T) {
	ca, key := genSyntheticCA(t)
	col := otlpcollector.Start(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		ep := col.HTTPEndpoint()
		if proto == "grpc" {
			ep = col.GRPCEndpoint()
		}
		t.Run(proto, func(t *testing.T) {
			addr := freeAddr(t)
			pr := startProc(t, baseEnv("OTEL_EXPORTER_OTLP_CERTIFICATE="+ca,
				"OTEL_EXPORTER_OTLP_ENDPOINT=https://"+ep, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://"+ep,
				"OTEL_EXPORTER_OTLP_PROTOCOL="+proto), "-tracing.enabled=true", "-admin.listen="+addr)
			pr.waitReady(addr)
			if code := pr.sigterm(); code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if b := pr.stderr.Bytes(); len(b) != 0 {
				t.Errorf("stderr has %d bytes, want empty", len(b))
			}
			assertTLSIgnoredWarn(t, pr, 1, ca, key)
		})
	}
}

// T23, FR-27: validation still applies when the endpoint is http://. An
// invalid PEM (or unreadable file) is a config error naming the variable.
func TestT23_InvalidPEMWithHTTPEndpointStillConfigError(t *testing.T) {
	const mk = "t23mark-Zx9Cv8Bn7Mq1"
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-"+mk+".pem")
	if err := os.WriteFile(bad, []byte("not pem "+mk), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing-"+mk+".pem")
	ca, key := genSyntheticCA(t)
	const p = "OTEL_EXPORTER_OTLP_"
	cases := []struct {
		name, setting string
		env           []string
	}{
		{"ca-garbage", p + "CERTIFICATE", []string{p + "CERTIFICATE=" + bad}},
		{"ca-missing", p + "CERTIFICATE", []string{p + "CERTIFICATE=" + missing}},
		{"traces-ca-garbage", p + "TRACES_CERTIFICATE", []string{p + "TRACES_CERTIFICATE=" + bad}},
		{"client-key-garbage", p + "CLIENT_KEY", []string{p + "CLIENT_CERTIFICATE=" + ca, p + "CLIENT_KEY=" + bad}},
	}
	_ = key
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, c := range cases {
			t.Run(proto+"/"+c.name, func(t *testing.T) {
				env := append(append([]string{}, c.env...), p+"ENDPOINT=http://127.0.0.1:4318", p+"PROTOCOL="+proto)
				expectConfigFailure(t, baseEnv(env...),
					[]string{"-tracing.enabled=true", "-admin.listen=" + freeAddr(t)}, c.setting, mk)
			})
		}
	}
}
