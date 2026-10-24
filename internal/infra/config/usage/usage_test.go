package usage_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/config/usage"
)

// SPEC-003 FR-21 (T8), AC-12 unit part (content), AC-9 (help never contains
// values). A test registry is used; the golden output for the real SPEC-001
// registry is asserted with T10.
//
// Interface decisions (see the test-writer report): the output is paragraphs
// separated by one blank line: a header, one paragraph per setting sorted by
// logical name, a control-flags paragraph and a final OTEL_* line. A setting's
// paragraph is the one holding its environment variable name as a whole word.
// The fixed OTEL_* line is checked by its content (starts with "OTEL_*",
// mentions "honoured" and "operator documentation"), not by exact wording.

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

func d(name string, spec types.Spec, p registry.Presence, def, desc string, secret bool) registry.Decl {
	return registry.Decl{Name: name, Type: spec, Presence: p, Default: def, Description: desc, Secret: secret}
}

func str() types.Spec { return types.Spec{Kind: types.KindString} }

// Names are deliberately declared out of order and across groups.
func testRegistry(tb testing.TB) *registry.Registry {
	tb.Helper()
	enum := types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}
	r, err := registry.New(
		registry.Group{Spec: "S1", Namespaces: []string{"t", "log"}, Decls: []registry.Decl{
			d("t.timeout", types.Spec{Kind: types.KindDuration}, registry.PresenceDefault, "5s", "Request timeout.", false),
			d("t.secret", str(), registry.PresenceOptional, "", "A synthetic credential.", true),
			d("log.format", enum, registry.PresenceDefault, "JSON", "Log output format.", false),
			d("t.ratio", types.Spec{Kind: types.KindFloat}, registry.PresenceDefault, "1.0", "A sampling ratio.", false),
			d("t.hosts", types.Spec{Kind: types.KindList, Elem: types.KindString}, registry.PresenceDefault, "a,b", "A host list.", false),
		}},
		registry.Group{Spec: "S2", Namespaces: []string{"a", "z"}, Decls: []registry.Decl{
			d("z.last", types.Spec{Kind: types.KindInt}, registry.PresenceDefault, "5", "The last one.", false),
			d("a.first", types.Spec{Kind: types.KindBool}, registry.PresenceDefault, "false", "The first one.", false),
			d("a.needed", str(), registry.PresenceRequired, "", "A required value.", false),
			d("a.maybe", types.Spec{Kind: types.KindHostPort}, registry.PresenceOptional, "", "An optional listener.", false),
			d("a.token", str(), registry.PresenceRequired, "", "A required synthetic credential.", true),
		}},
	)
	if err != nil {
		tb.Fatalf("registry: %v", err)
	}
	return r
}

func render(t *testing.T, r *registry.Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := usage.Write(&buf, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

func paragraphs(out string) []string {
	var ps []string
	for _, p := range strings.Split(strings.TrimRight(out, "\n"), "\n\n") {
		if strings.TrimSpace(p) != "" {
			ps = append(ps, p)
		}
	}
	return ps
}

// block returns the paragraph that holds the setting's environment variable.
func block(t *testing.T, out string, s registry.Setting) string {
	t.Helper()
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(s.Env) + `([^A-Za-z0-9_]|$)`)
	var found []string
	for _, p := range paragraphs(out) {
		if re.MatchString(p) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d paragraphs hold the environment variable, want exactly 1", s.Name, len(found))
	}
	return found[0]
}

const secretFmt = "(secret: set CORRAL_%s or CORRAL_%s_FILE, or pass --%s_file; never pass the value on the command line)"

func secretText(name string) string {
	env := registry.EnvName(name)
	return "(secret: set " + env + " or " + env + "_FILE, or pass --" + name + "_file; never pass the value on the command line)"
}

// FR-21: every field is present for every setting.
func TestWrite_EverySettingHasEveryField(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for _, s := range r.Settings() {
		b := block(t, out, s)
		for what, want := range map[string]string{
			"logical name": s.Name,
			"flag form":    "--" + s.Name + "=",
			"environment":  s.Env,
			"description":  s.Description,
			"type":         s.Type.Describe(),
		} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: block lacks the %s", s.Name, what)
			}
		}
	}
}

