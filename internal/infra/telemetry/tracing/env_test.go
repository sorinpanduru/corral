package tracing_test

// Tests for T10: tracing on/off resolution and OTEL_* validation.
// Expected API (plans/SPEC-001.md T10, names of constants are the test-writer's
// reading of the plan; see the task report):
//
//	tracing.LookupEnv, tracing.Resolve(config.Settings, LookupEnv) (bool, EnvConfig, error)
//	tracing.EnvConfig{Exporter, Protocol, Sampler, SamplerArg *float64, SampleRatio float64, Queue}
//	tracing.ExporterOTLP/ExporterNone, tracing.ProtocolHTTPProtobuf/ProtocolGRPC,
//	tracing.SamplerUnset/SamplerAlwaysOn/SamplerAlwaysOff/SamplerTraceIDRatio/
//	SamplerParentBasedAlwaysOn/SamplerParentBasedAlwaysOff/SamplerParentBasedTraceIDRatio.

import (
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing"
)

// FR-19: OTEL_SDK_DISABLED=true (case-insensitive) turns tracing off whatever
// tracing.enabled says, and nothing else is validated.
func TestResolve_SDKDisabledTrueDisables(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "True", "tRuE"} {
		t.Run(v, func(t *testing.T) {
			m := map[string]string{
				"OTEL_SDK_DISABLED":    v,
				"OTEL_TRACES_EXPORTER": "console", // invalid, but ignored when off
			}
			on, _, err := envResolveOn(m)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if on {
				t.Fatalf("enabled = true, want false with OTEL_SDK_DISABLED=%s", v)
			}
		})
	}
}

// FR-19, AC-14 (unit): any other value of OTEL_SDK_DISABLED, empty included,
// counts as false and is never an error.
func TestResolve_SDKDisabledOtherValuesAreFalse(t *testing.T) {
	for _, v := range []string{"yes", "1", "false", "FALSE", "", "on", "truee", "enabled", "0", "t"} {
		t.Run("value="+v, func(t *testing.T) {
			on, _, err := envResolveOn(map[string]string{"OTEL_SDK_DISABLED": v})
			if err != nil {
				t.Fatalf("OTEL_SDK_DISABLED=%q: err = %v, want nil", v, err)
			}
			if !on {
				t.Fatalf("OTEL_SDK_DISABLED=%q: enabled = false, want true (counts as false)", v)
			}
			// It cannot switch tracing on when tracing.enabled is false.
			on, _, err = tracing.Resolve(config.Defaults(), envMap(map[string]string{"OTEL_SDK_DISABLED": v}))
			if err != nil || on {
				t.Fatalf("tracing.enabled=false: on=%v err=%v, want false/nil", on, err)
			}
		})
	}
}

// AC-14 (unit) through the real process environment API: yes is false, no error.
func TestResolve_SDKDisabledYesViaRealEnv(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "yes")
	on, _, err := tracing.Resolve(envOn(1.0), lookupOS)
	if err != nil || !on {
		t.Fatalf("on=%v err=%v, want true/nil", on, err)
	}
}

// FR-19: tracing is off by default.
func TestResolve_OffByDefault(t *testing.T) {
	on, _, err := tracing.Resolve(config.Defaults(), envMap(nil))
	if err != nil || on {
		t.Fatalf("defaults: on=%v err=%v, want false/nil", on, err)
	}
}

// FR-27, C3: when tracing is off (settings or SDK_DISABLED) nothing except
// OTEL_SDK_DISABLED is read, so nothing can be validated.
func TestResolve_WhenOffOnlySDKDisabledIsRead(t *testing.T) {
	cases := []struct {
		name string
		s    config.Settings
		sdk  string
	}{
		{"tracing.enabled=false", config.Defaults(), "false"},
		{"tracing.enabled=false SDK unset", config.Defaults(), ""},
		{"SDK_DISABLED=TRUE", envOn(1.0), "TRUE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			lookup := func(k string) (string, bool) {
				asked = append(asked, k)
				if k == "OTEL_SDK_DISABLED" {
					if c.sdk == "" {
						return "", false
					}
					return c.sdk, true
				}
				return "garbage", true
			}
			on, _, err := tracing.Resolve(c.s, lookup)
			if err != nil || on {
				t.Fatalf("on=%v err=%v, want false/nil", on, err)
			}
			for _, k := range asked {
				if k != "OTEL_SDK_DISABLED" {
					t.Errorf("read %s while tracing is off", k)
				}
			}
		})
	}
}

