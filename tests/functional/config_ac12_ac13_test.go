//go:build functional

package functional

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// ---- AC-12: help, version and the FR-27 combinations ----

func TestFnAC12_HelpListsEverySetting(t *testing.T) {
	reg, err := settings.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--help", "-help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			addr := freeAddr(t)
			m := fmark(t)
			p := startProc(t, baseEnv("CORRAL_LOG_LEVEL="+m), flag, "--admin.listen="+addr)
			if code := p.waitExit(); code != 0 {
				t.Fatalf("exit %d, want 0", code)
			}
			out := string(p.stdout.Bytes())
			for _, s := range reg.Settings() {
				for what, want := range map[string]string{
					"name": s.Name, "flag": "--" + s.Flag + "=", "environment variable": s.Env,
					"type": s.Type.Describe(), "description": s.Description,
				} {
					if !strings.Contains(out, want) {
						t.Errorf("help lacks the %s of %s", what, s.Name)
					}
				}
				if s.Presence == registry.PresenceDefault && s.Default != "" && !strings.Contains(out, s.Default) {
					t.Errorf("help lacks the default of %s", s.Name)
				}
			}
			if strings.Contains(out, m) {
				t.Error("help carries an environment value")
			}
			requirePortFree(t, addr)
			requireStderrEmpty(t, p)
			// Plain text: not log records.
			if strings.HasPrefix(strings.TrimSpace(out), "{") {
				t.Error("help output looks like a log record")
			}
		})
	}
}

