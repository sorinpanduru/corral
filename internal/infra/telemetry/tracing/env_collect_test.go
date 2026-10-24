package tracing_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// SPEC-003 T6 (FR-19, FR-25; AC-6 unit, AC-13 unit, AC-15, AC-20, AC-21,
// AC-22): Resolve reports every invalid OTEL_* variable, tracks per-variable
// errors for the cross-variable skip rule, reads PEM files only through the
// injected reader (4 MiB cap, each path at most once) and reads the
// environment only through the injected lookup.
//
// Compliance (FR-11, FR-19): no error carries a value, a path or file
// contents. Values and paths here carry random markers; fixtures are
// synthetic and generated at test time.

const (
	vExporter = "OTEL_TRACES_EXPORTER"
	vQueue    = "OTEL_BSP_MAX_QUEUE_SIZE"
	vBatch    = "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"
	vProto    = "OTEL_EXPORTER_OTLP_PROTOCOL"
	vHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"
	gCert     = "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"
	gKey      = "OTEL_EXPORTER_OTLP_CLIENT_KEY"
	tCert     = "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE"
	tKey      = "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY"
)

// settingsOf returns the setting of every error, in order, failing the test
// when an error is not a *config.Error or carries a marker.
func settingsOf(tb testing.TB, errs []error, markers ...string) []string {
	tb.Helper()
	out := []string{}
	for _, err := range errs {
		var ce *config.Error
		if !errors.As(err, &ce) {
			tb.Fatalf("error %T is not a *config.Error", err)
		}
		out = append(out, ce.Setting)
		for _, m := range markers {
			for _, txt := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(txt, m) {
					tb.Errorf("error for %s carries a marker (value, path or content)", ce.Setting)
				}
			}
		}
	}
	return out
}

func requireSettings(tb testing.TB, errs []error, want []string, markers ...string) {
	tb.Helper()
	got := settingsOf(tb, errs, markers...)
	if !reflect.DeepEqual(got, want) {
		tb.Fatalf("error settings = %v, want %v", got, want)
	}
}

// memPair returns a memfs holding a valid certificate and key at synthetic
// paths, plus a second, non-matching key.
type memPair struct {
	fs                         *memfs.FS
	cert, key, otherKey, other string
}

func newMemPair(tb testing.TB) memPair {
	tb.Helper()
	a, b := envGenCerts(tb), envGenCerts(tb)
	rd := func(p string) []byte {
		bs, err := os.ReadFile(p)
		if err != nil {
			tb.Fatal(err)
		}
		return bs
	}
	m := memPair{fs: memfs.New(), cert: "/pem/a.crt", key: "/pem/a.key", other: "/pem/b.crt", otherKey: "/pem/b.key"}
	m.fs.Put(m.cert, rd(a.Cert))
	m.fs.Put(m.key, rd(a.Key))
	m.fs.Put(m.other, rd(b.Cert))
	m.fs.Put(m.otherKey, rd(b.Key))
	return m
}

func resolveMem(on tracing.ResolveInputs, m map[string]string, fs *memfs.FS) (bool, tracing.EnvConfig, []error) {
	return tracing.Resolve(on, envMap(m), fs.Read)
}

func onIn() tracing.ResolveInputs { return envOn(1.0) }

// ---- collect all errors (FR-19, AC-6 unit) ----

func TestResolve_ReportsEveryInvalidVariableSorted(t *testing.T) {
	m1, m2, m3 := envMarker(t), envMarker(t), envMarker(t)
	env := map[string]string{
		"OTEL_TRACES_SAMPLER":        "bad-" + m1,
		"OTEL_EXPORTER_OTLP_TIMEOUT": "-" + m2,
		"OTEL_BSP_SCHEDULE_DELAY":    "x" + m3,
	}
	_, _, errs := resolveMem(onIn(), env, memfs.New())
	requireSettings(t, errs, []string{
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_TRACES_SAMPLER",
	}, m1, m2, m3)
}

