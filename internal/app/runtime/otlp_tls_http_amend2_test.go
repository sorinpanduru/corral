package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
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
		{"both-ca", map[string]string{"CERTIFICATE": cert, "TRACES_CERTIFICATE": cert}}, // AC-40: one record, both names
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
				// AC-40, FR-41: the record precedes the startup-complete record.
				warnAt, doneAt := -1, -1
				for i, rec := range r.recs {
					if rec["component"] == "config" && rec["level"] == "WARN" {
						warnAt = i
					}
					if rec["component"] == "lifecycle" && rec["msg"] == "corral startup complete" {
						doneAt = i
					}
				}
				if doneAt < 0 {
					t.Errorf("no lifecycle startup-complete record found")
				} else if warnAt > doneAt {
					t.Errorf("FR-41 record (index %d) does not precede startup-complete (index %d)", warnAt, doneAt)
				}
				for k := range w {
					if !allowed[k] {
						t.Errorf("unexpected attribute %q on the WARN record", k)
					}
				}
				for _, l := range r.lines {
					if strings.Contains(l, t23PathMarker) || strings.Contains(l, "-----BEGIN") || strings.Contains(l, cert) || strings.Contains(l, key) || strings.Contains(l, "BEGIN CERTIFICATE") || strings.Contains(l, "PRIVATE KEY") {
						t.Errorf("log line contains a TLS file path or PEM content (FR-24)")
					}
				}
			})
		}
	}
}

// AC-40, FR-41: OTEL_EXPORTER_OTLP_INSECURE=false (and the _TRACES_ variant)
// with an http:// endpoint and NO TLS variable: spans still arrive in
// plaintext and no FR-41 record is emitted.
func TestT23A2_InsecureFalseHTTPNoTLSVars(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, iv := range []string{"INSECURE", "TRACES_INSECURE"} {
			t.Run(proto+"/"+iv, func(t *testing.T) {
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					return map[string]string{t23Pre + "ENDPOINT": "http://" + hp, t23Pre + iv: "false"}
				})
				if len(r.col.Spans()) == 0 {
					t.Errorf("plaintext collector received no span with INSECURE=false on an http:// endpoint")
				}
				if len(r.warns) != 0 {
					t.Errorf("FR-41 records = %d, want 0 (no TLS variable set)", len(r.warns))
				}
			})
		}
	}
}

// AC-40 (and FR-41 last bullets): no FR-41 record with tracing off or with
// OTEL_TRACES_EXPORTER=none, even with an http:// endpoint and valid TLS
// variables.
func TestT23A2_NoWarnWhenTracingOffOrExporterNone(t *testing.T) {
	cert, _ := t23Certs(t)
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
	}{
		"tracing-disabled": {[]string{}, nil},
		"tracing-off-flag": {[]string{"-tracing.enabled=false"}, nil},
		"sdk-disabled":     {[]string{"-tracing.enabled=true"}, map[string]string{"OTEL_SDK_DISABLED": "true"}},
		"exporter-none":    {[]string{"-tracing.enabled=true"}, map[string]string{"OTEL_TRACES_EXPORTER": "none"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{
				t23Pre + "ENDPOINT":    "http://127.0.0.1:4318",
				t23Pre + "CERTIFICATE": cert,
			}
			for k, v := range c.env {
				env[k] = v
			}
			h := run(t, runOpts{args: append([]string{"-admin.listen=" + reserveAddr(t)}, c.args...), env: env})
			h.waitStarted()
			if code := h.stop(); code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			for _, rec := range h.out.records(t) {
				if rec["component"] == "config" && rec["level"] == "WARN" {
					t.Errorf("unexpected FR-41 record: %v", rec["msg"])
				}
			}
		})
	}
}