func TestWrite_TypesAndEnumValues(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for name, want := range map[string]string{
		"log.format": "enum (json, text)", "t.hosts": "list of string", "t.timeout": "duration",
		"t.ratio": "float", "z.last": "int", "a.first": "bool", "a.maybe": "hostport",
	} {
		s, _ := r.Lookup(name)
		if !strings.Contains(block(t, out, s), want) {
			t.Errorf("%s: block lacks the type text %q", name, want)
		}
	}
}

// FR-21: the default is printed verbatim as declared.
func TestWrite_DefaultsVerbatim(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for name, want := range map[string]string{
		"t.timeout": "5s", "t.ratio": "1.0", "t.hosts": "a,b", "z.last": "5", "a.first": "false",
		"log.format": "JSON", // the declared spelling, not the canonical "json"
	} {
		s, _ := r.Lookup(name)
		b := block(t, out, s)
		if !strings.Contains(b, want) {
			t.Errorf("%s: block does not show the declared default %q", name, want)
		}
		if strings.Contains(b, "(required)") || strings.Contains(b, "(optional, unset by default)") {
			t.Errorf("%s: a setting with a default is marked required or optional", name)
		}
	}
	// "a,b" is not normalised to "a, b", "1.0" not to "1".
	s, _ := r.Lookup("t.hosts")
	if strings.Contains(block(t, out, s), "a, b") {
		t.Error("list default was reformatted")
	}
}

func TestWrite_RequiredAndOptional(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for name, want := range map[string]string{
		"a.needed": "(required)", "a.token": "(required)", "a.maybe": "(optional, unset by default)", "t.secret": "(optional, unset by default)",
	} {
		s, _ := r.Lookup(name)
		b := block(t, out, s)
		if !strings.Contains(b, want) {
			t.Errorf("%s: block lacks %q", name, want)
		}
		other := "(required)"
		if want == other {
			other = "(optional, unset by default)"
		}
		if strings.Contains(b, other) {
			t.Errorf("%s: block also holds %q", name, other)
		}
	}
}

// FR-21: the fixed secret text with the names filled in, secrets only.
func TestWrite_SecretText(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for _, s := range r.Settings() {
		b := block(t, out, s)
		if s.Secret {
			if !strings.Contains(b, secretText(s.Name)) {
				t.Errorf("%s: block lacks the exact secret text", s.Name)
			}
			if !strings.Contains(b, s.FileEnv) || !strings.Contains(b, s.FileFlag) {
				t.Errorf("%s: block does not name the _FILE variable and the _file flag", s.Name)
			}
		} else if strings.Contains(b, "(secret:") || strings.Contains(b, "_FILE") || strings.Contains(b, "_file") {
			t.Errorf("%s: a non-secret block mentions the secret text or file references", s.Name)
		}
	}
	if n := strings.Count(out, "(secret:"); n != 2 {
		t.Errorf("secret text appears %d times, want 2 (one per secret)", n)
	}
}

// Presence and secret text coexist (a required secret).
func TestWrite_RequiredSecret(t *testing.T) {
	r := testRegistry(t)
	s, _ := r.Lookup("a.token")
	b := block(t, render(t, r), s)
	if !strings.Contains(b, "(required)") || !strings.Contains(b, secretText("a.token")) {
		t.Error("required secret block lacks the presence or the secret text")
	}
}

// FR-21: sorted by logical name.
func TestWrite_SortedByName(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	var names []string
	for _, s := range r.Settings() {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	var order []string
	ps := paragraphs(out)
	for _, p := range ps {
		for _, n := range names {
			s, _ := r.Lookup(n)
			re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(s.Env) + `([^A-Za-z0-9_]|$)`)
			if re.MatchString(p) {
				order = append(order, n)
			}
		}
	}
	if strings.Join(order, ",") != strings.Join(names, ",") {
		t.Fatalf("setting paragraphs in order %v, want %v", order, names)
	}
}

// One paragraph per setting: nothing is merged or duplicated.
func TestWrite_OneBlockPerSetting(t *testing.T) {
	r := testRegistry(t)
	out := render(t, r)
	for _, s := range r.Settings() {
		_ = block(t, out, s) // exactly one paragraph each
		if n := strings.Count(out, "--"+s.Name+"="); n != 1 {
			t.Errorf("%s: flag form appears %d times, want 1", s.Name, n)
		}
	}
}

