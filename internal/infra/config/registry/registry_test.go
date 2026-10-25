package registry_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// SPEC-003 T3 (FR-1, FR-2, FR-3, FR-14 declaration rules, FR-15 reference
// names, FR-30 declaration; AC-1). Registries built here use synthetic
// namespaces (a, t, ...) so the registry is tested independently of the real
// settings.
//
// Interface decisions (see the test-writer report): New returns one error
// whose text names every offending setting and never carries a default value;
// DerivedNameConflicts returns the colliding derived names (flag names and
// environment names), sorted and without duplicates.

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

func str() types.Spec { return types.Spec{Kind: types.KindString} }

// opt is a minimal valid optional string setting.
func opt(name string) registry.Decl {
	return registry.Decl{Name: name, Type: str(), Presence: registry.PresenceOptional, Description: "synthetic setting"}
}

func secret(name string) registry.Decl {
	d := opt(name)
	d.Secret = true
	return d
}

func withDefault(name string, spec types.Spec, def string) registry.Decl {
	return registry.Decl{Name: name, Type: spec, Presence: registry.PresenceDefault, Default: def, Description: "synthetic setting"}
}

func grp(spec string, ns []string, decls ...registry.Decl) registry.Group {
	return registry.Group{Spec: spec, Namespaces: ns, Decls: decls}
}

func one(decls ...registry.Decl) registry.Group {
	seen := map[string]bool{}
	var ns []string
	for _, d := range decls {
		first, _, _ := strings.Cut(d.Name, ".")
		if !seen[first] {
			seen[first] = true
			ns = append(ns, first)
		}
	}
	return grp("SPEC-T", ns, decls...)
}

func mustNew(tb testing.TB, gs ...registry.Group) *registry.Registry {
	tb.Helper()
	r, err := registry.New(gs...)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return r
}

// requireRefused asserts New fails, returns no registry, names every given
// setting in the error text and carries no marker.
func requireRefused(t *testing.T, g []registry.Group, names []string, markers ...string) {
	t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("New panicked: %v", r)
			}
		}()
		r, err := registry.New(g...)
		if err == nil {
			t.Fatalf("New accepted the declarations (registry %v)", r != nil)
		}
		if r != nil {
			t.Errorf("New returned a registry together with an error")
		}
		for _, n := range names {
			if !strings.Contains(err.Error(), n) {
				t.Errorf("error text does not name %q: %q", n, err.Error())
			}
		}
		for _, m := range markers {
			if strings.Contains(err.Error(), m) {
				t.Errorf("error text carries a default value")
			}
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			t.Errorf("New returned a joined error list, want one error")
		}
	}()
}

// ---- happy path ----

func TestNew_BuildsAndSortsSettings(t *testing.T) {
	r := mustNew(t,
		grp("A", []string{"zed", "alpha"}, opt("zed.one"), opt("alpha.two")),
		grp("B", []string{"mid"}, opt("mid.x")),
	)
	var got []string
	for _, s := range r.Settings() {
		got = append(got, s.Name)
	}
	want := []string{"alpha.two", "mid.x", "zed.one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Settings() names = %v, want %v (sorted by name)", got, want)
	}
	for _, n := range want {
		if s, ok := r.Lookup(n); !ok || s.Name != n {
			t.Errorf("Lookup(%s) = %v, %v", n, s.Name, ok)
		}
	}
	if _, ok := r.Lookup("nope.x"); ok {
		t.Error("Lookup found an unregistered name")
	}
	if _, ok := r.Lookup(""); ok {
		t.Error("Lookup found the empty name")
	}
}

func TestSettings_ReturnsACopy(t *testing.T) {
	r := mustNew(t, one(opt("a.b"), opt("a.c")))
	s := r.Settings()
	s[0].Name = "tampered"
	if r.Settings()[0].Name != "a.b" {
		t.Fatal("Settings() exposes internal state")
	}
}