// AC-40 precedence: general endpoint http://, _TRACES_ endpoint https:// to a
// TLS collector, CA that the collector's certificate chains to: spans arrive
// over TLS and no FR-41 record is emitted. Both protocols. The testkit
// collector is plaintext only, so a local TLS collector is used.
func TestT23A2_HTTPSTracesEndpointDeliversOverTLS(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, certVar := range []string{"CERTIFICATE", "TRACES_CERTIFICATE"} {
			t.Run(proto+"/"+certVar, func(t *testing.T) {
				tc := startTLSCollector(t)
				hp := tc.HTTPAddr
				if proto == "grpc" {
					hp = tc.GRPCAddr
				}
				traces := "https://" + hp
				if proto == "http/protobuf" {
					traces += "/v1/traces"
				}
				h := run(t, runOpts{
					args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
					env: map[string]string{
						t23Pre + "PROTOCOL":        proto,
						t23Pre + "ENDPOINT":        "http://127.0.0.1:1",
						t23Pre + "TRACES_ENDPOINT": traces,
						t23Pre + certVar:           tc.CAFile,
					},
				})
				hd := h.waitStarted()
				_, sp := hd.TracerProvider.Tracer("t23").Start(context.Background(), "t23-span")
				sp.End()
				if code := h.stop(); code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
				if tc.Spans() == 0 {
					t.Errorf("TLS collector received no span")
				}
				for _, rec := range h.out.records(t) {
					if rec["component"] == "config" && rec["level"] == "WARN" {
						t.Errorf("unexpected FR-41 record with an https:// effective endpoint")
					}
				}
			})
		}
	}
}

// AC-40 precedence, other direction: general https:// (unreachable), _TRACES_
// http:// to the plaintext collector, CA set: plaintext delivery plus the record.
func TestT23A2_TracesHTTPOverridesGeneralHTTPS(t *testing.T) {
	cert, _ := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			r := t23Run(t, proto, nil, func(hp string) map[string]string {
				u := "http://" + hp
				if proto == "http/protobuf" {
					u += "/v1/traces"
				}
				return map[string]string{
					t23Pre + "ENDPOINT":        "https://127.0.0.1:1",
					t23Pre + "TRACES_ENDPOINT": u,
					t23Pre + "CERTIFICATE":     cert,
				}
			})
			r.requirePlaintextAndOneWarn(t)
		})
	}
}

// AC-40, FR-20, FR-41: the full 2 x 2 x 2 matrix at log.level=info. Protocol
// (http/protobuf, grpc) x variable form (general: OTEL_EXPORTER_OTLP_*;
// traces: OTEL_EXPORTER_OTLP_TRACES_*, where the TLS variables and the
// endpoint vary together) x TLS material (CA only; client cert + key). Each
// combination starts, delivers a span to the plaintext collector, and emits
// exactly one FR-41 record whose setting is the expected sorted names, before
// startup-complete, with the path marker and "-----BEGIN" absent.
func TestT23A2_AC40Matrix(t *testing.T) {
	cert, key := t23Certs(t)
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, form := range []string{"general", "traces"} {
			for _, mat := range []string{"ca", "client"} {
				t.Run(proto+"/"+form+"/"+mat, func(t *testing.T) {
					pre := t23Pre
					if form == "traces" {
						pre = t23Pre + "TRACES_"
					}
					tls := map[string]string{}
					if mat == "ca" {
						tls[pre+"CERTIFICATE"] = cert
					} else {
						tls[pre+"CLIENT_CERTIFICATE"], tls[pre+"CLIENT_KEY"] = cert, key
					}
					var names []string
					for k := range tls {
						names = append(names, k)
					}
					sort.Strings(names)
					want := strings.Join(names, ",")

					r := t23Run(t, proto, []string{"-log.level=info"}, func(hp string) map[string]string {
						u := "http://" + hp
						if form == "traces" && proto == "http/protobuf" {
							u += "/v1/traces" // per-signal URL is used as-is
						}
						env := map[string]string{pre + "ENDPOINT": u}
						for k, v := range tls {
							env[k] = v
						}
						return env
					})
					r.requirePlaintextAndOneWarn(t)
					if len(r.warns) == 1 {
						w := r.warns[0]
						if w["setting"] != want {
							t.Errorf("setting = %q, want %q", w["setting"], want)
						}
						if w["msg"] != t23Msg {
							t.Errorf("msg = %q, want %q", w["msg"], t23Msg)
						}
					}
					warnAt, doneAt := -1, -1
					for i, rec := range r.recs {
						if rec["component"] == "config" && rec["level"] == "WARN" {
							warnAt = i
						}
						if rec["component"] == "lifecycle" && rec["msg"] == "corral startup complete" {
							doneAt = i
						}
					}
					if doneAt < 0 || warnAt > doneAt {
						t.Errorf("FR-41 record (index %d) must precede startup-complete (index %d)", warnAt, doneAt)
					}
					for _, l := range r.lines {
						if strings.Contains(l, t23PathMarker) || strings.Contains(l, "-----BEGIN") {
							t.Errorf("log line contains the path marker or PEM content (FR-24)")
						}
					}
				})
			}
		}
	}
}

