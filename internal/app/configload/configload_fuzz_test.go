package configload_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/app/configload"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// FuzzLoad drives Load with arbitrary arguments and environment values over
// the test registry and an in-memory reader. It checks: no panic; two runs
// give identical outcomes; no supplied text is echoed into any rendering of
// the outcome; and the four error groups appear in FR-13 order, with groups 2
// and 4 sorted.

var fuzzEnvKeys = []string{
	"CORRAL_LOG_LEVEL", "CORRAL_LOG_FORMAT", "CORRAL_ADMIN_LISTEN", "CORRAL_TRACING_ENABLED", "CORRAL_TRACING_SAMPLE_RATIO",
	"CORRAL_T_X", "CORRAL_T_N", "CORRAL_T_SECRET", "CORRAL_T_SECRET_FILE", "CORRAL_TYPO", "CORRAL_",
	"OTEL_TRACES_EXPORTER", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY", "OTEL_BSP_MAX_QUEUE_SIZE",
	"OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "OTEL_SDK_DISABLED", "OTEL_RESOURCE_ATTRIBUTES",
}

// rank is the FR-13 group of an error: 1 command line, 2 per input, 3 cross
// rule, 4 OTEL_*.
func rank(e error) int {
	var ce *config.Error
	if !errors.As(e, &ce) {
		return 0
	}
	switch {
	case errors.Is(e, loader.ErrUnknownFlag), errors.Is(e, loader.ErrPositional), errors.Is(e, loader.ErrMissingValue),
		errors.Is(e, loader.ErrRepeated), errors.Is(e, loader.ErrSecretOnCommandLine), errors.Is(e, loader.ErrControlFlagValue):
		return 1
	case errors.Is(e, loader.ErrCrossRule):
		return 3
	case strings.HasPrefix(ce.Setting, "OTEL_"):
		return 4
	}
	return 2
}

func FuzzLoad(f *testing.F) {
	f.Add("--log.level=debug", "--tracing.enabled=true", "x", "y", uint8(0), uint8(11))
	f.Add("--t.n=500", "--bogus", "bad", "abc", uint8(6), uint8(20))
	f.Add("--tracing.enabled=true", "pos", "\xff", " ", uint8(15), uint8(13))
	f.Add("--t.secret=v", "--t.secret_file=/p", "/p", "/p", uint8(7), uint8(8))
	f.Add("--check-config", "--tracing.enabled=true", "http://c.invalid", "/pem/ca.crt", uint8(15), uint8(17))
	f.Add("--help", "--version", "", "", uint8(0), uint8(1))

	f.Fuzz(func(t *testing.T, a, b, v1, v2 string, k1, k2 uint8) {
		args := []string{a, b}
		env := []string{
			fuzzEnvKeys[int(k1)%len(fuzzEnvKeys)] + "=" + v1,
			fuzzEnvKeys[int(k2)%len(fuzzEnvKeys)] + "=" + v2,
		}
		mk := func() *memfs.FS {
			fs := memfs.New()
			fs.Put("/p", []byte("content\n"))
			return fs
		}
		out1 := configload.Load(reg(t), configload.Inputs{Args: args, Env: env, ReadFile: mk().Read})
		out2 := configload.Load(reg(t), configload.Inputs{Args: args, Env: env, ReadFile: mk().Read})
		if !reflect.DeepEqual(out1, out2) {
			t.Fatal("two loads of identical inputs differ")
		}

		// --check-config changes the mode only: same errors, same result.
		cc := configload.Load(reg(t), configload.Inputs{Args: append(append([]string{}, args...), "--check-config"), Env: env, ReadFile: mk().Read})
		if out1.Mode == loader.ModeRun && cc.Mode == loader.ModeCheckConfig {
			if !reflect.DeepEqual(out1.Errors, cc.Errors) || !reflect.DeepEqual(out1.Result, cc.Result) {
				t.Fatal("--check-config changed the errors or the loader result")
			}
		}

		// Groups in order; groups 2 and 4 sorted by setting.
		last := 0
		var lastSetting string
		for _, e := range out1.Errors {
			r := rank(e)
			if r == 0 {
				t.Fatalf("error %T is not a config.Error", e)
			}
			if r < last {
				t.Fatalf("group %d record after group %d", r, last)
			}
			var ce *config.Error
			errors.As(e, &ce)
			if r == last && (r == 2 || r == 4) && ce.Setting < lastSetting {
				t.Fatalf("group %d not sorted by setting", r)
			}
			last, lastSetting = r, ce.Setting
		}

		// No supplied text is echoed. Names of settings, variables, sentinels
		// and fixed tokens are constants; supplied text is only checked when it
		// is not part of them.
		consts := []string{"unknown", "argument", "CORRAL_<invalid-name>"}
		for _, s := range reg(t).Settings() {
			consts = append(consts, s.Name, s.Env, s.FileFlag, s.FileEnv)
		}
		consts = append(consts, fuzzEnvKeys...)
		for _, e := range out1.Errors {
			consts = append(consts, e.Error())
		}
		constText := strings.Join(consts, "\x00")
		rendered := render(out1)
		supplied := append(append([]string{}, args...), v1, v2)
		for _, a := range args {
			_, val, _ := strings.Cut(a, "=")
			supplied = append(supplied, val)
		}
		for _, s := range supplied {
			if len(s) >= 6 && !strings.Contains(constText, s) && strings.Contains(rendered, s) {
				t.Fatalf("supplied text of length %d is echoed", len(s))
			}
		}
	})
}