func TestResolve_BothVariantsReportedSeparately(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "bad",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "bad",
	}
	_, _, errs := resolveMem(onIn(), env, memfs.New())
	requireSettings(t, errs, []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"})
}

func TestResolve_ErrorsSortedBySettingByteOrder(t *testing.T) {
	env := map[string]string{
		"OTEL_TRACES_SAMPLER_ARG": "9", "OTEL_TRACES_SAMPLER": "x", "OTEL_RESOURCE_ATTRIBUTES": "x",
		"OTEL_BSP_EXPORT_TIMEOUT": "0", "OTEL_EXPORTER_OTLP_COMPRESSION": "x", "OTEL_EXPORTER_OTLP_INSECURE": "x",
	}
	_, _, errs := resolveMem(onIn(), env, memfs.New())
	got := settingsOf(t, errs)
	// Amendment 2: with an invalid OTEL_TRACES_SAMPLER the ARG check is
	// skipped, so ARG has no record even though its value (9) is out of range.
	want := len(env) - 1
	if len(got) != want {
		t.Fatalf("got %d errors, want %d: %v", len(got), want, got)
	}
	for _, g := range got {
		if g == "OTEL_TRACES_SAMPLER_ARG" {
			t.Fatalf("OTEL_TRACES_SAMPLER_ARG reported although the sampler is invalid: %v", got)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("errors not sorted by setting: %v", got)
	}
}

// SPEC-003 Amendment 2 (FR-19, AC-20): when OTEL_TRACES_SAMPLER is invalid, the
// OTEL_TRACES_SAMPLER_ARG check is skipped (the effective sampler is unknown,
// so whether ARG is used is unknown). Exactly one record.
func TestAC20_InvalidSamplerSkipsSamplerArgCheck(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"arg out of range":  {"OTEL_TRACES_SAMPLER": "x", "OTEL_TRACES_SAMPLER_ARG": "9"},
		"arg not a number":  {"OTEL_TRACES_SAMPLER": "x", "OTEL_TRACES_SAMPLER_ARG": "abc"},
		"arg valid":         {"OTEL_TRACES_SAMPLER": "x", "OTEL_TRACES_SAMPLER_ARG": "0.5"},
		"known-bad variant": {"OTEL_TRACES_SAMPLER": "jaeger_remote", "OTEL_TRACES_SAMPLER_ARG": "9"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, errs := resolveMem(onIn(), env, memfs.New())
			requireSettings(t, errs, []string{"OTEL_TRACES_SAMPLER"})
		})
	}
	// With a valid sampler the ARG check still runs.
	_, _, errs := resolveMem(onIn(), map[string]string{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "9"}, memfs.New())
	requireSettings(t, errs, []string{"OTEL_TRACES_SAMPLER_ARG"})
}

// Every value of the shared invalid table yields exactly one error naming the
// variable (behaviour parity with SPEC-001, now as a list of one).
func TestResolve_EveryInvalidCaseYieldsExactlyOneError(t *testing.T) {
	for _, c := range envInvalidCases(t) {
		t.Run(c.name, func(t *testing.T) {
			marker := envMarker(t)
			m := map[string]string{c.setting: c.value(t, marker)}
			for k, v := range c.extra {
				m[k] = v
			}
			_, _, errs := envResolveAll(onIn(), envMap(m))
			requireSettings(t, errs, []string{c.setting}, marker)
		})
	}
}

// AC-13 (unit): an invalid sampler with tracing on is a reported error, and
// with tracing off nothing is validated.
func TestResolve_InvalidSamplerWithTracingOn(t *testing.T) {
	_, _, errs := resolveMem(onIn(), map[string]string{"OTEL_TRACES_SAMPLER": "jaeger_remote"}, memfs.New())
	requireSettings(t, errs, []string{"OTEL_TRACES_SAMPLER"})
	_, _, errs = resolveMem(envOff(), map[string]string{"OTEL_TRACES_SAMPLER": "jaeger_remote"}, memfs.New())
	if len(errs) != 0 {
		t.Fatalf("tracing off: %d errors, want 0", len(errs))
	}
}

