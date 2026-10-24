package runtime_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

const (
	t23Msg = "OTLP TLS settings ignored because the traces endpoint uses http://"
	t23Pre = "OTEL_EXPORTER_OTLP_"
)

// t23Result is what a run produced: the plaintext collector, the WARN
// component=config records and every record's key set.
type t23Result struct {
	col   *otlpcollector.Collector
	warns []map[string]any
	lines []string
	recs  []map[string]any
}

// t23Run runs the runtime in-process against the plaintext collector for the
// given protocol. mk builds the env from the collector's endpoint host:port.
func t23Run(t *testing.T, proto string, extraArgs []string, mk func(hostport string) map[string]string) t23Result {
	t.Helper()
	col := otlpcollector.Start(t)
	hp := col.HTTPEndpoint()
	if proto == "grpc" {
		hp = col.GRPCEndpoint()
	}
	env := mk(hp)
	env[t23Pre+"PROTOCOL"] = proto
	h := run(t, runOpts{
		args: append([]string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)}, extraArgs...),
		env:  env,
	})
	hd := h.waitStarted()
	_, sp := hd.TracerProvider.Tracer("t23").Start(context.Background(), "t23-span")
	sp.End()
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	r := t23Result{col: col, lines: h.out.lines(), recs: h.out.records(t)}
	for _, rec := range r.recs {
		if rec["component"] == "config" && rec["level"] == "WARN" {
			r.warns = append(r.warns, rec)
		}
	}
	return r
}

func (r t23Result) requirePlaintextAndOneWarn(t *testing.T) {
	t.Helper()
	if len(r.col.Spans()) == 0 {
		t.Errorf("plaintext collector received no span (TLS material not withheld)")
	}
	if len(r.warns) != 1 {
		t.Errorf("WARN component=config records = %d, want 1", len(r.warns))
	}
}

// Amendment 2 (1), FR-20: OTEL_EXPORTER_OTLP_INSECURE=false (and the _TRACES_
// variant) does not turn an http:// endpoint into TLS: still plaintext, still
// one WARN, both protocols.
func TestT23A2_InsecureFalseWithHTTPEndpointStaysPlaintext(t *testing.T) {
	cert, _ := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, iv := range []string{"INSECURE", "TRACES_INSECURE"} {
			t.Run(proto+"/"+iv, func(t *testing.T) {
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					return map[string]string{
						t23Pre + "ENDPOINT":    "http://" + hp,
						t23Pre + "CERTIFICATE": cert,
						t23Pre + iv:            "false",
					}
				})
				r.requirePlaintextAndOneWarn(t)
			})
		}
	}
}

// Amendment 2 (2): an empty or whitespace-only _TRACES_ENDPOINT counts as
// unset (FR-27) and falls back to the general http:// endpoint.
func TestT23A2_EmptyTracesEndpointFallsBackToGeneralHTTP(t *testing.T) {
	cert, _ := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for name, v := range map[string]string{"empty": "", "spaces": "   ", "tab": "\t"} {
			t.Run(proto+"/"+name, func(t *testing.T) {
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					return map[string]string{
						t23Pre + "ENDPOINT":        "http://" + hp,
						t23Pre + "TRACES_ENDPOINT": v,
						t23Pre + "CERTIFICATE":     cert,
					}
				})
				r.requirePlaintextAndOneWarn(t)
			})
		}
	}
}

// Amendment 2 (3): the scheme is matched case-insensitively end to end.
func TestT23A2_UppercaseHTTPScheme(t *testing.T) {
	cert, _ := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, name := range []string{"ENDPOINT", "TRACES_ENDPOINT"} {
			t.Run(proto+"/"+name, func(t *testing.T) {
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					u := "HTTP://" + hp
					if proto == "http/protobuf" && name == "TRACES_ENDPOINT" {
						u += "/v1/traces"
					}
					return map[string]string{t23Pre + name: u, t23Pre + "CERTIFICATE": cert}
				})
				r.requirePlaintextAndOneWarn(t)
			})
		}
	}
}