// FR-27, C3: every invalid value is ignored when tracing is off.
func TestResolve_InvalidIgnoredWhenOff(t *testing.T) {
	for _, c := range envInvalidCases(t) {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{c.setting: c.value("whatever")}
			for k, v := range c.extra {
				m[k] = v
			}
			if on, _, err := tracing.Resolve(config.Defaults(), envMap(m)); err != nil || on {
				t.Fatalf("tracing.enabled=false: on=%v err=%v, want false/nil", on, err)
			}
			m["OTEL_SDK_DISABLED"] = "TRUE"
			if on, _, err := envResolveOn(m); err != nil || on {
				t.Fatalf("SDK_DISABLED=TRUE: on=%v err=%v, want false/nil", on, err)
			}
		})
	}
}

// FR-20 defaults: with no OTEL_* set, otlp, http/protobuf, FR-37 defaults and
// the default sampler with tracing.sample_ratio.
func TestResolve_Defaults(t *testing.T) {
	on, cfg, err := tracing.Resolve(envOn(0.25), envMap(nil))
	if err != nil || !on {
		t.Fatalf("on=%v err=%v, want true/nil", on, err)
	}
	if cfg.Exporter != tracing.ExporterOTLP {
		t.Errorf("Exporter = %v, want otlp", cfg.Exporter)
	}
	if cfg.Protocol != tracing.ProtocolHTTPProtobuf {
		t.Errorf("Protocol = %v, want http/protobuf", cfg.Protocol)
	}
	if cfg.Sampler != tracing.SamplerUnset {
		t.Errorf("Sampler = %v, want unset", cfg.Sampler)
	}
	if cfg.SamplerArg != nil {
		t.Errorf("SamplerArg = %v, want nil", *cfg.SamplerArg)
	}
	if cfg.SampleRatio != 0.25 {
		t.Errorf("SampleRatio = %v, want tracing.sample_ratio 0.25", cfg.SampleRatio)
	}
	want := tracing.QueueSettings{
		MaxQueueSize: 2048, MaxExportBatchSize: 512,
		ScheduleDelay: 5000 * time.Millisecond, ExportTimeout: 30000 * time.Millisecond,
	}
	if cfg.Queue != want {
		t.Errorf("Queue = %+v, want %+v", cfg.Queue, want)
	}
}

// FR-20 (B4): OTEL_TRACES_EXPORTER accepts otlp (default) and none.
func TestResolve_Exporter(t *testing.T) {
	if cfg := envMustResolve(t, map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}); cfg.Exporter != tracing.ExporterOTLP {
		t.Errorf("otlp: Exporter = %v", cfg.Exporter)
	}
	cfg := envMustResolve(t, map[string]string{"OTEL_TRACES_EXPORTER": "none"})
	if cfg.Exporter != tracing.ExporterNone {
		t.Errorf("none: Exporter = %v", cfg.Exporter)
	}
}

// FR-20: protocol selection and precedence of the _TRACES_ variant.
func TestResolve_Protocol(t *testing.T) {
	const gen, trc = "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	cases := []struct {
		name string
		env  map[string]string
		want tracing.Protocol
	}{
		{"unset", nil, tracing.ProtocolHTTPProtobuf},
		{"general http/protobuf", map[string]string{gen: "http/protobuf"}, tracing.ProtocolHTTPProtobuf},
		{"general grpc", map[string]string{gen: "grpc"}, tracing.ProtocolGRPC},
		{"traces grpc", map[string]string{trc: "grpc"}, tracing.ProtocolGRPC},
		{"traces over general (grpc over http)", map[string]string{gen: "http/protobuf", trc: "grpc"}, tracing.ProtocolGRPC},
		{"traces over general (http over grpc)", map[string]string{gen: "grpc", trc: "http/protobuf"}, tracing.ProtocolHTTPProtobuf},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if cfg := envMustResolve(t, c.env); cfg.Protocol != c.want {
				t.Errorf("Protocol = %v, want %v", cfg.Protocol, c.want)
			}
		})
	}
}

