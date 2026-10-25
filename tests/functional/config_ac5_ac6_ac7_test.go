//go:build functional

package functional

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

// ---- AC-5: unknown flags and unknown CORRAL_ variables ----

func TestFnAC5_UnknownFlagMarker(t *testing.T) {
	m := fmark(t)
	for _, args := range [][]string{{"--" + m + "=1"}, {"-" + m}, {"--" + m + "=" + m}} {
		p := startProc(t, baseEnv(), args...)
		if code := p.waitExit(); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		recs := p.records()
		if len(recs) != 1 || str(recs[0], "setting") != "unknown" || str(recs[0], "level") != "ERROR" {
			t.Fatalf("records = %v", settingsOfRecs(recs))
		}
		requireStderrEmpty(t, p)
		scanClean(t, p, m)
	}
}

func TestFnAC5_UnknownEnvironmentVariablesWarnOnce(t *testing.T) {
	m := fmark(t)
	addr := freeAddr(t)
	p := startProc(t, baseEnv(
		"CORRAL_SERVICE_HOST="+m, "CORRAL_ZZ_TYPO="+m, "CORRAL_="+m, "corral_lower="+m,
		"CORRAL_EMPTYVAR=", "CORRAL_BLANK=  ",
		"CORRAL_A-B="+m, // invalid byte in the name: rendered as a token
		"OTEL_SERVICE_NAME="+m, "OTEL_TRACES_SAMPLER=always_on",
	), "--admin.listen="+addr)
	p.waitReady(addr)
	if code := p.sigterm(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var warns []map[string]any
	for _, r := range p.records() {
		if str(r, "msg") == msgUnknownEnvFn {
			warns = append(warns, r)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("%d unknown-variable warnings, want exactly 1", len(warns))
	}
	w := warns[0]
	if str(w, "level") != "WARN" || str(w, "component") != "config" {
		t.Errorf("warning level/component = %v/%v", w["level"], w["component"])
	}
	// "CORRAL_<invalid-name>" sorts between digits and uppercase letters:
	// after CORRAL_ and before CORRAL_SERVICE_HOST.
	want := "CORRAL_,CORRAL_<invalid-name>,CORRAL_SERVICE_HOST,CORRAL_ZZ_TYPO"
	if got := str(w, "setting"); got != want {
		t.Errorf("setting = %q, want %q", got, want)
	}
	if strings.Contains(str(w, "setting"), "OTEL") {
		t.Error("an OTEL_ variable is listed as unknown")
	}
	requireStderrEmpty(t, p)
	scanClean(t, p, m)
}

// With an unknown variable and an invalid setting, the warning comes first and
// the error second, both JSON, even with --log.format=text.
func TestFnAC5_WarnThenErrorJSONWithTextFormat(t *testing.T) {
	m := fmark(t)
	p := startProc(t, baseEnv("CORRAL_SERVICE_HOST="+m), "--log.format=text", "--log.level="+m)
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	lines := p.stdout.Lines()
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
	recs := p.records() // fails on a non-JSON line
	if str(recs[0], "msg") != msgUnknownEnvFn || str(recs[0], "level") != "WARN" {
		t.Errorf("first record is not the warning")
	}
	if str(recs[1], "setting") != "log.level" || str(recs[1], "level") != "ERROR" {
		t.Errorf("second record is not the log.level error")
	}
	requireStderrEmpty(t, p)
	scanClean(t, p, m)
}

// ---- AC-6: ordering across groups ----

func TestFnAC6_MixedInputExactOrder(t *testing.T) {
	m1, m2, m3, m4, m5, m6 := fmark(t), fmark(t), fmark(t), fmark(t), fmark(t), fmark(t)
	p := startProc(t, baseEnv(
		"CORRAL_LOG_FORMAT="+m3, // invalid, overridden by the valid flag but still reported
		"OTEL_TRACES_SAMPLER="+m4, "OTEL_BSP_SCHEDULE_DELAY="+m5,
	), "--bogus="+m1, "--log.level="+m2, "--admin.listen="+m6, "--tracing.enabled=true", "--log.format=text")
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	recs := p.records() // every line must be JSON
	want := []string{
		"unknown",                                        // group 1
		"CORRAL_LOG_FORMAT", "admin.listen", "log.level", // group 2, byte order
		"OTEL_BSP_SCHEDULE_DELAY", "OTEL_TRACES_SAMPLER", // group 4, sorted
	}
	if got := settingsOfRecs(recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %v, want %v", got, want)
	}
	for _, r := range recs {
		if str(r, "level") != "ERROR" || str(r, "component") != "config" || str(r, "msg") != "invalid configuration" {
			t.Errorf("record is not an FR-11 error: %v", r)
		}
	}
	requireStderrEmpty(t, p)
	scanClean(t, p, m1, m2, m3, m4, m5, m6)
}

// With a test registry holding a required setting that is not supplied, the
// error names it by its logical name; supplied (flag or env), the start succeeds.
func TestFnAC6_RequiredSetting(t *testing.T) {
	p := startTest(t, baseEnv("CORRALTEST_REGISTRY=required"))
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	recs := p.records()
	if got := settingsOfRecs(recs); !reflect.DeepEqual(got, []string{"t.required"}) {
		t.Fatalf("settings = %v, want [t.required]", got)
	}
	requireStderrEmpty(t, p)

	for name, c := range map[string]struct {
		env  []string
		args []string
	}{
		"flag": {nil, []string{"--t.required=v"}},
		"env":  {[]string{"CORRAL_T_REQUIRED=v"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			addr := freeAddr(t)
			p := startTest(t, baseEnv(append([]string{"CORRALTEST_REGISTRY=required"}, c.env...)...), append([]string{"--admin.listen=" + addr}, c.args...)...)
			p.waitReady(addr)
			if code := p.sigterm(); code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}

// With CORRAL_TRACING_ENABLED=maybe and no flag, OTEL_* is not validated: only
// the CORRAL record appears.
func TestFnAC6_UnknownTracingEnabledSkipsOTEL(t *testing.T) {
	p := startProc(t, baseEnv("CORRAL_TRACING_ENABLED=maybe", "OTEL_TRACES_SAMPLER=jaeger_remote"))
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if got := settingsOfRecs(p.records()); !reflect.DeepEqual(got, []string{"CORRAL_TRACING_ENABLED"}) {
		t.Fatalf("settings = %v, want [CORRAL_TRACING_ENABLED]", got)
	}
}

// ---- AC-7 ----

func TestFnAC7_InvalidSettingWithBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	p := startProc(t, baseEnv(), "--admin.listen="+busy.Addr().String(), "--log.level=bad")
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2 (not 1)", code)
	}
	if msgs := lifecycleMsgs(p.records()); len(msgs) != 0 {
		t.Errorf("lifecycle records on the failure path: %v", msgs)
	}
	requireStderrEmpty(t, p)
}
