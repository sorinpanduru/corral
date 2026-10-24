package configload_test

import (
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/app/configload"
	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// ---- successful outcome ----

func TestLoad_ValidOutcome(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true", "--tracing.sample_ratio=0.25", "--log.level=debug", "--t.x=v"}, nil, nil)
	requireNames(t, out)
	if out.Mode != loader.ModeRun {
		t.Errorf("mode = %v, want run", out.Mode)
	}
	want := settings.Observability{LogFormat: logging.FormatJSON, LogLevel: slog.LevelDebug, AdminListen: "127.0.0.1:9187", TracingEnabled: true, SampleRatio: 0.25}
	if out.Observability != want {
		t.Errorf("Observability = %+v, want %+v", out.Observability, want)
	}
	if !out.TracingEnabled {
		t.Error("TracingEnabled = false")
	}
	if out.Tracing.Exporter != tracing.ExporterOTLP || out.Tracing.Protocol != tracing.ProtocolHTTPProtobuf || out.Tracing.SampleRatio != 0.25 {
		t.Errorf("Tracing = %+v", out.Tracing)
	}
	if out.Tracing.Queue.MaxQueueSize != 2048 || out.Tracing.Queue.MaxExportBatchSize != 512 {
		t.Errorf("Queue = %+v, want the FR-37 defaults", out.Tracing.Queue)
	}
	if !reflect.DeepEqual(out.Result.Supplied, []string{"log.level", "t.x", "tracing.enabled", "tracing.sample_ratio"}) {
		t.Errorf("Supplied = %v", out.Result.Supplied)
	}
}

func TestLoad_DefaultsTracingOff(t *testing.T) {
	out := run(t, nil, nil, nil)
	requireNames(t, out)
	if out.TracingEnabled {
		t.Error("tracing on by default")
	}
	if out.Observability != settings.DefaultObservability() {
		t.Errorf("Observability = %+v, want the defaults", out.Observability)
	}
	if len(out.TLSIgnored) != 0 {
		t.Errorf("TLSIgnored = %v", out.TLSIgnored)
	}
}

// The sampler variables take precedence over tracing.sample_ratio (FR-21).
func TestLoad_SamplerArgOverridesRatio(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true", "--tracing.sample_ratio=1.0"}, []string{"OTEL_TRACES_SAMPLER_ARG=0.1"}, nil)
	requireNames(t, out)
	if out.Tracing.SampleRatio != 0.1 {
		t.Errorf("SampleRatio = %v, want 0.1", out.Tracing.SampleRatio)
	}
}

// ---- known / unknown tracing.enabled (FR-19) ----

// AC-6 (unit): all four groups in order, with exact names.
func TestAC6_OrderAcrossAllFourGroups(t *testing.T) {
	m := newMarker(t)
	out := run(t,
		[]string{"--bogus=" + m, "--log.level=" + m, "--admin.listen=" + m, "--tracing.enabled=true"},
		[]string{"CORRAL_TRACING_SAMPLE_RATIO=" + m, "OTEL_TRACES_SAMPLER=bad" + m, "OTEL_BSP_SCHEDULE_DELAY=x" + m, "OTEL_EXPORTER_OTLP_TIMEOUT=-" + m}, nil)
	requireNames(t, out,
		"unknown",                                                  // group 1
		"CORRAL_TRACING_SAMPLE_RATIO", "admin.listen", "log.level", // group 2, byte order
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_TRACES_SAMPLER", // group 4, sorted
	)
	requireNoMarker(t, out, m)
}

// Group 3 sits between 2 and 4.
func TestLoad_Group3BetweenGroup2AndGroup4(t *testing.T) {
	out := run(t, []string{"--t.n=500", "--t.x=\x01", "--tracing.enabled=true"}, []string{"OTEL_TRACES_SAMPLER=bad"}, nil)
	requireNames(t, out, "t.x", "t.n", "OTEL_TRACES_SAMPLER")
}

// FR-19: tracing.enabled unknown (invalid winning input) -> OTEL_* skipped.
func TestLoad_InvalidEnvTracingEnabledSkipsOTEL(t *testing.T) {
	out := run(t, nil, []string{"CORRAL_TRACING_ENABLED=maybe", "OTEL_TRACES_EXPORTER=console", "OTEL_BSP_MAX_QUEUE_SIZE=abc"}, nil)
	requireNames(t, out, "CORRAL_TRACING_ENABLED")
}

func TestLoad_InvalidFlagTracingEnabledSkipsOTEL(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=maybe"}, []string{"OTEL_TRACES_EXPORTER=console"}, nil)
	requireNames(t, out, "tracing.enabled")
}

// An invalid overridden CORRAL_TRACING_ENABLED does not make it unknown when a
// valid flag wins: OTEL_* is still validated, after the CORRAL record.
func TestLoad_OverriddenInvalidEnvDoesNotSkipOTEL(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true"}, []string{"CORRAL_TRACING_ENABLED=maybe", "OTEL_TRACES_SAMPLER=bad"}, nil)
	requireNames(t, out, "CORRAL_TRACING_ENABLED", "OTEL_TRACES_SAMPLER")
}

// A group 1 error on --tracing.enabled makes it unknown.
func TestLoad_Group1ErrorOnTracingEnabledSkipsOTEL(t *testing.T) {
	for name, args := range map[string][]string{
		"bare":     {"--tracing.enabled"},
		"repeated": {"--tracing.enabled=true", "--tracing.enabled=false"},
		"space":    {"--tracing.enabled", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			out := run(t, args, []string{"OTEL_TRACES_SAMPLER=bad", "CORRAL_TRACING_ENABLED=true"}, nil)
			for _, n := range names(out.Errors) {
				if strings.HasPrefix(n, "OTEL_") {
					t.Fatalf("OTEL_* validated although tracing.enabled is unknown: %v", names(out.Errors))
				}
			}
			if len(out.Errors) == 0 {
				t.Fatal("no errors")
			}
		})
	}
}