func TestNew_ValidNames(t *testing.T) {
	for _, n := range []string{"a.b", "a.b2", "a.b_c", "ab.cd.ef", "a.file_x", "a.filex", "a.profile", "a.file.b", "a.b__c", "log.level", "tracing.sample_ratio", "a.x9_9"} {
		if _, err := registry.New(one(opt(n))); err != nil {
			t.Errorf("valid name %q refused: %v", n, err)
		}
	}
}

func TestNew_NoRuleIsFine(t *testing.T) {
	d := withDefault("a.b", types.Spec{Kind: types.KindInt}, "5")
	d.Rule = nil
	mustNew(t, one(d))
}

// FR-1: defaults are parsed once, with the type, into DefaultValue.
func TestSetting_DefaultValueIsParsed(t *testing.T) {
	r := mustNew(t, one(
		withDefault("a.i", types.Spec{Kind: types.KindInt}, "5"),
		withDefault("a.d", types.Spec{Kind: types.KindDuration}, "5s"),
		withDefault("a.e", types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}, "JSON"),
		withDefault("a.b", types.Spec{Kind: types.KindBool}, "TRUE"),
		withDefault("a.f", types.Spec{Kind: types.KindFloat}, ".5"),
		withDefault("a.h", types.Spec{Kind: types.KindHostPort}, "127.0.0.1:9187"),
		withDefault("a.l", types.Spec{Kind: types.KindList, Elem: types.KindString}, "x, y"),
		opt("a.opt"),
		registry.Decl{Name: "a.req", Type: str(), Presence: registry.PresenceRequired, Description: "synthetic"},
	))
	want := map[string]any{
		"a.i": int64(5), "a.d": 5 * time.Second, "a.e": "json", "a.b": true, "a.f": 0.5,
		"a.h": "127.0.0.1:9187", "a.l": []string{"x", "y"}, "a.opt": nil, "a.req": nil,
	}
	for name, w := range want {
		s, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if !reflect.DeepEqual(s.DefaultValue, w) {
			t.Errorf("%s: DefaultValue = %#v, want %#v", name, s.DefaultValue, w)
		}
	}
}

// The default is checked with the declared rule on the parsed value.
func TestNew_RuleSeesParsedDefault(t *testing.T) {
	var got any
	d := withDefault("a.i", types.Spec{Kind: types.KindInt}, "7")
	d.Rule = func(v any) error { got = v; return nil }
	mustNew(t, one(d))
	if got != int64(7) {
		t.Fatalf("rule saw %#v, want int64(7)", got)
	}
}

// ---- AC-1 refusal cases ----

func TestNew_RefusesDuplicateName(t *testing.T) {
	requireRefused(t, []registry.Group{one(opt("a.b"), opt("a.b"))}, []string{"a.b"})
	// across groups
	requireRefused(t, []registry.Group{grp("X", []string{"a"}, opt("a.b")), grp("Y", []string{"a2"}, opt("a.b"))}, []string{"a.b"})
}

func TestNew_RefusesBadNames(t *testing.T) {
	for _, n := range []string{
		"ab",         // single segment
		"",           // empty
		"a.B",        // uppercase segment
		"A.b",        // uppercase namespace
		"a.b-c",      // hyphen
		"a-x.b",      // hyphen in namespace
		"a..b",       // empty segment
		".a.b",       // leading dot
		"a.b.",       // trailing dot
		"a.1b",       // segment starts with a digit
		"1a.b",       // namespace starts with a digit
		"a._b",       // segment starts with an underscore
		"a.b c",      // space
		"a.b\n",      // control character
		"a.\u00e9",   // non-ASCII
		"a.b.file",   // reserved last segment
		"a.file",     // reserved last segment
		"a.b_file",   // reserved suffix
		"a.my_file",  // reserved suffix
		"a.b.c_file", // reserved suffix, deeper
	} {
		t.Run(fmt.Sprintf("%q", n), func(t *testing.T) {
			first, _, _ := strings.Cut(n, ".")
			g := grp("S", []string{first}, opt(n))
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				if _, err := registry.New(g); err == nil {
					t.Fatal("accepted")
				}
			}()
		})
	}
	// The error names the offender (names are programmer constants).
	requireRefused(t, []registry.Group{grp("S", []string{"a"}, opt("a.b.file"))}, []string{"a.b.file"})
	requireRefused(t, []registry.Group{grp("S", []string{"a"}, opt("a.b_file"))}, []string{"a.b_file"})
}