// Amendment 2 (4), FR-6: with log.level=error the WARN is not emitted (records
// below the configured level are dropped), yet TLS material is still withheld
// and spans are delivered in plaintext.
func TestT23A2_WarnSuppressedAtLogLevelError(t *testing.T) {
	cert, _ := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			r := t23Run(t, proto, []string{"-log.level=error"}, func(hp string) map[string]string {
				return map[string]string{t23Pre + "ENDPOINT": "http://" + hp, t23Pre + "CERTIFICATE": cert}
			})
			if len(r.col.Spans()) == 0 {
				t.Errorf("plaintext collector received no span")
			}
			if len(r.warns) != 0 {
				t.Errorf("WARN records at log.level=error = %d, want 0", len(r.warns))
			}
			for _, rec := range r.recs {
				if rec["level"] == "WARN" || rec["level"] == "INFO" {
					t.Errorf("record below error emitted: level=%v", rec["level"])
				}
			}
		})
	}
}

// Amendment 2 (5): the WARN's setting attribute is the sorted, comma-joined
// names of exactly the TLS variables that were set; msg is the fixed constant;
// the only attributes are time, level, msg, component and setting. File paths
// and contents never appear.
func TestT23A2_WarnShapeAndSettingNames(t *testing.T) {
	cert, key := t23Certs(t)
	type tc struct {
		name string
		tls  map[string]string
	}
	cases := []tc{
		{"ca", map[string]string{"CERTIFICATE": cert}},
		{"traces-ca", map[string]string{"TRACES_CERTIFICATE": cert}},
		{"client", map[string]string{"CLIENT_CERTIFICATE": cert, "CLIENT_KEY": key}},
		{"traces-client", map[string]string{"TRACES_CLIENT_CERTIFICATE": cert, "TRACES_CLIENT_KEY": key}},
		{"ca+client", map[string]string{"CERTIFICATE": cert, "CLIENT_CERTIFICATE": cert, "CLIENT_KEY": key}},
		{"general-ca+traces-client", map[string]string{"CERTIFICATE": cert, "TRACES_CLIENT_CERTIFICATE": cert, "TRACES_CLIENT_KEY": key}},
		{"all-six", map[string]string{
			"CERTIFICATE": cert, "TRACES_CERTIFICATE": cert,
			"CLIENT_CERTIFICATE": cert, "CLIENT_KEY": key,
			"TRACES_CLIENT_CERTIFICATE": cert, "TRACES_CLIENT_KEY": key,
		}},
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "component": true, "setting": true}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, c := range cases {
			t.Run(proto+"/"+c.name, func(t *testing.T) {
				var names []string
				for k := range c.tls {
					names = append(names, t23Pre+k)
				}
				sort.Strings(names)
				want := strings.Join(names, ",")
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					env := map[string]string{t23Pre + "ENDPOINT": "http://" + hp}
					for k, v := range c.tls {
						env[t23Pre+k] = v
					}
					return env
				})
				if len(r.warns) != 1 {
					t.Fatalf("WARN component=config records = %d, want 1", len(r.warns))
				}
				w := r.warns[0]
				if w["msg"] != t23Msg {
					t.Errorf("msg = %q, want %q", w["msg"], t23Msg)
				}
				if w["setting"] != want {
					t.Errorf("setting = %q, want %q", w["setting"], want)
				}
				for k := range w {
					if !allowed[k] {
						t.Errorf("unexpected attribute %q on the WARN record", k)
					}
				}
				for _, l := range r.lines {
					if strings.Contains(l, cert) || strings.Contains(l, key) || strings.Contains(l, "BEGIN CERTIFICATE") || strings.Contains(l, "PRIVATE KEY") {
						t.Errorf("log line contains a TLS file path or PEM content (FR-24)")
					}
				}
			})
		}
	}
}
