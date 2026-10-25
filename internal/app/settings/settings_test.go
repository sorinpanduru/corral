package settings_test

import (
	"bytes"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/config/usage"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// ---- AC-1: the real registry ----

func TestAC1_RegistryBuildsAndGroupsAreWellFormed(t *testing.T) {
	mustRegistry(t)
	gs := settings.Groups()
	if len(gs) == 0 {
		t.Fatal("no groups")
	}
	g := spec001Group(t)
	if !reflect.DeepEqual(g.Namespaces, []string{"log", "admin", "tracing"}) && !sameSet(g.Namespaces, []string{"log", "admin", "tracing"}) {
		t.Errorf("SPEC-001 namespaces = %v, want log, admin, tracing", g.Namespaces)
	}
	if len(g.Decls) != 5 {
		t.Errorf("SPEC-001 group declares %d settings, want 5", len(g.Decls))
	}
	if len(g.Rules) != 0 {
		t.Errorf("SPEC-001 group has %d cross rules, want none", len(g.Rules))
	}
	// One group per spec: no two groups share a Spec name.
	seen := map[string]bool{}
	for _, g := range gs {
		if g.Spec == "" || seen[g.Spec] {
			t.Errorf("group Spec %q is empty or repeated", g.Spec)
		}
		seen[g.Spec] = true
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// AC-1: every registered setting has an FR-12 type, a valid presence, a
// non-empty one-line description and derived names that follow FR-3 and are
// unique.
func TestAC1_EverySettingIsWellFormed(t *testing.T) {
	r := mustRegistry(t)
	flags, envs := map[string]string{}, map[string]string{}
	for _, s := range r.Settings() {
		if err := s.Type.Check(); err != nil {
			t.Errorf("%s: type spec invalid: %v", s.Name, err)
		}
		switch s.Presence {
		case registry.PresenceDefault, registry.PresenceRequired, registry.PresenceOptional:
		default:
			t.Errorf("%s: invalid presence", s.Name)
		}
		if strings.TrimSpace(s.Description) == "" || strings.ContainsAny(s.Description, "\r\n") {
			t.Errorf("%s: description empty or multi-line", s.Name)
		}
		for _, c := range s.Description {
			if unicode.IsControl(c) {
				t.Errorf("%s: description holds a control character", s.Name)
				break
			}
		}
		if want := "CORRAL_" + strings.ToUpper(strings.ReplaceAll(s.Name, ".", "_")); s.Env != want {
			t.Errorf("%s: env name %q, want %q (FR-3)", s.Name, s.Env, want)
		}
		if s.Flag != s.Name {
			t.Errorf("%s: flag name %q, want the logical name", s.Name, s.Flag)
		}
		if s.Secret && s.Presence == registry.PresenceDefault {
			t.Errorf("%s: secret with a default", s.Name)
		}
		names := []string{s.Flag}
		envNames := []string{s.Env}
		if s.Secret {
			names = append(names, s.Name+"_file")
			envNames = append(envNames, s.Env+"_FILE")
		}
		for _, n := range names {
			if o, dup := flags[n]; dup {
				t.Errorf("flag name %s used by %s and %s", n, o, s.Name)
			}
			flags[n] = s.Name
		}
		for _, n := range envNames {
			if o, dup := envs[n]; dup {
				t.Errorf("environment name %s used by %s and %s", n, o, s.Name)
			}
			envs[n] = s.Name
		}
	}
}

// AC-1 / SPEC-003 Interfaces: the SPEC-001 settings with their types,
// presences, defaults and descriptions.
func TestAC1_Spec001SettingsAsDeclared(t *testing.T) {
	r := mustRegistry(t)
	want := map[string]struct {
		spec types.Spec
		def  string
		desc string
	}{
		"log.format":           {types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}, "json", "Log output format."},
		"log.level":            {types.Spec{Kind: types.KindEnum, EnumValues: []string{"debug", "info", "warn", "error"}}, "info", "Minimum log level."},
		"admin.listen":         {types.Spec{Kind: types.KindHostPort}, "127.0.0.1:9187", "Admin HTTP listener address."},
		"tracing.enabled":      {types.Spec{Kind: types.KindBool}, "false", "Enable OTLP tracing."},
		"tracing.sample_ratio": {types.Spec{Kind: types.KindFloat}, "1.0", "Trace sampling ratio."},
	}
	for _, n := range spec001Names {
		w := want[n]
		s, ok := r.Lookup(n)
		if !ok {
			t.Fatalf("%s is not registered", n)
		}
		if s.Type.Kind != w.spec.Kind || !reflect.DeepEqual(s.Type.EnumValues, w.spec.EnumValues) {
			t.Errorf("%s: type %s, want %s", n, s.Type.Describe(), w.spec.Describe())
		}
		if s.Type.EnumCaseSensitive {
			t.Errorf("%s: enum is case-sensitive, want case-insensitive", n)
		}
		if s.Presence != registry.PresenceDefault || s.Default != w.def {
			t.Errorf("%s: presence %v default %q, want default %q", n, s.Presence, s.Default, w.def)
		}
		if s.Secret {
			t.Errorf("%s: declared secret", n)
		}
		if s.Description != w.desc {
			t.Errorf("%s: description %q, want %q", n, s.Description, w.desc)
		}
	}
	// Derived names from the SPEC-003 name-mapping table.
	if s, _ := r.Lookup("log.level"); s.Env != "CORRAL_LOG_LEVEL" {
		t.Errorf("log.level env = %s", s.Env)
	}
	if s, _ := r.Lookup("tracing.sample_ratio"); s.Env != "CORRAL_TRACING_SAMPLE_RATIO" {
		t.Errorf("tracing.sample_ratio env = %s", s.Env)
	}
	// The group declares exactly those five.
	var names []string
	for _, d := range spec001Group(t).Decls {
		names = append(names, d.Name)
	}
	if !sameSet(names, spec001Names) {
		t.Errorf("SPEC-001 group declares %v, want %v", names, spec001Names)
	}
}

// FR-1 / SPEC-001 FR-21: the sample ratio rule is 0.0 to 1.0 inclusive.
func TestSampleRatioRule(t *testing.T) {
	d := declOf(t, "tracing.sample_ratio")
	if d.Rule == nil {
		t.Fatal("tracing.sample_ratio has no range rule")
	}
	for _, ok := range []float64{0, 0.5, 1, 1e-9, 0.9999999} {
		if err := d.Rule(ok); err != nil {
			t.Errorf("rule rejects %v", ok)
		}
	}
	for _, bad := range []float64{-0.1, -1e-12, 1.0000001, 2, 1e300} {
		if err := d.Rule(bad); err == nil {
			t.Errorf("rule accepts %v", bad)
		}
	}
}

func declOf(t *testing.T, name string) registry.Decl {
	t.Helper()
	for _, d := range spec001Group(t).Decls {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("%s not declared in the SPEC-001 group", name)
	return registry.Decl{}
}

// ---- typed view (FR-16) ----

func TestDefaultObservability(t *testing.T) {
	want := settings.Observability{
		LogFormat: logging.FormatJSON, LogLevel: slog.LevelInfo, AdminListen: "127.0.0.1:9187",
		TracingEnabled: false, SampleRatio: 1.0,
	}
	if got := settings.DefaultObservability(); got != want {
		t.Fatalf("DefaultObservability() = %+v, want %+v", got, want)
	}
	res := load(t, nil, nil)
	requireOnly(t, res, spec001Names)
	if got := settings.ObservabilityFrom(res); got != want {
		t.Fatalf("ObservabilityFrom(no inputs) = %+v, want the defaults", got)
	}
}

func TestObservabilityFrom_Mapping(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  []string
		want settings.Observability
	}{
		{"all by flag", []string{"--log.format=text", "--log.level=warn", "--admin.listen=0.0.0.0:0", "--tracing.enabled=true", "--tracing.sample_ratio=0.25"}, nil,
			settings.Observability{LogFormat: logging.FormatText, LogLevel: slog.LevelWarn, AdminListen: "0.0.0.0:0", TracingEnabled: true, SampleRatio: 0.25}},
		{"all by env", nil, []string{"CORRAL_LOG_FORMAT=TEXT", "CORRAL_LOG_LEVEL=Error", "CORRAL_ADMIN_LISTEN=[::1]:8080", "CORRAL_TRACING_ENABLED=TRUE", "CORRAL_TRACING_SAMPLE_RATIO=0"},
			settings.Observability{LogFormat: logging.FormatText, LogLevel: slog.LevelError, AdminListen: "[::1]:8080", TracingEnabled: true, SampleRatio: 0}},
		{"debug", []string{"--log.level=DEBUG"}, nil, with(func(o *settings.Observability) { o.LogLevel = slog.LevelDebug })},
		{"info", []string{"--log.level=Info"}, nil, with(func(o *settings.Observability) { o.LogLevel = slog.LevelInfo })},
		{"flag over env", []string{"--log.level=debug"}, []string{"CORRAL_LOG_LEVEL=error"}, with(func(o *settings.Observability) { o.LogLevel = slog.LevelDebug })},
		{"ratio 1", []string{"--tracing.sample_ratio=1"}, nil, settings.DefaultObservability()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, c.args, c.env)
			requireOnly(t, res, spec001Names)
			if got := settings.ObservabilityFrom(res); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func with(f func(*settings.Observability)) settings.Observability {
	o := settings.DefaultObservability()
	f(&o)
	return o
}

// ---- AC-2: table over every registered non-secret setting ----

type sample struct {
	valid1, valid2 string // two different valid values, neither equal to the default
	invalid        string
}

// samples forces SPEC-002 (and later) to add a row for each new non-secret
// setting: the test fails for a registered setting with no sample.
var samples = map[string]sample{
	"log.format":           {"text", "JSON", "xml"},
	"log.level":            {"debug", "WARN", "debug-4"},
	"admin.listen":         {"0.0.0.0:9000", "[::1]:8080", ":9187"},
	"tracing.enabled":      {"true", "TRUE", "maybe"},
	"tracing.sample_ratio": {"0.25", "0.5", "1.5"},

	// SPEC-002 (T17).
	"proxy.listen":                        {"0.0.0.0:27018", "[::1]:27019", "127.0.0.1:0"},
	"proxy.max_client_connections":        {"5", "6", "0"},
	"proxy.client_idle_timeout":           {"1s", "2m", "-1s"},
	"proxy.client_io_timeout":             {"10s", "1m", "0s"},
	"proxy.shutdown.listener_close_delay": {"1s", "3s", "-1s"},
	"proxy.shutdown.drain_timeout":        {"5s", "1m", "0s"},
	"pool.max_size":                       {"20", "30", "0"},
	"pool.min_size":                       {"2", "3", "-1"},
	"pool.max_waiters":                    {"5", "7", "0"},
	"pool.checkout_timeout":               {"1s", "2s", "0s"},
	"upstream.hosts":                      {"a.test:27017", "b.test:27018", "a.test:0"},
	"upstream.auth.source":                {"authdb", "admin2", "a$b"},
	"upstream.connect_timeout":            {"3s", "4s", "0s"},
	"upstream.heartbeat_interval":         {"1s", "2s", "100ms"},
	"upstream.max_idle_time":              {"1m", "0s", "-1s"},
	"upstream.tls.enabled":                {"true", "TRUE", "maybe"},
}

// contextualSamples are SPEC-002 settings whose valid values depend on other
// settings or on files (cross rules, the PEM rule, the default mechanism that
// needs credentials). The generic loop below cannot give them a valid
// configuration in isolation; proxy_settings_test.go covers each of them with
// its companions (AC-31), so they are excluded here by name, not silently.
var contextualSamples = map[string]string{
	"proxy.tls.enabled":        "needs cert_path and key (proxy-tls-material)",
	"proxy.tls.cert_path":      "PEM rule: needs a file",
	"proxy.tls.client_ca_path": "PEM rule and proxy-tls-mtls",
	"upstream.tls.ca_path":     "PEM rule and upstream-tls-ca",
	"upstream.auth.mechanism":  "default needs credentials (upstream-auth-credentials)",
	"upstream.replica_set":     "any non-empty string is valid, so the generic invalid-value step has nothing to try; its rule is covered in proxy_settings_test.go",
}

func TestAC2_EveryNonSecretSetting(t *testing.T) {
	r := mustRegistry(t)
	for _, s := range r.Settings() {
		if s.Secret {
			continue
		}
		if _, ok := contextualSamples[s.Name]; ok {
			continue
		}
		sm, ok := samples[s.Name]
		if !ok {
			t.Errorf("no AC-2 sample for registered setting %s: add one to the samples table", s.Name)
			continue
		}
		t.Run(s.Name, func(t *testing.T) {
			parse := func(raw string) any {
				v, err := s.Type.Parse(raw)
				if err != nil {
					t.Fatalf("sample for %s is not valid for its type", s.Name)
				}
				return v
			}
			v1, v2 := parse(sm.valid1), parse(sm.valid2)
			own := []string{s.Name, s.Env}
			flag := func(v string) []string { return []string{"--" + s.Flag + "=" + v} }
			env := func(v string) []string { return []string{s.Env + "=" + v} }

			// no input: the default applies
			if s.Presence == registry.PresenceDefault {
				res := load(t, nil, nil)
				requireOnly(t, res, own)
				if v := res.Values[s.Name]; !v.Set || v.Source != loader.SourceDefault || !reflect.DeepEqual(v.V, s.DefaultValue) {
					t.Errorf("default not applied (set=%v source=%v)", v.Set, v.Source)
				}
			}
			// flag alone
			res := load(t, flag(sm.valid1), nil)
			requireOnly(t, res, own)
			if v := res.Values[s.Name]; !v.Set || v.Source != loader.SourceFlag || !reflect.DeepEqual(v.V, v1) {
				t.Errorf("flag value not used")
			}
			// env alone
			res = load(t, nil, env(sm.valid2))
			requireOnly(t, res, own)
			if v := res.Values[s.Name]; !v.Set || v.Source != loader.SourceEnv || !reflect.DeepEqual(v.V, v2) {
				t.Errorf("env value not used")
			}
			// both, different valid values: the flag wins
			res = load(t, flag(sm.valid1), env(sm.valid2))
			requireOnly(t, res, own)
			if v := res.Values[s.Name]; v.Source != loader.SourceFlag || !reflect.DeepEqual(v.V, v1) {
				t.Errorf("flag does not win")
			}
			// invalid env with a valid flag: rejected under CORRAL_<NAME>
			res = load(t, flag(sm.valid1), env(sm.invalid))
			requireOnly(t, res, own, s.Env)
			if res.Failed(s.Name) {
				t.Errorf("setting failed although the valid flag wins")
			}
			// invalid flag: rejected under the logical name
			res = load(t, flag(sm.invalid), nil)
			requireOnly(t, res, own, s.Name)
		})
	}
}

// ---- AC-17 and the SPEC-001 AC-21 cases, via flag and via environment ----

var ac21Valid = []struct {
	setting, value string
	check          func(settings.Observability) bool
}{
	{"log.level", "debug", func(o settings.Observability) bool { return o.LogLevel == slog.LevelDebug }},
	{"log.level", "DEBUG", func(o settings.Observability) bool { return o.LogLevel == slog.LevelDebug }},
	{"log.level", "Info", func(o settings.Observability) bool { return o.LogLevel == slog.LevelInfo }},
	{"log.level", "warn", func(o settings.Observability) bool { return o.LogLevel == slog.LevelWarn }},
	{"log.level", "ERROR", func(o settings.Observability) bool { return o.LogLevel == slog.LevelError }},
	{"log.format", "json", func(o settings.Observability) bool { return o.LogFormat == logging.FormatJSON }},
	{"log.format", "TEXT", func(o settings.Observability) bool { return o.LogFormat == logging.FormatText }},
	{"admin.listen", "127.0.0.1:0", func(o settings.Observability) bool { return o.AdminListen == "127.0.0.1:0" }},
	{"admin.listen", "0.0.0.0:65535", func(o settings.Observability) bool { return o.AdminListen == "0.0.0.0:65535" }},
	{"admin.listen", "[::]:9187", func(o settings.Observability) bool { return o.AdminListen == "[::]:9187" }},
	{"admin.listen", "localhost:8080", func(o settings.Observability) bool { return o.AdminListen == "localhost:8080" }},
	{"admin.listen", "db-proxy.example.internal:9187", func(o settings.Observability) bool { return o.AdminListen == "db-proxy.example.internal:9187" }},
	{"tracing.enabled", "true", func(o settings.Observability) bool { return o.TracingEnabled }},
	{"tracing.enabled", "False", func(o settings.Observability) bool { return !o.TracingEnabled }},
	{"tracing.sample_ratio", "0", func(o settings.Observability) bool { return o.SampleRatio == 0 }},
	{"tracing.sample_ratio", "1", func(o settings.Observability) bool { return o.SampleRatio == 1 }},
	{"tracing.sample_ratio", ".5", func(o settings.Observability) bool { return o.SampleRatio == 0.5 }},
	{"tracing.sample_ratio", "1e-1", func(o settings.Observability) bool { return o.SampleRatio == 0.1 }},
}

var ac21Invalid = []struct{ setting, value string }{
	{"log.level", "debug-4"}, {"log.level", "info+2"}, {"log.level", "trace"}, {"log.level", "debug "}, {"log.level", "\u200b"},
	{"log.format", "xml"}, {"log.format", "json "}, {"log.format", "jsonx"},
	{"admin.listen", ":9187"}, {"admin.listen", ":0"}, {"admin.listen", "[]:9187"}, {"admin.listen", "127.0.0.1"}, {"admin.listen", "127.0.0.1:"},
	{"admin.listen", "127.0.0.1:65536"}, {"admin.listen", "127.0.0.1:-1"}, {"admin.listen", "127.0.0.1:abc"}, {"admin.listen", " :9187"},
	{"admin.listen", "0:9187"}, {"admin.listen", "0x0:9187"}, {"admin.listen", "127.1:9187"}, {"admin.listen", "a_b:9187"},
	{"admin.listen", "a..b:9187"}, {"admin.listen", "-a.example:9187"}, {"admin.listen", "[::]"},
	// FR-12 tightenings relative to the interim loader
	{"admin.listen", "127.0.0.1:09187"}, {"admin.listen", "127.0.0.1:+80"}, {"admin.listen", "localhost.:9187"}, {"admin.listen", "[fe80::1%eth0]:9187"},
	{"tracing.enabled", "maybe"}, {"tracing.enabled", "1"}, {"tracing.enabled", "yes"}, {"tracing.enabled", "true "},
	{"tracing.sample_ratio", "-0.1"}, {"tracing.sample_ratio", "1.0000001"}, {"tracing.sample_ratio", "2"}, {"tracing.sample_ratio", "1e300"},
	{"tracing.sample_ratio", "1e999"}, {"tracing.sample_ratio", "NaN"}, {"tracing.sample_ratio", "+Inf"}, {"tracing.sample_ratio", "-Inf"},
	{"tracing.sample_ratio", "abc"}, {"tracing.sample_ratio", "+0.5"}, {"tracing.sample_ratio", "0x1p-2"}, {"tracing.sample_ratio", "1_0"},
}

func TestAC17_ValidValuesViaFlagAndEnv(t *testing.T) {
	r := mustRegistry(t)
	for _, c := range ac21Valid {
		s, _ := r.Lookup(c.setting)
		for _, via := range []string{"flag", "env"} {
			var res loader.Result
			if via == "flag" {
				res = load(t, []string{"--" + c.setting + "=" + c.value}, nil)
			} else {
				res = load(t, nil, []string{s.Env + "=" + c.value})
			}
			requireOnly(t, res, spec001Names)
			if !c.check(settings.ObservabilityFrom(res)) {
				t.Errorf("%s via %s: value of length %d not mapped to the typed view", c.setting, via, len(c.value))
			}
		}
	}
}

func TestAC17_InvalidValuesViaFlagAndEnv(t *testing.T) {
	r := mustRegistry(t)
	for _, c := range ac21Invalid {
		s, _ := r.Lookup(c.setting)
		res := load(t, []string{"--" + c.setting + "=" + c.value}, nil)
		requireOnly(t, res, spec001Names, c.setting)
		if !res.Failed(c.setting) {
			t.Errorf("%s via flag: not failed", c.setting)
		}
		res = load(t, nil, []string{s.Env + "=" + c.value})
		requireOnly(t, res, spec001Names, s.Env)
		if !res.Failed(c.setting) {
			t.Errorf("%s via env: not failed", c.setting)
		}
	}
}

// Rejections name the setting only: no rendering carries the value.
func TestAC17_NoValueEchoed(t *testing.T) {
	r := mustRegistry(t)
	for _, name := range spec001Names {
		s, _ := r.Lookup(name)
		m := "MARKER" + strings.Repeat("ab12", 6)
		for _, res := range []loader.Result{
			load(t, []string{"--" + name + "=" + m}, nil),
			load(t, nil, []string{s.Env + "=" + m}),
		} {
			if len(res.Errors) == 0 {
				t.Fatalf("%s: marker value accepted", name)
			}
			var buf bytes.Buffer
			config.LogErrors(logging.BootstrapLogger(&buf), res.Errors)
			for _, e := range res.Errors {
				buf.WriteString(e.Error())
			}
			if strings.Contains(buf.String(), m) {
				t.Errorf("%s: rendering carries the supplied value", name)
			}
		}
	}
}

// ---- AC-4 ----

func TestAC4_EmptyFlagValueRejected(t *testing.T) {
	res := load(t, []string{"--log.level="}, nil)
	requireOnly(t, res, spec001Names, "log.level")
	for _, name := range spec001Names {
		res := load(t, []string{"--" + name + "="}, nil)
		requireOnly(t, res, spec001Names, name)
	}
}

func TestAC4_EmptyEnvIsUnsetForEverySetting(t *testing.T) {
	r := mustRegistry(t)
	var env []string
	for _, name := range spec001Names {
		s, _ := r.Lookup(name)
		env = append(env, s.Env+"=")
	}
	res := load(t, nil, env)
	requireOnly(t, res, spec001Names)
	if got := settings.ObservabilityFrom(res); got != settings.DefaultObservability() {
		t.Fatalf("empty environment values changed the settings: %+v", got)
	}
}

// ---- AC-22 (CORRAL_LOG_LEVEL part, real registry) ----

func TestAC22_WhitespaceOnlyLogLevelIsUnset(t *testing.T) {
	for _, v := range []string{"\t", "\v", "\f", "\u0085", "\u00a0", "\u2003", "\u3000"} {
		res := load(t, nil, []string{"CORRAL_LOG_LEVEL=" + v})
		requireOnly(t, res, spec001Names)
		if got := settings.ObservabilityFrom(res); got.LogLevel != slog.LevelInfo {
			t.Errorf("whitespace-only value (length %d): log level %v, want the default info", len(v), got.LogLevel)
		}
		if v := res.Values["log.level"]; v.Source != loader.SourceDefault {
			t.Errorf("source = %v, want default", v.Source)
		}
		for _, s := range res.Supplied {
			if s == "log.level" {
				t.Errorf("log.level listed as supplied for a whitespace-only value")
			}
		}
	}
}

func TestAC22_ZeroWidthSpaceAndInvalidUTF8AreSetAndRejected(t *testing.T) {
	for name, v := range map[string]string{"U+200B": "\u200b", "invalid UTF-8": "\xff\xfe", "single bad byte": "\xff"} {
		t.Run(name, func(t *testing.T) {
			res := load(t, nil, []string{"CORRAL_LOG_LEVEL=" + v})
			requireOnly(t, res, spec001Names, "CORRAL_LOG_LEVEL")
		})
	}
}

// ---- golden help output (FR-21) ----

// The SPEC-001 group alone renders exactly the committed golden text, which
// is also what the SPEC-003 Interfaces table renders to (checked
// independently of the implementation).
func TestHelp_Spec001Golden(t *testing.T) {
	golden, err := os.ReadFile("testdata/help_spec001.golden")
	if err != nil {
		t.Fatal(err)
	}
	render := func(g registry.Group) string {
		r, err := registry.New(g)
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		var b bytes.Buffer
		if err := usage.Write(&b, r); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := render(specTable()); got != string(golden) {
		t.Fatal("golden file does not match the usage rendering of the SPEC-003 table (regenerate the golden)")
	}
	if got := render(spec001Group(t)); got != string(golden) {
		t.Fatalf("help for the SPEC-001 group differs from the golden text:\n%s", firstDiff(got, string(golden)))
	}
}

// The full registry's help lists every SPEC-001 setting, sorted, and never a
// value from the process environment.
func TestHelp_FullRegistry(t *testing.T) {
	r := mustRegistry(t)
	t.Setenv("CORRAL_LOG_LEVEL", "MARKERdebug")
	var b bytes.Buffer
	if err := usage.Write(&b, r); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "MARKERdebug") {
		t.Fatal("help carries an environment value")
	}
	last := -1
	for _, n := range spec001Names {
		i := strings.Index(out, "\n"+n+"\n")
		if i < 0 {
			t.Fatalf("help lacks %s", n)
		}
		if i < last {
			t.Fatalf("help is not sorted at %s", n)
		}
		last = i
	}
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return "line " + string(rune('0'+i%10)) + ": got " + la[i] + " | want " + lb[i]
		}
	}
	return "length differs"
}