func TestResolve_ValidEnvironmentReturnsNoErrors(t *testing.T) {
	on, cfg, errs := resolveMem(onIn(), nil, memfs.New())
	if !on || len(errs) != 0 {
		t.Fatalf("on=%v errors=%d", on, len(errs))
	}
	if cfg.Exporter != tracing.ExporterOTLP || cfg.SampleRatio != 1.0 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// The inputs carry the ratio now (no config.Settings).
func TestResolve_InputsCarryEnabledAndRatio(t *testing.T) {
	_, cfg, errs := resolveMem(tracing.ResolveInputs{Enabled: true, SampleRatio: 0.25}, nil, memfs.New())
	if len(errs) != 0 || cfg.SampleRatio != 0.25 {
		t.Fatalf("errs=%d ratio=%v", len(errs), cfg.SampleRatio)
	}
}

// ---- cross-variable checks and the skip rule (FR-19, AC-20) ----

// AC-20 first bullet: invalid queue size skips the batch check even when the
// batch exceeds the default queue size.
func TestAC20_InvalidQueueSkipsBatchCheck(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"batch above default queue": {vQueue: "abc", vBatch: "4096"},
		"batch below default queue": {vQueue: "abc", vBatch: "10"},
		"batch unset":               {vQueue: "abc"},
		"queue zero":                {vQueue: "0", vBatch: "4096"},
		"queue overflow":            {vQueue: "99999999999999999999999", vBatch: "4096"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, errs := resolveMem(onIn(), env, memfs.New())
			requireSettings(t, errs, []string{vQueue})
		})
	}
}

// The mirror case: an invalid batch size has its own error and no
// batch-too-large record is added for it.
func TestAC20_InvalidBatchHasOneRecord(t *testing.T) {
	_, _, errs := resolveMem(onIn(), map[string]string{vQueue: "100", vBatch: "abc"}, memfs.New())
	requireSettings(t, errs, []string{vBatch})
}

func TestAC20_BothInvalidTwoRecords(t *testing.T) {
	_, _, errs := resolveMem(onIn(), map[string]string{vQueue: "0", vBatch: "0"}, memfs.New())
	requireSettings(t, errs, []string{vBatch, vQueue})
}

// AC-20 third bullet: valid queue 100 and batch 200 is one batch record.
func TestAC20_BatchLargerThanQueue(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"100/200":                  {vQueue: "100", vBatch: "200"},
		"batch over default queue": {vBatch: "4096"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, errs := resolveMem(onIn(), env, memfs.New())
			requireSettings(t, errs, []string{vBatch})
		})
	}
	_, _, errs := resolveMem(onIn(), map[string]string{vQueue: "100", vBatch: "100"}, memfs.New())
	if len(errs) != 0 {
		t.Fatalf("batch == queue gave %d errors", len(errs))
	}
}

type formVars struct{ name, cert, key string }

var forms = []formVars{{"general", gCert, gKey}, {"traces", tCert, tKey}}