// FR-21: control flags section and the fixed OTEL_* last line.
func TestWrite_ControlFlagsAndOTELLine(t *testing.T) {
	out := render(t, testRegistry(t))
	for _, sp := range []string{"--help", "-help", "-h", "--version", "-version", "--check-config", "-check-config"} {
		re := regexp.MustCompile(`(^|[\s,|(:;])` + regexp.QuoteMeta(sp) + `($|[\s,|);:])`)
		if !re.MatchString(out) {
			t.Errorf("control flag spelling %s is not listed", sp)
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "OTEL_*") || !strings.Contains(last, "honoured") || !strings.Contains(last, "operator documentation") {
		t.Errorf("last line is not the fixed OTEL_* line: %q", last)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("output does not end with a newline")
	}
	// The control flags are not in a setting paragraph.
	ps := paragraphs(out)
	if len(ps) < 4 {
		t.Fatalf("only %d paragraphs", len(ps))
	}
}

func TestWrite_HeaderFirst(t *testing.T) {
	out := render(t, testRegistry(t))
	first := strings.SplitN(out, "\n", 2)[0]
	if strings.TrimSpace(first) == "" || strings.Contains(first, "CORRAL_") || strings.HasPrefix(first, "--") {
		t.Errorf("first line is not a header: %q", first)
	}
	if !strings.Contains(strings.ToLower(first), "corral") {
		t.Errorf("header does not name the program: %q", first)
	}
}

// Plain text, not a log record; deterministic.
func TestWrite_PlainTextDeterministic(t *testing.T) {
	r := testRegistry(t)
	a, b := render(t, r), render(t, r)
	if a != b {
		t.Fatal("output differs between calls")
	}
	if !utf8.ValidString(a) {
		t.Error("output is not valid UTF-8")
	}
	if strings.Contains(a, "\x1b") {
		t.Error("output contains terminal escape sequences")
	}
	for _, r := range a {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Errorf("control character U+%04X in help output", r)
			break
		}
	}
	trim := strings.TrimSpace(a)
	if strings.HasPrefix(trim, "{") || strings.Contains(a, `"level"`) || strings.Contains(a, `"msg"`) {
		t.Error("output looks like a log record")
	}
}

// AC-9 / FR-21: help never includes values from the environment, flags or
// files. Write sees only the registry; hostile process state changes nothing.
func TestWrite_NeverIncludesValues(t *testing.T) {
	m := newMarker(t)
	r := testRegistry(t)
	clean := render(t, r)
	for _, v := range []string{"CORRAL_T_TIMEOUT", "CORRAL_T_SECRET", "CORRAL_T_SECRET_FILE", "CORRAL_A_NEEDED", "CORRAL_LOG_FORMAT"} {
		t.Setenv(v, m)
	}
	hostile := render(t, r)
	if strings.Contains(hostile, m) {
		t.Fatal("help output carries an environment value")
	}
	if hostile != clean {
		t.Fatal("help output depends on the process environment")
	}
	// Secrets have no default to print, whatever is declared elsewhere.
	for _, s := range r.Settings() {
		if s.Secret && s.DefaultValue != nil {
			t.Errorf("%s: secret with a default value", s.Name)
		}
	}
}

func TestWrite_EmptyRegistry(t *testing.T) {
	r, err := registry.New()
	if err != nil {
		t.Skipf("empty registry not constructible: %v", err)
	}
	out := render(t, r)
	if !strings.Contains(out, "--help") || !strings.Contains(out, "OTEL_*") {
		t.Error("empty registry output lacks the control flags or the OTEL_* line")
	}
}

type failWriter struct{ n int }

var errBoom = errors.New("boom")

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errBoom
	}
	w.n--
	return len(p), nil
}

// Write errors from the writer are returned, never swallowed or panicked on.
func TestWrite_ReturnsWriterError(t *testing.T) {
	r := testRegistry(t)
	for budget := 0; budget < 6; budget++ {
		err := usage.Write(&failWriter{n: budget}, r)
		if err == nil {
			// A writer that accepts every write the implementation makes.
			continue
		}
		if !errors.Is(err, errBoom) {
			t.Errorf("budget %d: error %v does not wrap the writer error", budget, err)
		}
	}
	if err := usage.Write(&failWriter{n: 0}, r); !errors.Is(err, errBoom) {
		t.Errorf("immediate writer failure: got %v", err)
	}
}