// The secret text appears for secret settings (corraltest registry).
func TestFnAC12_HelpSecretText(t *testing.T) {
	p := startTest(t, baseEnv("CORRALTEST_REGISTRY=secret"), "--help")
	if code := p.waitExit(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "(secret: set CORRAL_T_SECRET or CORRAL_T_SECRET_FILE, or pass --t.secret_file; never pass the value on the command line)"
	if !strings.Contains(string(p.stdout.Bytes()), want) {
		t.Error("help lacks the exact secret text")
	}
}

var versionLine = regexp.MustCompile(`^corral \S+\n$`)

func TestFnAC12_VersionLine(t *testing.T) {
	for _, bin := range []string{binPath, testBinPath} {
		addr := freeAddr(t)
		p := startProcBin(t, bin, baseEnv(), "--version", "--admin.listen="+addr)
		if code := p.waitExit(); code != 0 {
			t.Fatalf("exit %d", code)
		}
		out := string(p.stdout.Bytes())
		if !versionLine.MatchString(out) {
			t.Errorf("output %q is not one `corral <version>` line", out)
		}
		if out != "corral devel\n" {
			t.Errorf("unstamped build prints %q, want %q", out, "corral devel\n")
		}
		requirePortFree(t, addr)
		requireStderrEmpty(t, p)
	}
}

// FR-27 combinations table.
func TestFnAC12_CombinationsTable(t *testing.T) {
	type row struct {
		name    string
		args    []string
		exit    int
		help    bool     // stdout is the usage text
		version bool     // stdout is the version line
		records []string // exact settings of the JSON records (nil: none required)
		msgs    []string // exact messages of the JSON records for non-error rows
	}
	rows := []row{
		{name: "--help --version", args: []string{"--help", "--version"}, help: true},
		{name: "--version --check-config", args: []string{"--version", "--check-config"}, version: true},
		{name: "--check-config --help", args: []string{"--check-config", "--help"}, help: true},
		{name: "--help --bogus=1", args: []string{"--help", "--bogus=1"}, help: true},
		{name: "--version --log.level=bad", args: []string{"--version", "--log.level=bad"}, version: true},
		{name: "--check-config --check-config", args: []string{"--check-config", "--check-config"}, msgs: []string{msgLoadedFn}},
		{name: "--check-config=true", args: []string{"--check-config=true"}, exit: 2, records: []string{"unknown"}},
		{name: "--check-config --help=x", args: []string{"--check-config", "--help=x"}, exit: 2, records: []string{"unknown"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			addr := freeAddr(t)
			p := startProc(t, baseEnv(), append(append([]string{}, r.args...), "--admin.listen="+addr)...)
			if code := p.waitExit(); code != r.exit {
				t.Fatalf("exit %d, want %d", code, r.exit)
			}
			out := string(p.stdout.Bytes())
			switch {
			case r.help:
				if !strings.Contains(out, "--help") || strings.HasPrefix(strings.TrimSpace(out), "{") {
					t.Error("stdout is not the usage text")
				}
			case r.version:
				if out != "corral devel\n" {
					t.Errorf("stdout %q is not the version line", out)
				}
			default:
				recs := p.records()
				if r.records != nil {
					if got := settingsOfRecs(recs); !reflect.DeepEqual(got, r.records) {
						t.Errorf("settings = %v, want %v", got, r.records)
					}
				}
				if r.msgs != nil {
					if got := msgsOfRecs(recs); !reflect.DeepEqual(got, r.msgs) {
						t.Errorf("messages = %q, want %q", got, r.msgs)
					}
				}
			}
			if r.help || r.version {
				requirePortFree(t, addr)
				if lines := p.stdout.Lines(); len(lines) > 0 && strings.HasPrefix(lines[0], "{") {
					t.Error("a log record in help or version mode")
				}
			}
			requireStderrEmpty(t, p)
		})
	}
}

// ---- AC-13: check-config ----

func TestFnAC13_ValidCheckConfig(t *testing.T) {
	col := otlpcollector.Start(t)
	addr := freeAddr(t)
	p := startProc(t, baseEnv("OTEL_EXPORTER_OTLP_ENDPOINT=http://"+col.HTTPEndpoint()),
		"--check-config", "--log.level=info", "--tracing.enabled=true", "--admin.listen="+addr)
	if code := p.waitExit(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	recs := p.records()
	if got := msgsOfRecs(recs); !reflect.DeepEqual(got, []string{msgLoadedFn}) {
		t.Fatalf("records = %q, want the FR-20 record only", got)
	}
	if msgs := lifecycleMsgs(recs); len(msgs) != 0 {
		t.Errorf("lifecycle records in check-config mode: %v", msgs)
	}
	requirePortFree(t, addr)
	if n := col.ConnCount(); n != 0 {
		t.Errorf("the collector accepted %d connections in check-config mode", n)
	}
	requireStderrEmpty(t, p)
}

func TestFnAC13_TextFormatIsSlogText(t *testing.T) {
	p := startProc(t, baseEnv(), "--check-config", "--log.format=text")
	if code := p.waitExit(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := p.stdout.Lines()
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	l := lines[0]
	for _, frag := range []string{"level=INFO", "component=config", `msg="configuration loaded"`} {
		if !strings.Contains(l, frag) {
			t.Errorf("text record lacks %q", frag)
		}
	}
	if strings.HasPrefix(l, "{") {
		t.Error("the success record is JSON with --log.format=text")
	}
}

func TestFnAC13_InvalidSamplerIsJSONExit2(t *testing.T) {
	p := startProc(t, baseEnv("OTEL_TRACES_SAMPLER=jaeger_remote"), "--check-config", "--tracing.enabled=true", "--log.format=text")
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	recs := p.records() // JSON or the test fails
	if got := settingsOfRecs(recs); !reflect.DeepEqual(got, []string{"OTEL_TRACES_SAMPLER"}) {
		t.Fatalf("settings = %v", got)
	}
}

// An http:// endpoint with a valid CA certificate: the FR-41 record comes
// before the FR-20 record.
func TestFnAC13_TLSIgnoredBeforeLoaded(t *testing.T) {
	ca, _ := genSyntheticCA(t)
	p := startProc(t, baseEnv("OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.invalid:4318", "OTEL_EXPORTER_OTLP_CERTIFICATE="+ca),
		"--check-config", "--tracing.enabled=true")
	if code := p.waitExit(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := msgsOfRecs(p.records()); !reflect.DeepEqual(got, []string{msgTLSFn, msgLoadedFn}) {
		t.Fatalf("records = %q, want the FR-41 record then the FR-20 record", got)
	}
	requireStderrEmpty(t, p)
}