// AC-20 second bullet and the pairing matrix, for both forms.
func TestAC20_ClientPairingMatrix(t *testing.T) {
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			p := newMemPair(t)
			junk := "/pem/junk.txt"
			p.fs.Put(junk, []byte("not PEM at all\n"))
			cases := []struct {
				name string
				env  map[string]string
				want []string
			}{
				{"valid pair", map[string]string{f.cert: p.cert, f.key: p.key}, []string{}},
				{"cert without key", map[string]string{f.cert: p.cert}, []string{f.key}},
				{"key without cert", map[string]string{f.key: p.key}, []string{f.cert}},
				{"mismatched pair", map[string]string{f.cert: p.cert, f.key: p.otherKey}, []string{f.key}},
				// Skip rule: a variable of the pair has its own error, so the
				// pairing and presence checks of that form produce nothing.
				{"non-PEM cert, valid key", map[string]string{f.cert: junk, f.key: p.key}, []string{f.cert}},
				{"valid cert, non-PEM key", map[string]string{f.cert: p.cert, f.key: junk}, []string{f.key}},
				{"missing cert file, valid key", map[string]string{f.cert: "/pem/absent", f.key: p.key}, []string{f.cert}},
				{"valid cert, missing key file", map[string]string{f.cert: p.cert, f.key: "/pem/absent"}, []string{f.key}},
				{"cert invalid, key unset", map[string]string{f.cert: junk}, []string{f.cert}},
				{"key invalid, cert unset", map[string]string{f.key: junk}, []string{f.key}},
				{"both invalid", map[string]string{f.cert: junk, f.key: junk}, []string{f.cert, f.key}},
				{"key points at the certificate file", map[string]string{f.cert: p.cert, f.key: p.cert}, []string{f.key}},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					_, _, errs := resolveMem(onIn(), c.env, p.fs)
					requireSettings(t, errs, c.want)
				})
			}
		})
	}
}

// The two forms are independent: each is checked on its own.
func TestAC20_FormsAreIndependent(t *testing.T) {
	p := newMemPair(t)
	junk := "/pem/junk.txt"
	p.fs.Put(junk, []byte("not PEM\n"))
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"two valid pairs", map[string]string{gCert: p.cert, gKey: p.key, tCert: p.other, tKey: p.otherKey}, []string{}},
		{"general invalid cert, traces mismatched",
			map[string]string{gCert: junk, gKey: p.key, tCert: p.cert, tKey: p.otherKey}, []string{gCert, tKey}},
		{"mixed prefixes (general cert, traces key)",
			map[string]string{gCert: p.cert, tKey: p.key}, []string{gKey, tCert}},
		{"general fine, traces cert only", map[string]string{gCert: p.cert, gKey: p.key, tCert: p.other}, []string{tKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, errs := resolveMem(onIn(), c.env, p.fs)
			requireSettings(t, errs, c.want)
		})
	}
}

// ---- AC-21: an invalid exporter does not stop other validation ----

func TestAC21_ExporterConsoleWithProtocolAndHeaders(t *testing.T) {
	env := map[string]string{vExporter: "console", vProto: "http/json", vHeaders: "no-equals"}
	_, _, errs := resolveMem(onIn(), env, memfs.New())
	requireSettings(t, errs, []string{vHeaders, vProto, vExporter})
}

// Every per-variable check gives the same result whether the exporter is
// otlp, none, unset or invalid: the invalid exporter adds exactly its own
// record and removes none.
func TestAC21_PerVariableChecksIdenticalForEveryExporter(t *testing.T) {
	for _, c := range envInvalidCases(t) {
		if c.setting == vExporter {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			marker := envMarker(t)
			base := map[string]string{c.setting: c.value(t, marker)}
			for k, v := range c.extra {
				base[k] = v
			}
			with := func(exp string) []string {
				m := map[string]string{}
				for k, v := range base {
					m[k] = v
				}
				if exp != "" {
					m[vExporter] = exp
				}
				_, _, errs := envResolveAll(onIn(), envMap(m))
				return settingsOf(t, errs, marker)
			}
			want := []string{c.setting}
			for _, exp := range []string{"", "otlp", "none"} {
				if got := with(exp); !reflect.DeepEqual(got, want) {
					t.Errorf("exporter %q: errors %v, want %v", exp, got, want)
				}
			}
			for _, exp := range []string{"console", "zipkin", "otlp,none"} {
				got := with(exp)
				w := append([]string{c.setting, vExporter}, nil...)
				sort.Strings(w)
				if !reflect.DeepEqual(got, w) {
					t.Errorf("invalid exporter %q: errors %v, want %v", exp, got, w)
				}
			}
		})
	}
}

// The valid-everything case: an invalid exporter alone is one record.
func TestAC21_InvalidExporterAloneIsOneRecord(t *testing.T) {
	p := newMemPair(t)
	env := map[string]string{vExporter: "console", vProto: "grpc", vHeaders: "a=b", gCert: p.cert, gKey: p.key}
	_, _, errs := resolveMem(onIn(), env, p.fs)
	requireSettings(t, errs, []string{vExporter})
}