// FR-2: two names that collide on the environment variable.
func TestNew_RefusesEnvCollision(t *testing.T) {
	requireRefused(t, []registry.Group{grp("S", []string{"a", "a_b"}, opt("a.b_c"), opt("a_b.c"))}, []string{"a.b_c", "a_b.c"})
	requireRefused(t, []registry.Group{
		grp("S1", []string{"a"}, opt("a.b_c")),
		grp("S2", []string{"a_b"}, opt("a_b.c")),
	}, []string{"a.b_c", "a_b.c"})
}

// FR-2: a_b.file next to a secret a.b is refused (the reserved segment rule
// catches it; the derived-name check is covered by DerivedNameConflicts).
func TestNew_RefusesFileReferenceCollisions(t *testing.T) {
	requireRefused(t, []registry.Group{grp("S", []string{"a", "a_b"}, secret("a.b"), opt("a_b.file"))}, []string{"a_b.file"})
	requireRefused(t, []registry.Group{grp("S", []string{"a"}, secret("a.b"), opt("a.b_file"))}, []string{"a.b_file"})
	requireRefused(t, []registry.Group{grp("S", []string{"a"}, secret("a.b"), opt("a.b.file"))}, []string{"a.b.file"})
}

// FR-14: a secret cannot have a default; the default is never printed.
func TestNew_RefusesSecretWithDefault(t *testing.T) {
	m := newMarker(t)
	d := withDefault("a.pw", str(), m)
	d.Secret = true
	requireRefused(t, []registry.Group{one(d)}, []string{"a.pw"}, m)
}

func TestNew_RefusesDefaultFailingType(t *testing.T) {
	m := newMarker(t)
	for name, d := range map[string]registry.Decl{
		"int":                             withDefault("a.i", types.Spec{Kind: types.KindInt}, m),
		"bool":                            withDefault("a.b", types.Spec{Kind: types.KindBool}, "yes"),
		"duration":                        withDefault("a.d", types.Spec{Kind: types.KindDuration}, "5"),
		"enum":                            withDefault("a.e", types.Spec{Kind: types.KindEnum, EnumValues: []string{"x"}}, m),
		"hostport":                        withDefault("a.h", types.Spec{Kind: types.KindHostPort}, ":9187"),
		"string empty without AllowEmpty": withDefault("a.s", str(), ""),
		"control char":                    withDefault("a.c", str(), "x\x00"+m),
		"float plus":                      withDefault("a.f", types.Spec{Kind: types.KindFloat}, "+1"),
	} {
		t.Run(name, func(t *testing.T) {
			requireRefused(t, []registry.Group{one(d)}, []string{d.Name}, m)
		})
	}
}

func TestNew_RefusesDefaultFailingRule(t *testing.T) {
	m := newMarker(t)
	d := withDefault("a.n", str(), m)
	d.Rule = func(v any) error { return errors.New("rule says no: " + fmt.Sprint(v)) }
	requireRefused(t, []registry.Group{one(d)}, []string{"a.n"}, m)
	// The rule is not run for settings without a default.
	called := false
	o := opt("a.o")
	o.Rule = func(any) error { called = true; return errors.New("no") }
	mustNew(t, one(o))
	if called {
		t.Error("rule ran for a setting with no default")
	}
}

func TestNew_RefusesBadPresence(t *testing.T) {
	d := opt("a.b")
	d.Presence = registry.Presence(99)
	requireRefused(t, []registry.Group{one(d)}, []string{"a.b"})
}

