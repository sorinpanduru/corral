//go:build functional

package functional

import (
	"net"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// expectConfigFailure runs the binary, requires a non-zero exit, exactly one
// JSON error record on stdout with component=config and the wanted setting,
// empty stderr, and no echo of the marker anywhere.
func expectConfigFailure(t *testing.T, env []string, args []string, setting, mk string) {
	t.Helper()
	p := startProc(t, env, args...)
	if code := p.waitExit(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	var cfg []map[string]any
	for _, r := range p.records() {
		if str(r, "component") == "config" {
			cfg = append(cfg, r)
		}
	}
	if len(cfg) != 1 {
		t.Fatalf("component=config records = %d, want exactly 1", len(cfg))
	}
	if got := str(cfg[0], "setting"); got != setting {
		t.Errorf("setting = %q, want %q", got, setting)
	}
	if str(cfg[0], "level") != "ERROR" {
		t.Errorf("level = %q, want ERROR", str(cfg[0], "level"))
	}
	if b := p.stderr.Bytes(); len(b) != 0 {
		t.Errorf("stderr has %d bytes, want empty", len(b))
	}
	if mk != "" {
		sc := markerscan.New()
		sc.AddMarker(t, mk)
		sc.Add(markerscan.SourceStdout, p.stdout.Bytes())
		sc.Add(markerscan.SourceStderr, p.stderr.Bytes())
		sc.AssertClean(t)
	}
}

// AC-21 (FR-27) invalid settings via flags, with a marker that must not be
// echoed.
func TestAC21_InvalidSetting(t *testing.T) {
	const mk = "fnmark-Zq7Lm2Xv9Ab4Cd6E"
	cases := []struct {
		name, setting string
		args          []string
	}{
		{"log.level", "log.level", []string{"-log.level=" + mk}},
		{"log.level offset", "log.level", []string{"-log.level=debug-4"}},
		{"log.format", "log.format", []string{"-log.format=" + mk}},
		{"admin.listen no host", "admin.listen", []string{"-admin.listen=:9187"}},
		{"admin.listen marker", "admin.listen", []string{"-admin.listen=" + mk}},
		{"tracing.enabled", "tracing.enabled", []string{"-tracing.enabled=" + mk}},
		{"tracing.sample_ratio", "tracing.sample_ratio", []string{"-tracing.sample_ratio=" + mk}},
		{"known flag without value", "log.level", []string{"-log.level"}},
		{"known flag twice", "log.level", []string{"-log.level=info", "-log.level=debug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectConfigFailure(t, baseEnv(), tc.args, tc.setting, mk)
		})
	}
}

// AC-21 with tracing enabled and an invalid OTEL_* value.
func TestAC21_InvalidOTEL(t *testing.T) {
	const mk = "otmark-Qw8Er5Ty2Ui0Op1A"
	cases := []struct {
		name, setting string
		env           []string
	}{
		{"protocol marker", "OTEL_EXPORTER_OTLP_PROTOCOL", []string{"OTEL_EXPORTER_OTLP_PROTOCOL=" + mk}},
		{"sampler", "OTEL_TRACES_SAMPLER", []string{"OTEL_TRACES_SAMPLER=jaeger_remote"}},
		{"exporter console", "OTEL_TRACES_EXPORTER", []string{"OTEL_TRACES_EXPORTER=console"}},
		{"compression", "OTEL_EXPORTER_OTLP_COMPRESSION", []string{"OTEL_EXPORTER_OTLP_COMPRESSION=zstd"}},
		{"sampler arg marker", "OTEL_TRACES_SAMPLER_ARG", []string{"OTEL_TRACES_SAMPLER=traceidratio", "OTEL_TRACES_SAMPLER_ARG=" + mk}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectConfigFailure(t, baseEnv(tc.env...),
				[]string{"-tracing.enabled=true", "-admin.listen=" + freeAddr(t)}, tc.setting, mk)
		})
	}
	// With tracing off the same values are ignored and corral starts.
	t.Run("tracing off ignores OTEL_*", func(t *testing.T) {
		addr := freeAddr(t)
		p := startProc(t, baseEnv("OTEL_TRACES_SAMPLER=jaeger_remote", "OTEL_EXPORTER_OTLP_PROTOCOL="+mk),
			"-admin.listen="+addr)
		p.waitReady(addr)
		if code := p.sigterm(); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})
}

// AC-21 / ADR-0003 amendment 1: unknown, malformed and positional arguments
// give setting=unknown / setting=argument and are never echoed.
func TestAC21_UnknownMalformedPositional(t *testing.T) {
	const mk = "argmark-Xc3Vb6Nm9Lk2Jh5G"
	cases := []struct {
		name, setting string
		args          []string
	}{
		{"unknown flag", "unknown", []string{"-" + mk + "=1"}},
		{"unknown flag double dash", "unknown", []string{"--" + mk}},
		{"unknown flag marker value", "unknown", []string{"-no.such.flag=" + mk}},
		{"malformed double dash", "unknown", []string{"--"}},
		{"malformed single dash", "unknown", []string{"-"}},
		{"malformed empty name", "unknown", []string{"-=" + mk}},
		{"positional", "argument", []string{mk}},
		{"positional after flag", "argument", []string{"-log.level=info", mk}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectConfigFailure(t, baseEnv(), tc.args, tc.setting, mk)
		})
	}
}

// AC-21 / ADR-0003 amendment 1: --name=value is accepted like -name=value.
func TestAC21_DoubleDashAccepted(t *testing.T) {
	addr := freeAddr(t)
	p := startProc(t, baseEnv(), "--admin.listen="+addr, "--log.level=DEBUG", "--log.format=json")
	p.waitReady(addr)
	if code := p.sigterm(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	p.records()
}

// AC-30 (FR-35): admin.listen already in use gives a non-zero exit and an error
// record with setting=admin.listen and no address text.
func TestAC30_AddressInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	addr := busy.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	p := startProc(t, baseEnv(), "-admin.listen="+addr)
	if code := p.waitExit(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	var cfg []map[string]any
	for _, r := range p.records() {
		if str(r, "component") == "config" {
			cfg = append(cfg, r)
		}
	}
	if len(cfg) != 1 || str(cfg[0], "setting") != "admin.listen" {
		t.Fatalf("want exactly one component=config record with setting=admin.listen, got %d", len(cfg))
	}
	if got := str(cfg[0], "corral.error.detail"); got != "EADDRINUSE" {
		t.Errorf("corral.error.detail = %q, want EADDRINUSE", got)
	}
	out := string(p.stdout.Bytes()) + string(p.stderr.Bytes())
	for _, bad := range []string{addr, ":" + port, "127.0.0.1"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains address text %q", bad)
		}
	}
	if b := p.stderr.Bytes(); len(b) != 0 {
		t.Errorf("stderr has %d bytes, want empty", len(b))
	}
	for _, m := range lifecycleMsgs(p.records()) {
		if strings.Contains(strings.ToLower(m), "startup complete") {
			t.Errorf("startup-complete record after a bind failure")
		}
	}
}