// tracing.enabled from env alone (valid) turns validation on.
func TestLoad_TracingEnabledFromEnvValidatesOTEL(t *testing.T) {
	out := run(t, nil, []string{"CORRAL_TRACING_ENABLED=true", "OTEL_TRACES_SAMPLER=bad"}, nil)
	requireNames(t, out, "OTEL_TRACES_SAMPLER")
}

func TestLoad_TracingOffDoesNotValidateOTEL(t *testing.T) {
	out := run(t, nil, []string{"OTEL_TRACES_SAMPLER=bad", "OTEL_BSP_MAX_QUEUE_SIZE=abc"}, nil)
	requireNames(t, out)
	out = run(t, []string{"--tracing.enabled=false"}, []string{"OTEL_TRACES_EXPORTER=console"}, nil)
	requireNames(t, out)
}

// OTEL_SDK_DISABLED=true: no OTEL_* errors; tracing.enabled stays supplied.
func TestLoad_SDKDisabled(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true"}, []string{"OTEL_SDK_DISABLED=true", "OTEL_TRACES_SAMPLER=bad", "OTEL_BSP_MAX_QUEUE_SIZE=abc"}, nil)
	requireNames(t, out)
	if out.TracingEnabled {
		t.Error("TracingEnabled although OTEL_SDK_DISABLED=true")
	}
	found := false
	for _, s := range out.Result.Supplied {
		found = found || s == "tracing.enabled"
	}
	if !found {
		t.Errorf("tracing.enabled missing from Supplied: %v", out.Result.Supplied)
	}
}

// OTEL_* is outside the registry: never unknown, never supplied.
func TestLoad_OTELNeverUnknownOrSupplied(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true"}, []string{
		"OTEL_TRACES_EXPORTER=none", "OTEL_SERVICE_NAME=svc", "OTEL_RESOURCE_ATTRIBUTES=a=b", "OTEL_BSP_MAX_QUEUE_SIZE=100", "OTEL_UNRELATED=x", "CORRAL_TYPO=1",
	}, nil)
	requireNames(t, out)
	if !reflect.DeepEqual(out.Result.UnknownEnv, []string{"CORRAL_TYPO"}) {
		t.Errorf("UnknownEnv = %v", out.Result.UnknownEnv)
	}
	for _, s := range out.Result.Supplied {
		if strings.HasPrefix(strings.ToUpper(s), "OTEL") {
			t.Errorf("OTEL variable %s listed as supplied", s)
		}
	}
	// Even with tracing off and with errors elsewhere.
	out = run(t, []string{"--log.level=bad"}, []string{"OTEL_X=1"}, nil)
	if len(out.Result.UnknownEnv) != 0 {
		t.Errorf("UnknownEnv = %v", out.Result.UnknownEnv)
	}
}