// FR-37: OTEL_BSP_* mapping to QueueSettings, durations in milliseconds.
func TestResolve_QueueMapping(t *testing.T) {
	cfg := envMustResolve(t, map[string]string{
		"OTEL_BSP_MAX_QUEUE_SIZE":        "100",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "50",
		"OTEL_BSP_SCHEDULE_DELAY":        "250",
		"OTEL_BSP_EXPORT_TIMEOUT":        "1500",
	})
	want := tracing.QueueSettings{MaxQueueSize: 100, MaxExportBatchSize: 50,
		ScheduleDelay: 250 * time.Millisecond, ExportTimeout: 1500 * time.Millisecond}
	if cfg.Queue != want {
		t.Errorf("Queue = %+v, want %+v", cfg.Queue, want)
	}
	// Partial: unspecified fields keep their defaults.
	cfg = envMustResolve(t, map[string]string{"OTEL_BSP_SCHEDULE_DELAY": "10"})
	if cfg.Queue.ScheduleDelay != 10*time.Millisecond || cfg.Queue.MaxQueueSize != 2048 ||
		cfg.Queue.MaxExportBatchSize != 512 || cfg.Queue.ExportTimeout != 30*time.Second {
		t.Errorf("Queue = %+v, want only ScheduleDelay changed", cfg.Queue)
	}
	// Batch equal to queue is allowed (batch <= queue).
	envMustResolve(t, map[string]string{"OTEL_BSP_MAX_QUEUE_SIZE": "64", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "64"})
}

// FR-37 / AC-21: batch size larger than the queue is rejected, including
// against the defaults (batch 512 vs queue 2048), naming the batch variable.
func TestResolve_BatchLargerThanQueue(t *testing.T) {
	const batch = "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"
	for name, m := range map[string]map[string]string{
		"explicit both":          {"OTEL_BSP_MAX_QUEUE_SIZE": "10", batch: "11"},
		"batch above default q":  {batch: "4096"},
		"queue below default b.": {"OTEL_BSP_MAX_QUEUE_SIZE": "100"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := envResolveOn(m)
			envRequireRejected(t, err, batch, envMarker(t))
		})
	}
}

// FR-21 (OPEN-6): sampler values, ARG precedence over tracing.sample_ratio.
func TestResolve_SamplerAcceptsSixValues(t *testing.T) {
	cases := map[string]tracing.SamplerKind{
		"always_on":                tracing.SamplerAlwaysOn,
		"always_off":               tracing.SamplerAlwaysOff,
		"traceidratio":             tracing.SamplerTraceIDRatio,
		"parentbased_always_on":    tracing.SamplerParentBasedAlwaysOn,
		"parentbased_always_off":   tracing.SamplerParentBasedAlwaysOff,
		"parentbased_traceidratio": tracing.SamplerParentBasedTraceIDRatio,
	}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			cfg := envMustResolve(t, map[string]string{"OTEL_TRACES_SAMPLER": v})
			if cfg.Sampler != want {
				t.Errorf("Sampler = %v, want %v", cfg.Sampler, want)
			}
		})
	}
}

// AC-16: OTEL_TRACES_SAMPLER=always_on wins over tracing.sample_ratio=0.0.
func TestResolve_SamplerOverridesSettingsRatio(t *testing.T) {
	on, cfg, err := tracing.Resolve(envOn(0.0), envMap(map[string]string{"OTEL_TRACES_SAMPLER": "always_on"}))
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	if cfg.Sampler != tracing.SamplerAlwaysOn {
		t.Errorf("Sampler = %v, want always_on", cfg.Sampler)
	}
}

// AC-16, FR-21: ARG replaces tracing.sample_ratio; an unset ARG keeps it.
func TestResolve_SamplerArgPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		sampler string // "" = unset
		arg     string // "" = unset
		setting float64
		want    float64
	}{
		{"arg 0.0 over ratio 1.0 (default sampler)", "", "0.0", 1.0, 0.0},
		{"arg 1 over ratio 0.0 (default sampler)", "", "1", 0.0, 1.0},
		{"arg 0.5 over ratio 0.1 traceidratio", "traceidratio", "0.5", 0.1, 0.5},
		{"arg 0.75 over ratio 0.1 parentbased_traceidratio", "parentbased_traceidratio", "0.75", 0.1, 0.75},
		{"arg unset keeps ratio (default sampler)", "", "", 0.3, 0.3},
		{"arg unset keeps ratio (traceidratio)", "traceidratio", "", 0.3, 0.3},
		{"arg unset keeps ratio 0.0", "parentbased_traceidratio", "", 0.0, 0.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{}
			if c.sampler != "" {
				m["OTEL_TRACES_SAMPLER"] = c.sampler
			}
			if c.arg != "" {
				m["OTEL_TRACES_SAMPLER_ARG"] = c.arg
			}
			on, cfg, err := tracing.Resolve(envOn(c.setting), envMap(m))
			if err != nil || !on {
				t.Fatalf("on=%v err=%v", on, err)
			}
			if cfg.SampleRatio != c.want {
				t.Errorf("SampleRatio = %v, want %v", cfg.SampleRatio, c.want)
			}
			if c.arg == "" && cfg.SamplerArg != nil {
				t.Errorf("SamplerArg = %v, want nil when ARG is unset", *cfg.SamplerArg)
			}
			if c.arg != "" && (cfg.SamplerArg == nil || *cfg.SamplerArg != c.want) {
				t.Errorf("SamplerArg = %v, want %v", cfg.SamplerArg, c.want)
			}
		})
	}
}