func TestNew_RefusesBadDescription(t *testing.T) {
	for name, desc := range map[string]string{
		"empty": "", "multi-line": "line one\nline two", "carriage return": "a\rb", "NUL": "a\x00b",
		"DEL": "a\x7fb", "C1": "a\u0085b", "tab": "a\tb",
	} {
		t.Run(name, func(t *testing.T) {
			d := opt("a.b")
			d.Description = desc
			requireRefused(t, []registry.Group{one(d)}, []string{"a.b"})
		})
	}
	d := opt("a.b")
	d.Description = "A one-line description, with punctuation (and Unicode: \u00e9)."
	mustNew(t, one(d))
}

func TestNew_RefusesBadTypeSpec(t *testing.T) {
	for name, s := range map[string]types.Spec{
		"enum without values": {Kind: types.KindEnum},
		"list of list":        {Kind: types.KindList, Elem: types.KindList},
		"list of text":        {Kind: types.KindList, Elem: types.KindText},
		"allow-zero on int":   {Kind: types.KindInt, AllowZero: true},
		"unknown kind":        {Kind: types.Kind(100)},
	} {
		t.Run(name, func(t *testing.T) {
			d := opt("a.b")
			d.Type = s
			requireRefused(t, []registry.Group{one(d)}, []string{"a.b"})
		})
	}
}

// Namespace ownership (FR-2, ADR-0011).
func TestNew_RefusesNamespaceOutsideGroup(t *testing.T) {
	requireRefused(t, []registry.Group{grp("S", []string{"a"}, opt("b.x"))}, []string{"b.x"})
	requireRefused(t, []registry.Group{grp("S", nil, opt("a.x"))}, []string{"a.x"})
	requireRefused(t, []registry.Group{grp("S", []string{"ab"}, opt("a.x"))}, []string{"a.x"})
}

func TestNew_RefusesNamespaceClaimedTwice(t *testing.T) {
	g1 := grp("SPEC-ONE", []string{"a"}, opt("a.x"))
	g2 := grp("SPEC-TWO", []string{"a", "b"}, opt("b.y"))
	requireRefused(t, []registry.Group{g1, g2}, []string{"a"})
	// Even when the second group declares nothing in it.
	requireRefused(t, []registry.Group{g1, grp("SPEC-TWO", []string{"a"})}, []string{"a"})
	// Two specs may not both declare in one namespace.
	requireRefused(t, []registry.Group{grp("S1", []string{"a"}, opt("a.x")), grp("S2", []string{"a"}, opt("a.y"))}, []string{"a"})
}

func TestNew_SeveralNamespacesPerGroup(t *testing.T) {
	mustNew(t, grp("S", []string{"a", "b"}, opt("a.x"), opt("b.y")))
	// A namespace with no declarations is fine.
	mustNew(t, grp("S", []string{"a", "unused"}, opt("a.x")))
}

// FR-1, FR-30: cross-setting rule declaration checks.
func okRule(id string, involves []string, primary string) registry.CrossRule {
	return registry.CrossRule{ID: id, Involves: involves, Primary: primary,
		Check: func(get func(string) (any, bool)) error { return nil }}
}

func TestNew_CrossRuleChecks(t *testing.T) {
	mk := func(rules ...registry.CrossRule) registry.Group {
		g := one(opt("a.x"), opt("a.y"))
		g.Rules = rules
		return g
	}
	mustNew(t, mk(okRule("r1", []string{"a.x", "a.y"}, "a.x")))
	mustNew(t, mk(okRule("r1", []string{"a.x"}, "a.x")))

	bad := map[string]registry.CrossRule{
		"primary not among involved": okRule("bad-primary", []string{"a.x"}, "a.y"),
		"primary empty":              okRule("empty-primary", []string{"a.x"}, ""),
		"primary unregistered":       okRule("unreg-primary", []string{"a.x", "zz.q"}, "zz.q"),
		"no involved":                okRule("no-involved", nil, "a.x"),
		"empty involved":             okRule("empty-involved", []string{}, "a.x"),
		"involves unregistered":      okRule("unreg-involved", []string{"a.x", "a.nope"}, "a.x"),
		"nil check":                  {ID: "nil-check", Involves: []string{"a.x"}, Primary: "a.x"},
	}
	for name, rule := range bad {
		t.Run(name, func(t *testing.T) {
			requireRefused(t, []registry.Group{mk(rule)}, []string{rule.ID})
		})
	}
}

