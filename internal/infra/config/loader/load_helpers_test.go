package loader_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// SPEC-003 T5 (loader resolution, ADR-0012): the loader contract for CORRAL
// settings, tested with a synthetic registry, source.Env snapshots and the
// memfs reader. No process globals are used.
//
// Compliance (FR-11, FR-14, FR-15): values, paths and file contents carry
// random markers. Failure messages print setting names, record counts and
// lengths, never raw values.
//
// Interface decisions (see the test-writer report): Result.Values has an entry
// for every registered setting; a failed or unset setting has Set=false;
// Result.Errors are *config.Error values in FR-13 order, each wrapping the
// sentinel named in the plan; Result.UnknownEnv is the rendered FR-8 list.

// load runs the loader with the given arguments, environment entries and reader.
func load(t testing.TB, r *registry.Registry, args []string, env []string, fs *memfs.FS) loader.Result {
	t.Helper()
	if fs == nil {
		fs = memfs.New()
	}
	return loader.Load(r, loader.Inputs{Args: args, Env: source.NewEnv(env), ReadFile: fs.Read})
}

// rec is an expected error record.
type rec struct {
	setting string
	err     error
}

// requireRecords asserts the exact records, in order: setting name and
// sentinel. Every error must be a *config.Error.
func requireRecords(t testing.TB, res loader.Result, want ...rec) {
	t.Helper()
	var got []string
	for _, e := range res.Errors {
		var ce *config.Error
		if !errors.As(e, &ce) {
			t.Fatalf("error %T is not a *config.Error", e)
		}
		got = append(got, ce.Setting)
	}
	var w []string
	for _, x := range want {
		w = append(w, x.setting)
	}
	if strings.Join(got, "\x00") != strings.Join(w, "\x00") {
		t.Fatalf("error settings = %q, want %q", got, w)
	}
	for i, x := range want {
		if x.err != nil && !errors.Is(res.Errors[i], x.err) {
			t.Errorf("record %d (%s): wrong sentinel (text %q)", i, x.setting, res.Errors[i].Error())
		}
	}
}

func requireNoErrors(t testing.TB, res loader.Result) {
	t.Helper()
	requireRecords(t, res)
}

// requireValue asserts the effective value, its source and whether it came
// from a file reference.
func requireValue(t testing.TB, res loader.Result, name string, want any, src loader.Source, fileRef bool) {
	t.Helper()
	v, ok := res.Values[name]
	if !ok {
		t.Fatalf("no Values entry for %s", name)
	}
	if !v.Set {
		t.Fatalf("%s is not set, want a value", name)
	}
	if !equal(v.V, want) {
		t.Errorf("%s: value %#v (%T), want %#v (%T)", name, v.V, v.V, want, want)
	}
	if v.Source != src {
		t.Errorf("%s: source %v, want %v", name, v.Source, src)
	}
	if v.FileRef != fileRef {
		t.Errorf("%s: FileRef %v, want %v", name, v.FileRef, fileRef)
	}
	if g, ok := res.Get(name); !ok || !equal(g, want) {
		t.Errorf("%s: Get disagrees with Values", name)
	}
	if res.Failed(name) {
		t.Errorf("%s: Failed is true for a setting with a value", name)
	}
}

func requireUnset(t testing.TB, res loader.Result, name string) {
	t.Helper()
	v, ok := res.Values[name]
	if !ok {
		t.Fatalf("no Values entry for %s", name)
	}
	if v.Set {
		t.Errorf("%s is set, want unset", name)
	}
	if _, ok := res.Get(name); ok {
		t.Errorf("%s: Get reports a value", name)
	}
}

func equal(a, b any) bool { return fmt.Sprintf("%T %#v", a, a) == fmt.Sprintf("%T %#v", b, b) }

// ---- the test registry ----

type regOpts struct {
	req        bool
	extraDecls []registry.Decl
	extraRules []registry.CrossRule
}

func d(name string, spec types.Spec, p registry.Presence, def string) registry.Decl {
	return registry.Decl{Name: name, Type: spec, Presence: p, Default: def, Description: "synthetic " + name}
}

func asInt(v any) int64 {
	i, _ := v.(int64)
	return i
}