// FR-21: ARG is ignored (not validated) when the effective sampler uses no ratio.
func TestResolve_SamplerArgIgnoredWithoutRatioSampler(t *testing.T) {
	for _, s := range []string{"always_on", "always_off", "parentbased_always_on", "parentbased_always_off"} {
		for _, arg := range []string{"7.5", "-1", "garbage", "NaN"} {
			t.Run(s+"/"+arg, func(t *testing.T) {
				cfg := envMustResolve(t, map[string]string{"OTEL_TRACES_SAMPLER": s, "OTEL_TRACES_SAMPLER_ARG": arg})
				if cfg.SampleRatio != 1.0 {
					t.Errorf("SampleRatio = %v, want tracing.sample_ratio 1.0 (ARG ignored)", cfg.SampleRatio)
				}
			})
		}
	}
}

// FR-20: OTEL_SERVICE_NAME is any non-empty string; empty counts as unset.
func TestResolve_ServiceNameNeverRejected(t *testing.T) {
	for _, v := range []string{"", "corral", "my service with spaces", "名前", "a=b,c", "x%zz"} {
		envMustResolve(t, map[string]string{"OTEL_SERVICE_NAME": v})
	}
}

// FR-20: valid values of every honoured / validated variable are accepted.
func TestResolve_ValidValuesAccepted(t *testing.T) {
	certs := envGenCerts(t)
	for _, c := range envValidCases(certs) {
		t.Run(c.setting+"="+c.label, func(t *testing.T) {
			m := map[string]string{c.setting: c.value}
			for k, v := range c.extra {
				m[k] = v
			}
			envMustResolve(t, m)
		})
	}
}

// FR-20: the _TRACES_ variants are honoured and validated like the general ones.
func TestResolve_TracesVariantsValid(t *testing.T) {
	certs := envGenCerts(t)
	envMustResolve(t, map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":           "https://collector.invalid:4318/v1/traces",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":            "k=v%20w",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT":            "2500",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION":        "gzip",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE":           "False",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE":        certs.Cert,
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE": certs.Cert,
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY":         certs.Key,
	})
}

// FR-27, C3, AC-21 (part 2), AC-14, AC-16: each invalid value is a fatal
// config.Error naming the variable when tracing is on, with no value echo.
func TestResolve_InvalidRejectedNamingVariable(t *testing.T) {
	for _, c := range envInvalidCases(t) {
		t.Run(c.name, func(t *testing.T) {
			marker := envMarker(t)
			m := map[string]string{c.setting: c.value(marker)}
			for k, v := range c.extra {
				m[k] = v
			}
			_, _, err := envResolveOn(m)
			envRequireRejected(t, err, c.setting, marker)
		})
	}
}

// FR-27, AC-21: the whole supplied value being a marker is rejected without
// echo for every variable whose bare marker is invalid, including via the
// _TRACES_ variants and when the other variant is valid.
func TestResolve_BareMarkerRejectedEverywhere(t *testing.T) {
	vars := []string{
		"OTEL_TRACES_EXPORTER", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		"OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE",
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_BSP_EXPORT_TIMEOUT",
	}
	for _, v := range vars {
		t.Run(v, func(t *testing.T) {
			marker := envMarker(t)
			_, _, err := envResolveOn(map[string]string{v: marker})
			envRequireRejected(t, err, v, marker)
		})
	}
}

// FR-20 / FR-27: both variants are validated; a valid _TRACES_ variant does not
// hide an invalid general variable.
func TestResolve_GeneralVariantValidatedWhenTracesVariantValid(t *testing.T) {
	certs := envGenCerts(t)
	pairs := []struct{ gen, trc, good, bad string }{
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://c.invalid:4318", "ftp://c.invalid"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "a=b", "novalue"},
		{"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "10", "-5"},
		{"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "gzip", "zstd"},
		{"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true", "yes"},
		{"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", certs.Cert, certs.Key + ".missing"},
	}
	for _, p := range pairs {
		t.Run(p.gen, func(t *testing.T) {
			_, _, err := envResolveOn(map[string]string{p.gen: p.bad, p.trc: p.good})
			envRequireRejected(t, err, p.gen, envMarker(t))
		})
		t.Run(p.trc, func(t *testing.T) {
			_, _, err := envResolveOn(map[string]string{p.gen: p.good, p.trc: p.bad})
			envRequireRejected(t, err, p.trc, envMarker(t))
		})
	}
}