// Cross-variable checks still run with an invalid exporter.
func TestAC21_CrossChecksStillRunWithInvalidExporter(t *testing.T) {
	_, _, errs := resolveMem(onIn(), map[string]string{vExporter: "console", vQueue: "100", vBatch: "200"}, memfs.New())
	requireSettings(t, errs, []string{vBatch, vExporter})
}

// ---- AC-22: the OTEL_* whitespace set ----

var fr7Whitespace = []string{" ", "\t", "\n", "\v", "\f", "\r", "\u0085", "\u00a0", "\u2003", "\u3000", " \t\u00a0\u3000 "}

func TestAC22_WhitespaceOnlyCountsAsUnsetForEveryVariable(t *testing.T) {
	vars := []string{
		vExporter, "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME",
		vQueue, vBatch, "OTEL_BSP_SCHEDULE_DELAY", "OTEL_BSP_EXPORT_TIMEOUT",
	}
	for _, suffix := range []string{"PROTOCOL", "ENDPOINT", "HEADERS", "TIMEOUT", "COMPRESSION", "INSECURE",
		"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY"} {
		vars = append(vars, "OTEL_EXPORTER_OTLP_"+suffix, "OTEL_EXPORTER_OTLP_TRACES_"+suffix)
	}
	_, want, errs := resolveMem(onIn(), nil, memfs.New())
	if len(errs) != 0 {
		t.Fatal("baseline failed")
	}
	for _, ws := range fr7Whitespace {
		fs := memfs.New()
		for _, v := range vars {
			_, cfg, errs := resolveMem(onIn(), map[string]string{v: ws}, fs)
			if len(errs) != 0 {
				t.Errorf("%s with whitespace-only value (length %d): %d errors, want unset", v, len(ws), len(errs))
				continue
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Errorf("%s with whitespace-only value changed the config", v)
			}
		}
		// Whitespace-only values never reach the file reader.
		for _, v := range vars {
			if n := fs.Calls(ws); n != 0 {
				t.Errorf("reader called %d times for a whitespace-only path (%s)", n, v)
			}
		}
	}
}

func TestAC22_NotWhitespace(t *testing.T) {
	for name, v := range map[string]string{"zero-width space": "\u200b", "BOM": "\ufeff", "invalid UTF-8": "\xff", "bare 0x85": "\x85"} {
		t.Run(name, func(t *testing.T) {
			_, _, errs := resolveMem(onIn(), map[string]string{vExporter: v}, memfs.New())
			requireSettings(t, errs, []string{vExporter})
		})
	}
}

// Trimming of OTEL_* values is kept: surrounding Unicode whitespace is removed.
func TestAC22_SurroundingWhitespaceTrimmed(t *testing.T) {
	_, cfg, errs := resolveMem(onIn(), map[string]string{
		vExporter: "\u00a0none\u3000", "OTEL_EXPORTER_OTLP_PROTOCOL": "\tgrpc\u2003", vQueue: " 100\u00a0", vBatch: "\u0085100 ",
	}, memfs.New())
	if len(errs) != 0 {
		t.Fatalf("%d errors", len(errs))
	}
	if cfg.Exporter != tracing.ExporterNone || cfg.Protocol != tracing.ProtocolGRPC ||
		cfg.Queue.MaxQueueSize != 100 || cfg.Queue.MaxExportBatchSize != 100 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// ---- PEM reads through the injected reader (FR-15 style, FR-19, FR-25) ----

func TestPEM_ReadOncePerDistinctPath(t *testing.T) {
	p := newMemPair(t)
	// The certificate path is used by an individual check (CERTIFICATE) and
	// by a pair check (CLIENT_CERTIFICATE) in both variants; the key path by
	// the individual and the pair check.
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE":        p.cert,
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": p.cert,
		gCert:                                   p.cert, gKey: p.key,
		tCert: p.cert, tKey: p.key,
	}
	_, _, errs := resolveMem(onIn(), env, p.fs)
	if len(errs) != 0 {
		t.Fatalf("%d errors for a valid configuration", len(errs))
	}
	for _, path := range []string{p.cert, p.key} {
		if n := p.fs.Calls(path); n != 1 {
			t.Errorf("path %s read %d times, want exactly 1", path, n)
		}
	}
	for _, path := range []string{p.other, p.otherKey} {
		if n := p.fs.Calls(path); n != 0 {
			t.Errorf("unreferenced path %s read %d times", path, n)
		}
	}
}