func baseDecls() []registry.Decl {
	str := types.Spec{Kind: types.KindString}
	portRule := func(v any) error {
		if n := asInt(v); n < 1 || n > 65535 {
			return errors.New("port rule: " + fmt.Sprint(v))
		}
		return nil
	}
	port := d("t.port", types.Spec{Kind: types.KindInt}, registry.PresenceDefault, "8080")
	port.Rule = portRule
	secret := d("t.secret", types.Spec{Kind: types.KindText}, registry.PresenceOptional, "")
	secret.Secret = true
	return []registry.Decl{
		d("log.level", types.Spec{Kind: types.KindEnum, EnumValues: []string{"debug", "info", "warn", "error"}}, registry.PresenceDefault, "info"),
		d("tracing.enabled", types.Spec{Kind: types.KindBool}, registry.PresenceDefault, "false"),
		d("t.name", str, registry.PresenceDefault, "dflt"),
		d("t.n", types.Spec{Kind: types.KindInt}, registry.PresenceDefault, "5"),
		port,
		d("t.dur", types.Spec{Kind: types.KindDuration}, registry.PresenceDefault, "5s"),
		d("t.ratio", types.Spec{Kind: types.KindFloat}, registry.PresenceDefault, "1.0"),
		d("t.list", types.Spec{Kind: types.KindList, Elem: types.KindString}, registry.PresenceDefault, "d1,d2"),
		d("t.opt", types.Spec{Kind: types.KindInt}, registry.PresenceOptional, ""),
		d("t.a", types.Spec{Kind: types.KindInt}, registry.PresenceDefault, "1"),
		d("t.b", types.Spec{Kind: types.KindInt}, registry.PresenceDefault, "10"),
		d("t.x", str, registry.PresenceOptional, ""),
		secret,
	}
}

func rule(id string, involves []string, primary string, unset bool, check func(get func(string) (any, bool)) error) registry.CrossRule {
	return registry.CrossRule{ID: id, Involves: involves, Primary: primary, AppliesToUnset: unset, Check: check}
}

func baseRules() []registry.CrossRule {
	return []registry.CrossRule{
		rule("b-ge-a", []string{"t.a", "t.b"}, "t.b", false, func(get func(string) (any, bool)) error {
			a, _ := get("t.a")
			b, _ := get("t.b")
			if asInt(b) < asInt(a) {
				return fmt.Errorf("b %v below a %v", b, a)
			}
			return nil
		}),
		rule("opt-le-b", []string{"t.opt", "t.b"}, "t.opt", false, func(get func(string) (any, bool)) error {
			o, _ := get("t.opt")
			b, _ := get("t.b")
			if asInt(o) > asInt(b) {
				return fmt.Errorf("opt %v above b %v", o, b)
			}
			return nil
		}),
		rule("secret-short", []string{"t.secret"}, "t.secret", false, func(get func(string) (any, bool)) error {
			s, _ := get("t.secret")
			if str, _ := s.(string); strings.HasPrefix(str, "toolong") {
				return errors.New("secret rejected: " + str)
			}
			return nil
		}),
		rule("b-le-100", []string{"t.b"}, "t.b", false, func(get func(string) (any, bool)) error {
			b, _ := get("t.b")
			if asInt(b) > 100 {
				return fmt.Errorf("b %v above 100", b)
			}
			return nil
		}),
	}
}

func mkRegistry(tb testing.TB, o regOpts) *registry.Registry {
	tb.Helper()
	decls := baseDecls()
	if o.req {
		decls = append(decls, d("t.req", types.Spec{Kind: types.KindString}, registry.PresenceRequired, ""))
	}
	decls = append(decls, o.extraDecls...)
	rules := append(baseRules(), o.extraRules...)
	ns := []string{"t", "log", "tracing"}
	r, err := registry.New(registry.Group{Spec: "T", Namespaces: ns, Decls: decls, Rules: rules})
	if err != nil {
		tb.Fatalf("registry: %v", err)
	}
	return r
}

func baseReg(tb testing.TB) *registry.Registry { return mkRegistry(tb, regOpts{}) }

// renderRecords renders every error through the bootstrap logger (JSON), as
// the runtime does on the failure path, plus the unknown-env list.
func renderRecords(res loader.Result) string {
	var buf bytes.Buffer
	l := logging.BootstrapLogger(&buf)
	config.LogErrors(l, res.Errors)
	config.LogUnknownEnv(l, res.UnknownEnv)
	var sb strings.Builder
	sb.WriteString(buf.String())
	for _, e := range res.Errors {
		sb.WriteString(e.Error())
		sb.WriteString(fmt.Sprintf("%+v %#v", e, e))
	}
	sb.WriteString(strings.Join(res.Supplied, ","))
	return sb.String()
}

func requireNoMarker(t testing.TB, res loader.Result, markers ...string) {
	t.Helper()
	out := renderRecords(res)
	for _, m := range markers {
		if strings.Contains(out, m) {
			t.Errorf("rendered records carry a marker (value, path or content)")
		}
	}
}

func ep(m string, name string) string { return "/synthetic/" + m + "/" + name }

// newFS returns an empty in-memory reader. (The marker parameter keeps call
// sites uniform; paths are built with ep.)
func newFS(tb testing.TB, _ string) *memfs.FS { return memfs.New() }