// A rule may involve settings declared by another group (registry-wide
// resolution), as long as they are registered.
func TestNew_CrossRuleAcrossGroups(t *testing.T) {
	g1 := grp("S1", []string{"a"}, opt("a.x"))
	g2 := grp("S2", []string{"b"}, opt("b.y"))
	g2.Rules = []registry.CrossRule{okRule("cross", []string{"a.x", "b.y"}, "b.y")}
	mustNew(t, g1, g2)
	// and in the opposite declaration order
	g2b := grp("S2", []string{"b"}, opt("b.y"))
	g1b := grp("S1", []string{"a"}, opt("a.x"))
	g1b.Rules = []registry.CrossRule{okRule("cross", []string{"a.x", "b.y"}, "a.x")}
	mustNew(t, g2b, g1b)
}

func TestCrossRules_DeclarationOrder(t *testing.T) {
	g1 := one(opt("a.x"), opt("a.y"))
	g1.Rules = []registry.CrossRule{okRule("first", []string{"a.x"}, "a.x"), okRule("second", []string{"a.y"}, "a.y")}
	g2 := grp("S2", []string{"b"}, opt("b.z"))
	g2.Rules = []registry.CrossRule{okRule("third", []string{"b.z", "a.x"}, "b.z")}
	r := mustNew(t, g1, g2)
	var ids []string
	for _, c := range r.CrossRules() {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []string{"first", "second", "third"}) {
		t.Fatalf("CrossRules() ids = %v, want declaration order", ids)
	}
}

// One error lists every problem (not just the first), deterministically.
func TestNew_ReportsAllProblemsDeterministically(t *testing.T) {
	m := newMarker(t)
	pw := withDefault("a.pw", str(), m)
	pw.Secret = true
	g := grp("S", []string{"a"},
		opt("a.dup"), opt("a.dup"), opt("a.b_file"), opt("a.OK"), pw,
		withDefault("a.bad", types.Spec{Kind: types.KindInt}, "x"))
	names := []string{"a.dup", "a.b_file", "a.OK", "a.pw", "a.bad"}
	requireRefused(t, []registry.Group{g}, names, m)
	_, e1 := registry.New(g)
	_, e2 := registry.New(g)
	if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
		t.Fatal("error text is not deterministic")
	}
}

func TestNew_NeverPanics(t *testing.T) {
	weird := []registry.Group{
		{},
		{Spec: "x"},
		{Namespaces: []string{""}},
		{Namespaces: []string{"a"}, Decls: []registry.Decl{{}}},
		{Namespaces: []string{"a"}, Decls: []registry.Decl{{Name: "a.b"}}},
		{Namespaces: []string{"a"}, Decls: []registry.Decl{opt("a.b")}, Rules: []registry.CrossRule{{}}},
		{Namespaces: []string{"a"}, Decls: []registry.Decl{opt("a.b")}, Rules: []registry.CrossRule{{Involves: []string{""}, Primary: ""}}},
		{Namespaces: []string{"a", "a"}, Decls: []registry.Decl{opt("a.b")}},
		{Namespaces: []string{"a"}, Decls: []registry.Decl{{Name: strings.Repeat("a.", 5000) + "b", Type: types.Spec{Kind: types.Kind(-1)}}}},
	}
	for i, g := range weird {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d panicked: %v", i, r)
				}
			}()
			_, _ = registry.New(g)
		}()
	}
}