// A path that fails to read is still read once, however many variables
// name it; each variable that names it reports its own error.
func TestPEM_FailedReadAlsoOnce(t *testing.T) {
	p := newMemPair(t)
	p.fs.Fail("/pem/bad", source.ErrUnreadable)
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE":        "/pem/bad",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": "/pem/bad",
		gCert:                                   "/pem/bad", gKey: p.key,
	}
	_, _, errs := resolveMem(onIn(), env, p.fs)
	requireSettings(t, errs, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE", gCert, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE"})
	if n := p.fs.Calls("/pem/bad"); n != 1 {
		t.Fatalf("failing path read %d times, want 1", n)
	}
}

func TestPEM_ReaderGetsTheFourMiBLimit(t *testing.T) {
	p := newMemPair(t)
	var seen []int64
	rd := func(path string, max int64) ([]byte, error) {
		seen = append(seen, max)
		return p.fs.Read(path, max)
	}
	_, _, errs := tracing.Resolve(onIn(), envMap(map[string]string{gCert: p.cert, gKey: p.key}), rd)
	if len(errs) != 0 {
		t.Fatalf("%d errors", len(errs))
	}
	if len(seen) == 0 {
		t.Fatal("reader never called")
	}
	for _, m := range seen {
		if m != 4194304 {
			t.Errorf("reader called with maxBytes %d, want 4194304", m)
		}
	}
}