// AC-40 unit case at runtime level: with tracing on and an http:// endpoint, a
// non-PEM file in each of the six TLS variables is rejected under FR-27: exit
// non-zero, exactly one config error naming that variable, no later startup
// step, the path and file content never echoed, and no FR-41 record. The
// valid counterpart is supplied for the client pair so pairing rules cannot be
// the reason for the rejection.
func TestT23A2_NonPEMInEachTLSVariableRejectedNoWarn(t *testing.T) {
	cert, key := t23Certs(t)
	vars := []string{
		"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY",
		"TRACES_CERTIFICATE", "TRACES_CLIENT_CERTIFICATE", "TRACES_CLIENT_KEY",
	}
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, v := range vars {
			t.Run(proto+"/"+v, func(t *testing.T) {
				m := marker(t, "t23badpem")
				bad := filepath.Join(t.TempDir(), m+".pem")
				if err := os.WriteFile(bad, []byte("not pem "+m), 0o600); err != nil {
					t.Fatal(err)
				}
				env := map[string]string{
					t23Pre + "ENDPOINT": "http://127.0.0.1:4318",
					t23Pre + "PROTOCOL": proto,
					t23Pre + v:          bad,
				}
				switch {
				case strings.HasSuffix(v, "CLIENT_CERTIFICATE"):
					env[t23Pre+strings.TrimSuffix(v, "CERTIFICATE")+"KEY"] = key
				case strings.HasSuffix(v, "CLIENT_KEY"):
					env[t23Pre+strings.TrimSuffix(v, "KEY")+"CERTIFICATE"] = cert
				}
				var hooks atomic.Int32
				h := run(t, runOpts{
					args:   []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
					env:    env,
					before: func(context.Context) { hooks.Add(1) },
				})
				if code := h.wait(); code == 0 {
					t.Fatalf("exit code = 0, want non-zero (FR-27)")
				}
				if h.startedCalls() != 0 || hooks.Load() != 0 {
					t.Errorf("startup continued after an FR-27 rejection")
				}
				var cfg []map[string]any
				for _, rec := range h.out.records(t) {
					if rec["component"] == "config" {
						cfg = append(cfg, rec)
					}
					if rec["msg"] == t23Msg {
						t.Errorf("FR-41 record emitted although startup failed under FR-27")
					}
				}
				if len(cfg) != 1 {
					t.Fatalf("component=config records = %d, want exactly 1", len(cfg))
				}
				if cfg[0]["setting"] != t23Pre+v || cfg[0]["level"] != "ERROR" {
					t.Errorf("config record: setting=%v level=%v, want %s ERROR", cfg[0]["setting"], cfg[0]["level"], t23Pre+v)
				}
				for _, l := range h.out.lines() {
					if strings.Contains(l, m) || strings.Contains(l, filepath.Dir(bad)) {
						t.Errorf("output echoes the file path or content (FR-24, FR-27)")
					}
				}
			})
		}
	}
}

// AC-40: an http:// endpoint with no TLS variable and no INSECURE variable:
// spans arrive in plaintext and no FR-41 record is emitted (both protocols,
// general and _TRACES_ endpoint).
func TestT23A2_HTTPNoTLSVarsDelivers(t *testing.T) {
	for _, proto := range []string{"http/protobuf", "grpc"} {
		for _, name := range []string{"ENDPOINT", "TRACES_ENDPOINT"} {
			t.Run(proto+"/"+name, func(t *testing.T) {
				r := t23Run(t, proto, nil, func(hp string) map[string]string {
					u := "http://" + hp
					if proto == "http/protobuf" && name == "TRACES_ENDPOINT" {
						u += "/v1/traces"
					}
					return map[string]string{t23Pre + name: u}
				})
				if len(r.col.Spans()) == 0 {
					t.Errorf("plaintext collector received no span")
				}
				if len(r.warns) != 0 {
					t.Errorf("FR-41 records = %d, want 0 (no TLS variable set)", len(r.warns))
				}
			})
		}
	}
}