// ---- derived names (FR-2, FR-3, FR-15) ----

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"log.level":            "CORRAL_LOG_LEVEL",
		"tracing.sample_ratio": "CORRAL_TRACING_SAMPLE_RATIO",
		"admin.listen":         "CORRAL_ADMIN_LISTEN",
		"a.b":                  "CORRAL_A_B",
		"a.b.c":                "CORRAL_A_B_C",
		"upstream.password":    "CORRAL_UPSTREAM_PASSWORD",
		"a.b_c":                "CORRAL_A_B_C",
		"a_b.c":                "CORRAL_A_B_C",
	} {
		if got := registry.EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetting_DerivedNames(t *testing.T) {
	r := mustNew(t, one(opt("t.x"), secret("t.secret"), opt("log.level")))
	cases := []struct {
		name, flag, env, fileFlag, fileEnv string
	}{
		{"t.x", "t.x", "CORRAL_T_X", "", ""},
		{"log.level", "log.level", "CORRAL_LOG_LEVEL", "", ""},
		{"t.secret", "t.secret", "CORRAL_T_SECRET", "t.secret_file", "CORRAL_T_SECRET_FILE"},
	}
	for _, c := range cases {
		s, ok := r.Lookup(c.name)
		if !ok {
			t.Fatalf("%s missing", c.name)
		}
		if s.Flag != c.flag || s.Env != c.env || s.FileFlag != c.fileFlag || s.FileEnv != c.fileEnv {
			t.Errorf("%s: derived names = %q %q %q %q, want %q %q %q %q", c.name,
				s.Flag, s.Env, s.FileFlag, s.FileEnv, c.flag, c.env, c.fileFlag, c.fileEnv)
		}
	}
}

func TestByFlag(t *testing.T) {
	r := mustNew(t, one(opt("t.x"), secret("t.secret")))
	if s, ref, ok := r.ByFlag("t.x"); !ok || ref || s.Name != "t.x" {
		t.Errorf("ByFlag(t.x) = %v %v %v", s.Name, ref, ok)
	}
	if s, ref, ok := r.ByFlag("t.secret"); !ok || ref || s.Name != "t.secret" {
		t.Errorf("ByFlag(t.secret) = %v %v %v", s.Name, ref, ok)
	}
	if s, ref, ok := r.ByFlag("t.secret_file"); !ok || !ref || s.Name != "t.secret" {
		t.Errorf("ByFlag(t.secret_file) = %v %v %v, want the secret as a file reference", s.Name, ref, ok)
	}
	for _, f := range []string{
		"t.x_file", // non-secret: no file reference (FR-15)
		"t.nope", "", "t", "T.X", "t.X", "-t.x", "--t.x", "t.x=1", " t.x", "t.x ", "t.secret_FILE", "t.secret.file", "t.secret_file_file",
		"CORRAL_T_X", // an environment name is not a flag name
	} {
		if s, _, ok := r.ByFlag(f); ok {
			t.Errorf("ByFlag(%q) matched %s", f, s.Name)
		}
	}
}

func TestByEnv(t *testing.T) {
	r := mustNew(t, one(opt("t.x"), secret("t.secret"), opt("log.level")))
	if s, ref, ok := r.ByEnv("CORRAL_T_X"); !ok || ref || s.Name != "t.x" {
		t.Errorf("ByEnv(CORRAL_T_X) = %v %v %v", s.Name, ref, ok)
	}
	if s, ref, ok := r.ByEnv("CORRAL_LOG_LEVEL"); !ok || ref || s.Name != "log.level" {
		t.Errorf("ByEnv(CORRAL_LOG_LEVEL) = %v %v %v", s.Name, ref, ok)
	}
	if s, ref, ok := r.ByEnv("CORRAL_T_SECRET"); !ok || ref || s.Name != "t.secret" {
		t.Errorf("ByEnv(CORRAL_T_SECRET) = %v %v %v", s.Name, ref, ok)
	}
	if s, ref, ok := r.ByEnv("CORRAL_T_SECRET_FILE"); !ok || !ref || s.Name != "t.secret" {
		t.Errorf("ByEnv(CORRAL_T_SECRET_FILE) = %v %v %v, want the secret as a file reference", s.Name, ref, ok)
	}
	for _, e := range []string{
		"CORRAL_T_X_FILE", // non-secret: no file reference
		"CORRAL_UNKNOWN", "", "CORRAL_", "corral_t_x", "Corral_T_X", "t.x", "T_X", " CORRAL_T_X", "CORRAL_T_X ", "CORRAL_T_SECRET_FILE_FILE",
		"OTEL_TRACES_EXPORTER",
	} {
		if s, _, ok := r.ByEnv(e); ok {
			t.Errorf("ByEnv(%q) matched %s", e, s.Name)
		}
	}
}

// ---- DerivedNameConflicts: the derived-name rule without the segment rule ----

func conflicts(decls ...registry.Decl) []string { return registry.DerivedNameConflicts(decls) }

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// AC-1: the derived-name check includes the <name>_file and CORRAL_<NAME>_FILE
// names. These decls bypass the segment rule on purpose.
func TestDerivedNameConflicts_FileReferenceNames(t *testing.T) {
	got := conflicts(secret("a.b"), opt("a.b_file"))
	if !has(got, "a.b_file") {
		t.Errorf("flag collision a.b_file not reported: %v", got)
	}
	if !has(got, "CORRAL_A_B_FILE") {
		t.Errorf("environment collision CORRAL_A_B_FILE not reported: %v", got)
	}

	// a_b.file only collides on the environment name (a_b.file != a.b_file).
	got = conflicts(secret("a.b"), opt("a_b.file"))
	if !has(got, "CORRAL_A_B_FILE") {
		t.Errorf("environment collision CORRAL_A_B_FILE not reported: %v", got)
	}
	if has(got, "a.b_file") || has(got, "a_b.file") {
		t.Errorf("reported a flag collision that does not exist: %v", got)
	}

	// a.b.file maps to CORRAL_A_B_FILE as well.
	got = conflicts(secret("a.b"), opt("a.b.file"))
	if !has(got, "CORRAL_A_B_FILE") {
		t.Errorf("a.b.file collision not reported: %v", got)
	}
}

func TestDerivedNameConflicts_PlainCollisions(t *testing.T) {
	got := conflicts(opt("a.b_c"), opt("a_b.c"))
	if !reflect.DeepEqual(got, []string{"CORRAL_A_B_C"}) {
		t.Errorf("a.b_c / a_b.c: got %v, want [CORRAL_A_B_C]", got)
	}
	got = conflicts(opt("a.b"), opt("a.b"))
	if !has(got, "a.b") || !has(got, "CORRAL_A_B") {
		t.Errorf("duplicate: got %v, want both the flag and the environment name", got)
	}
}

func TestDerivedNameConflicts_NoneForDistinctNames(t *testing.T) {
	for i, ds := range [][]registry.Decl{
		{}, {opt("a.b"), opt("a.c"), opt("log.level")}, {secret("a.b"), opt("a.c")}, {secret("a.b")},
		{secret("t.secret"), opt("t.secret2")}, {secret("a.b"), secret("a.b_c"), secret("a_b.d")},
	} {
		if got := registry.DerivedNameConflicts(ds); len(got) != 0 {
			t.Errorf("case %d: unexpected conflicts %v", i, got)
		}
	}
}

// Sorted ascending, without duplicates.
func TestDerivedNameConflicts_SortedUnique(t *testing.T) {
	got := conflicts(secret("a.b"), opt("a.b_file"), opt("a.b_file"), opt("x.y_z"), opt("x_y.z"), opt("x.y_z"))
	if !sort.StringsAreSorted(got) {
		t.Errorf("not sorted: %v", got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		if seen[g] {
			t.Errorf("duplicate %q in %v", g, got)
		}
		seen[g] = true
	}
	if len(got) == 0 {
		t.Error("no conflicts reported")
	}
}
