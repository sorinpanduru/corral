package tracing_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// startOTLP resolves the given variables (corral validation, FR-27) and then
// builds the real OTLP exporter through tracing.Start, which hands the
// process environment to the Go OTLP exporters (FR-20 pass-through, "standard
// SDK semantics"). The exporter-options construction has no narrower seam, so
// this is the lowest level that reaches it.
func startOTLP(t *testing.T, vars map[string]string) error {
	t.Helper()
	return startOTLPSpan(t, vars, nil)
}

// startOTLPSpan is startOTLP plus, when onSpan is non-nil, one span emitted
// and flushed before shutdown, then onSpan is called.
func startOTLPSpan(t *testing.T, vars map[string]string, onSpan func()) error {
	t.Helper()
	for _, k := range os.Environ() {
		if name, _, _ := strings.Cut(k, "="); strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	lookup := func(k string) (string, bool) { return os.LookupEnv(k) }
	on, cfg, err := tracing.Resolve(envOn(1.0), lookup)
	if err != nil || !on {
		t.Fatalf("Resolve: on=%v err=%v; corral validation must accept this config (FR-27)", on, err)
	}
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	mf, err := metrics.NewFactory(prometheus.NewRegistry(), cat)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := tracing.Start(ctx, tracing.Options{
		Settings: envOn(1.0), Env: lookup, Config: cfg, Enabled: on, Metrics: mf,
	})
	if err == nil && onSpan != nil {
		_, sp := tr.TracerProvider().Tracer("t23").Start(ctx, "t23-span")
		sp.End()
		_ = tr.ForceFlush(ctx)
		onSpan()
	}
	if err == nil {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = tr.Shutdown(sctx)
	}
	return err
}

// FR-20, FR-27, AC-15: a valid CA file (or client cert+key) combined with a
// plain http:// endpoint is a valid configuration; the exporter is built and
// Start succeeds, for both protocols and the _TRACES_ variants. The OTel
// otlptracehttp v1.47.0 exporter fails Start with "insecure HTTP endpoint
// cannot use TLS client configuration" when an http:// scheme (insecure) meets
// the TLS config built from these variables (client.go Start); corral must not
// surface that as a startup failure.
func TestStart_OTLPTLSMaterialWithHTTPEndpoint(t *testing.T) {
	certs := envGenCerts(t)
	type tc struct {
		name string
		vars map[string]string
	}
	cases := []tc{
		{"ca+endpoint", map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}},
		{"traces-ca+endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}},
		{"ca+traces-endpoint", map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4318/v1/traces"}},
		{"traces-ca+traces-endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4318/v1/traces"}},
		{"client-cert+key+endpoint", map[string]string{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_CLIENT_KEY": certs.Key, "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}},
		{"traces-client-cert+key+traces-endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY": certs.Key, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4318/v1/traces"}},
		{"ca+client+endpoint", map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_CLIENT_KEY": certs.Key, "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}},
		{"ca+endpoint+insecure-true", map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_INSECURE": "true"}},
	}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, c := range cases {
			t.Run(proto+"/"+c.name, func(t *testing.T) {
				vars := map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": proto}
				for k, v := range c.vars {
					if proto == "grpc" {
						v = strings.TrimSuffix(v, "/v1/traces")
					}
					vars[k] = v
				}
				if err := startOTLP(t, vars); err != nil {
					t.Fatalf("Start error = %v (%T), want nil", err, err)
				}
			})
		}
	}
}

// Controls: the combinations the defect report says already work must keep
// working (FR-20).
func TestStart_OTLPTLSMaterialControls(t *testing.T) {
	certs := envGenCerts(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for name, vars := range map[string]map[string]string{
			"ca-no-endpoint":      {"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert},
			"ca-https-endpoint":   {"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_ENDPOINT": "https://127.0.0.1:4318"},
			"http-endpoint-no-ca": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
		} {
			t.Run(proto+"/"+name, func(t *testing.T) {
				v := map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": proto}
				for k, x := range vars {
					v[k] = x
				}
				if err := startOTLP(t, v); err != nil {
					t.Fatalf("Start error = %v, want nil", err)
				}
			})
		}
	}
}

// FR-20, Owner decision T23 (SPEC-001 Amendment 2 pending): an http:// traces
// endpoint is plaintext for both protocols and TLS material is withheld from
// the exporter. The testkit collector serves plaintext only, so a span that
// arrives proves no TLS was applied. No seam exposes the exporter options, so
// delivery is the observable; the implementer may add one (for example a pure
// function returning the exporter option set for an EnvConfig plus the
// effective endpoint) to also unit-test the withholding directly.
func TestStart_HTTPEndpointWithTLSMaterialDeliversPlaintext(t *testing.T) {
	certs := envGenCerts(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for name, tls := range map[string]map[string]string{
			"ca":     {"OTEL_EXPORTER_OTLP_CERTIFICATE": certs.Cert},
			"traces": {"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": certs.Cert},
			"client": {"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_CLIENT_KEY": certs.Key},
			"tr+cl":  {"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE": certs.Cert, "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY": certs.Key},
		} {
			t.Run(proto+"/"+name, func(t *testing.T) {
				col := otlpcollector.Start(t)
				ep := "http://" + col.HTTPEndpoint()
				if proto == "grpc" {
					ep = "http://" + col.GRPCEndpoint()
				}
				vars := map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": proto, "OTEL_EXPORTER_OTLP_ENDPOINT": ep}
				for k, v := range tls {
					vars[k] = v
				}
				err := startOTLPSpan(t, vars, func() {})
				if err != nil {
					t.Fatalf("Start error = %v, want nil", err)
				}
				if len(col.Spans()) == 0 {
					t.Errorf("plaintext collector received no span (TLS material applied to an http:// endpoint)")
				}
			})
		}
	}
}