func TestPEM_SizeCap(t *testing.T) {
	a := envGenCerts(t)
	cert, err := os.ReadFile(a.Cert)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(n int) []byte {
		return append(append([]byte{}, cert...), bytes.Repeat([]byte("\n"), n-len(cert))...)
	}
	fs := memfs.New()
	fs.Put("/ok", pad(4194304))
	fs.Put("/big", pad(4194305))
	_, _, errs := resolveMem(onIn(), map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/ok"}, fs)
	if len(errs) != 0 {
		t.Fatalf("exactly 4 MiB of valid PEM rejected: %d errors", len(errs))
	}
	_, _, errs = resolveMem(onIn(), map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/big"}, fs)
	requireSettings(t, errs, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE"})
}

func TestPEM_ReaderFailureKindsAreConfigErrorsWithoutEcho(t *testing.T) {
	for name, kind := range map[string]error{
		"unreadable":  source.ErrUnreadable,
		"not regular": source.ErrNotRegular,
		"too large":   source.ErrTooLarge,
		"custom":      errors.New("reader says: "),
	} {
		for _, v := range envFileVars {
			t.Run(name+"/"+v, func(t *testing.T) {
				m := envMarker(t)
				path := "/synthetic/" + m + "/file.pem"
				fs := memfs.New()
				e := kind
				if name == "custom" {
					e = errors.New("reader failed for " + path + " " + m)
				}
				fs.Fail(path, e)
				p := newMemPair(t)
				env := envMerge(map[string]string{v: path}, envPairExtra(v, envCerts{Cert: p.cert, Key: p.key}))
				fs.Put(p.cert, mustRead(t, p.fs, p.cert))
				fs.Put(p.key, mustRead(t, p.fs, p.key))
				_, _, errs := resolveMem(onIn(), env, fs)
				requireSettings(t, errs, []string{v}, m, path)
				if n := fs.Calls(path); n != 1 {
					t.Errorf("failing path read %d times, want 1", n)
				}
			})
		}
	}
}

func mustRead(tb testing.TB, fs *memfs.FS, path string) []byte {
	tb.Helper()
	b, err := fs.Read(path, source.MaxPEMFileBytes)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// Content that is not PEM is rejected without echoing the content.
func TestPEM_ContentNotEchoed(t *testing.T) {
	m := envMarker(t)
	fs := memfs.New()
	fs.Put("/junk", []byte("-----BEGIN CERTIFICATE-----\n"+m+"\n-----END CERTIFICATE-----\n"))
	_, _, errs := resolveMem(onIn(), map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/junk"}, fs)
	requireSettings(t, errs, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE"}, m)
}

// ---- nothing is read when tracing is off; no process globals (FR-25, AC-15) ----

func TestOff_NothingReadAndOnlySDKDisabledLooked(t *testing.T) {
	p := newMemPair(t)
	cases := []struct {
		name string
		in   tracing.ResolveInputs
		sdk  string
	}{
		{"enabled=false", envOff(), ""},
		{"SDK_DISABLED=true", onIn(), "true"},
		{"SDK_DISABLED=TRUE and enabled=false", envOff(), "TRUE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			lookup := func(k string) (string, bool) {
				asked = append(asked, k)
				if k == "OTEL_SDK_DISABLED" && c.sdk != "" {
					return c.sdk, true
				}
				return p.cert, true // would be a file path if read
			}
			on, _, errs := tracing.Resolve(c.in, lookup, p.fs.Read)
			if on || len(errs) != 0 {
				t.Fatalf("on=%v errors=%d", on, len(errs))
			}
			for _, k := range asked {
				if k != "OTEL_SDK_DISABLED" {
					t.Errorf("looked up %s while tracing is off", k)
				}
			}
			for _, path := range []string{p.cert, p.key, p.other, p.otherKey} {
				if n := p.fs.Calls(path); n != 0 {
					t.Errorf("reader called %d times while tracing is off", n)
				}
			}
		})
	}
}

// Unset variables never reach the reader.
func TestPEM_NothingReadWhenNoFileVariableSet(t *testing.T) {
	p := newMemPair(t)
	var calls int
	rd := func(path string, max int64) ([]byte, error) { calls++; return p.fs.Read(path, max) }
	_, _, errs := tracing.Resolve(onIn(), envMap(map[string]string{vExporter: "otlp"}), rd)
	if len(errs) != 0 || calls != 0 {
		t.Fatalf("errors=%d reader calls=%d", len(errs), calls)
	}
}

// AC-15 (behavioural): the process environment and filesystem are not
// consulted. Hostile real values change nothing, and the real filesystem is
// never touched when the injected reader is a stub.
func TestResolve_IgnoresProcessEnvironment(t *testing.T) {
	t.Setenv(vExporter, "console")
	t.Setenv(vQueue, "abc")
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "/etc/hostname")
	_, _, errs := resolveMem(onIn(), nil, memfs.New())
	if len(errs) != 0 {
		t.Fatalf("process environment leaked into Resolve: %d errors", len(errs))
	}
	var calls int
	rd := func(string, int64) ([]byte, error) { calls++; return nil, source.ErrUnreadable }
	_, _, errs = tracing.Resolve(onIn(), envMap(map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/etc/hostname"}), rd)
	requireSettings(t, errs, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE"})
	if calls != 1 {
		t.Fatalf("injected reader called %d times, want 1", calls)
	}
}

// The environment snapshot of T2 plugs in directly.
func TestResolve_WorksWithSourceEnvSnapshot(t *testing.T) {
	e := source.NewEnv([]string{vExporter + "=console", vQueue + "=abc", vQueue + "=100", "UNRELATED=1"})
	_, _, errs := tracing.Resolve(onIn(), e.Lookup, memfs.New().Read)
	// First occurrence wins: the queue size is the invalid "abc".
	requireSettings(t, errs, []string{vQueue, vExporter})
}