// tracing.sample_ratio failed: the default ratio is used and OTEL_* is still
// validated (tracing.enabled is known).
func TestLoad_SampleRatioFailedStillValidatesOTEL(t *testing.T) {
	out := run(t, []string{"--tracing.enabled=true", "--tracing.sample_ratio=9"}, []string{"OTEL_TRACES_SAMPLER=bad"}, nil)
	requireNames(t, out, "tracing.sample_ratio", "OTEL_TRACES_SAMPLER")
}

// ---- AC-20 / AC-21 end to end, after a CORRAL record ----

func TestAC20_EndToEnd(t *testing.T) {
	fs := pemFS(t)
	fs.Put("/pem/junk", []byte("not pem\n"))
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"invalid queue skips batch check", []string{"OTEL_BSP_MAX_QUEUE_SIZE=abc", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE=4096"}, []string{"OTEL_BSP_MAX_QUEUE_SIZE"}},
		{"non-PEM client certificate skips pairing",
			[]string{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=/pem/junk", "OTEL_EXPORTER_OTLP_CLIENT_KEY=/pem/client.key"}, []string{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"}},
		{"queue 100, batch 200", []string{"OTEL_BSP_MAX_QUEUE_SIZE=100", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE=200"}, []string{"OTEL_BSP_MAX_EXPORT_BATCH_SIZE"}},
		// Amendment 2: an invalid sampler skips the SAMPLER_ARG check.
		{"invalid sampler skips its argument", []string{"OTEL_TRACES_SAMPLER=x", "OTEL_TRACES_SAMPLER_ARG=9"}, []string{"OTEL_TRACES_SAMPLER"}},
		{"valid sampler checks its argument", []string{"OTEL_TRACES_SAMPLER=traceidratio", "OTEL_TRACES_SAMPLER_ARG=9"}, []string{"OTEL_TRACES_SAMPLER_ARG"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := run(t, []string{"--log.level=bad", "--tracing.enabled=true"}, c.env, fs)
			requireNames(t, out, append([]string{"log.level"}, c.want...)...)
			// Alone: only the OTEL records.
			out = run(t, []string{"--tracing.enabled=true"}, c.env, fs)
			requireNames(t, out, c.want...)
		})
	}
}

func TestAC21_EndToEnd(t *testing.T) {
	env := []string{"OTEL_TRACES_EXPORTER=console", "OTEL_EXPORTER_OTLP_PROTOCOL=http/json", "OTEL_EXPORTER_OTLP_HEADERS=novalue"}
	out := run(t, []string{"--log.level=bad", "--tracing.enabled=true"}, env, nil)
	requireNames(t, out, "log.level", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_TRACES_EXPORTER")
	out = run(t, []string{"--tracing.enabled=true"}, env, nil)
	requireNames(t, out, "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_TRACES_EXPORTER")
}

// ---- AC-22 / FR-41 names ----

// An endpoint of only U+00A0 counts as not set, so a CA variable ignored by a
// plaintext endpoint produces no FR-41 name.
func TestAC22_NBSPEndpointNoFR41(t *testing.T) {
	fs := pemFS(t)
	out := run(t, []string{"--tracing.enabled=true"}, []string{"OTEL_EXPORTER_OTLP_ENDPOINT=\u00a0", "OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt"}, fs)
	requireNames(t, out)
	if len(out.TLSIgnored) != 0 {
		t.Fatalf("TLSIgnored = %v, want none", out.TLSIgnored)
	}
	// Every FR-7 whitespace character behaves alike.
	for _, ws := range []string{" ", "\t", "\u0085", "\u2003", "\u3000", " \u00a0 "} {
		out := run(t, []string{"--tracing.enabled=true"}, []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + ws, "OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt"}, fs)
		if len(out.Errors) != 0 || len(out.TLSIgnored) != 0 {
			t.Errorf("whitespace endpoint (length %d): errors %d, TLSIgnored %v", len(ws), len(out.Errors), out.TLSIgnored)
		}
	}
}

// The contrast: a real http:// endpoint with TLS material names it (names
// only), https does not.
func TestFR41_NamesForPlaintextEndpoint(t *testing.T) {
	fs := pemFS(t)
	tls := []string{"OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=/pem/ca.crt", "OTEL_EXPORTER_OTLP_CLIENT_KEY=/pem/client.key"}
	out := run(t, []string{"--tracing.enabled=true"}, append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.invalid:4318"}, tls...), fs)
	requireNames(t, out)
	want := []string{"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY"}
	if !reflect.DeepEqual(out.TLSIgnored, want) {
		t.Fatalf("TLSIgnored = %v, want %v", out.TLSIgnored, want)
	}
	out = run(t, []string{"--tracing.enabled=true"}, append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.invalid:4318"}, tls...), fs)
	requireNames(t, out)
	if len(out.TLSIgnored) != 0 {
		t.Fatalf("https endpoint: TLSIgnored = %v", out.TLSIgnored)
	}
	// Tracing off: nothing to report.
	out = run(t, nil, append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.invalid:4318"}, tls...), fs)
	if len(out.TLSIgnored) != 0 {
		t.Fatalf("tracing off: TLSIgnored = %v", out.TLSIgnored)
	}
}

// AC-13 (unit): the FR-41 names are computed in check-config mode too.
func TestAC13_CheckConfigComputesTLSNames(t *testing.T) {
	fs := pemFS(t)
	out := run(t, []string{"--check-config", "--tracing.enabled=true"}, []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.invalid:4318", "OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt"}, fs)
	if out.Mode != loader.ModeCheckConfig {
		t.Fatalf("mode = %v", out.Mode)
	}
	requireNames(t, out)
	if !reflect.DeepEqual(out.TLSIgnored, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE"}) {
		t.Fatalf("TLSIgnored = %v", out.TLSIgnored)
	}
	// Check-config validates everything a run would, including OTEL_*.
	out = run(t, []string{"--check-config", "--tracing.enabled=true"}, []string{"OTEL_TRACES_SAMPLER=bad"}, nil)
	requireNames(t, out, "OTEL_TRACES_SAMPLER")
}

// ---- modes ----

func TestHelpAndVersionTouchNothing(t *testing.T) {
	fs := pemFS(t)
	var calls int
	rd := func(p string, max int64) ([]byte, error) { calls++; return fs.Read(p, max) }
	for _, c := range []struct {
		args []string
		want loader.Mode
	}{
		{[]string{"--help"}, loader.ModeHelp}, {[]string{"-h", "--bogus"}, loader.ModeHelp},
		{[]string{"--version"}, loader.ModeVersion}, {[]string{"--version", "--log.level=bad", "pos"}, loader.ModeVersion},
		{[]string{"--check-config", "--help"}, loader.ModeHelp},
	} {
		out := configload.Load(reg(t), configload.Inputs{Args: c.args, ReadFile: rd, Env: []string{
			"OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt", "CORRAL_T_SECRET_FILE=/pem/ca.crt", "CORRAL_LOG_LEVEL=bad", "CORRAL_TYPO=1", "OTEL_TRACES_SAMPLER=bad",
		}})
		if out.Mode != c.want {
			t.Errorf("args %v: mode %v, want %v", c.args, out.Mode, c.want)
		}
		if len(out.Errors) != 0 {
			t.Errorf("args %v: %d errors in help or version mode", c.args, len(out.Errors))
		}
	}
	if calls != 0 {
		t.Fatalf("the file reader was called %d times in help or version mode", calls)
	}
}

// ---- file reads: each reference once, with the right limits ----

func TestLoad_ReaderLimitsAndReadOnce(t *testing.T) {
	c, k := pemPair(t)
	fs := memfs.New()
	fs.Put("/pem/ca.crt", c)
	fs.Put("/pem/client.key", k)
	fs.Put("/sec", []byte("secretvalue\n"))
	limits := map[string][]int64{}
	rd := func(p string, max int64) ([]byte, error) { limits[p] = append(limits[p], max); return fs.Read(p, max) }
	out := configload.Load(reg(t), configload.Inputs{
		Args: []string{"--tracing.enabled=true", "--t.secret_file=/sec"},
		Env: []string{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=/pem/ca.crt", "OTEL_EXPORTER_OTLP_CLIENT_KEY=/pem/client.key",
			"OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt"},
		ReadFile: rd,
	})
	requireNames(t, out)
	if v, _ := out.Result.Get("t.secret"); v != "secretvalue" {
		t.Errorf("secret from file not loaded")
	}
	for p, want := range map[string]int64{"/sec": source.MaxSecretFileBytes, "/pem/ca.crt": source.MaxPEMFileBytes, "/pem/client.key": source.MaxPEMFileBytes} {
		got := limits[p]
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: reader calls with limits %v, want one call with %d", p, got, want)
		}
	}
}

// Nothing is read for OTEL_* when tracing is off.
func TestLoad_NoPEMReadWhenTracingOff(t *testing.T) {
	fs := pemFS(t)
	out := run(t, nil, []string{"OTEL_EXPORTER_OTLP_CERTIFICATE=/pem/ca.crt"}, fs)
	requireNames(t, out)
	if fs.Calls("/pem/ca.crt") != 0 {
		t.Fatalf("PEM read %d times with tracing off", fs.Calls("/pem/ca.crt"))
	}
}

// ---- determinism, injection, programming errors ----

func TestLoad_Deterministic(t *testing.T) {
	args := []string{"--bogus", "--log.level=x", "--tracing.enabled=true", "pos", "--t.n=500"}
	env := []string{"OTEL_TRACES_SAMPLER=bad", "OTEL_BSP_SCHEDULE_DELAY=0", "CORRAL_TYPO=1", "CORRAL_A-B=1", "CORRAL_ADMIN_LISTEN=x"}
	first := run(t, args, env, nil)
	for i := 0; i < 20; i++ {
		if again := run(t, args, env, nil); !reflect.DeepEqual(first, again) {
			t.Fatal("two runs with identical inputs differ")
		}
	}
}

func TestLoad_IgnoresProcessGlobals(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	t.Setenv("CORRAL_LOG_LEVEL", "bad")
	t.Setenv("OTEL_SDK_DISABLED", "false")
	out := run(t, []string{"--tracing.enabled=true"}, nil, nil)
	requireNames(t, out)
}

// Programming errors (a registry without the tracing settings, a nil file
// reader) are not configuration errors: they come back in Outcome.Err, a
// non-config error, never in Outcome.Errors, so the runtime maps them to exit
// 1 and never to the FR-11 records or exit 2.
func TestLoad_ProgrammingErrorsAreSeparate(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	cases := map[string]configload.Outcome{
		"registry without tracing settings": configload.Load(onlyTGroup(t), configload.Inputs{ReadFile: memfs.New().Read}),
		"nil file reader":                   configload.Load(reg(t), configload.Inputs{Args: []string{"--tracing.enabled=true"}}),
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if out.Err == nil {
				t.Fatal("Outcome.Err is nil")
			}
			var ce *config.Error
			if errors.As(out.Err, &ce) {
				t.Errorf("a programming error is a *config.Error (setting %q)", ce.Setting)
			}
			if len(out.Errors) != 0 {
				t.Errorf("programming error also reported as %d configuration errors", len(out.Errors))
			}
		})
	}
	// Ordinary outcomes, valid or not, have no programming error.
	if out := run(t, []string{"--log.level=bad"}, nil, nil); out.Err != nil || len(out.Errors) != 1 {
		t.Errorf("config error outcome: Err=%v, %d errors", out.Err, len(out.Errors))
	}
	if out := run(t, nil, nil, nil); out.Err != nil {
		t.Errorf("valid outcome has Err=%v", out.Err)
	}
	// Help and version never need the reader.
	if out := configload.Load(reg(t), configload.Inputs{Args: []string{"--help"}}); out.Err != nil || out.Mode != loader.ModeHelp {
		t.Errorf("help with a nil reader: Err=%v mode=%v", out.Err, out.Mode)
	}
}
