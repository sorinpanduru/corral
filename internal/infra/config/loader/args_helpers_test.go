package loader_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// SPEC-003 T4 (FR-6, FR-8 flags, FR-27, FR-28 command-line rows; AC-3, AC-12
// unit part). A test registry with synthetic settings is used so the parser is
// tested independently of the real settings.
//
// Compliance (FR-11, FR-14): supplied text is never echoed. Markers are random
// per test and are checked against every ArgProblem rendering. Failure
// messages print argument positions and setting names, never raw arguments.
//
// Interface decisions (see the test-writer report): Pos is the 0-based index
// into the args slice given to ParseArgs; problems are ordered by Pos;
// inputs are ordered by Pos; failedFlag keys are logical setting names and
// only true entries are present; no FlagInput is returned for a setting whose
// flag source failed (setting flag or its _file flag produced a record).

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

func decl(name string, secret bool) registry.Decl {
	return registry.Decl{Name: name, Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceOptional,
		Secret: secret, Description: "synthetic setting"}
}

// testRegistry holds t.x, t.y (strings), t.secret and a.b (secrets),
// log.level (enum with default) and tracing.enabled (bool with default).
func testRegistry(tb testing.TB) *registry.Registry {
	tb.Helper()
	r, err := registry.New(
		registry.Group{Spec: "T", Namespaces: []string{"t", "a"}, Decls: []registry.Decl{
			decl("t.x", false), decl("t.y", false), decl("t.secret", true), decl("a.b", true),
		}},
		registry.Group{Spec: "O", Namespaces: []string{"log", "tracing"}, Decls: []registry.Decl{
			{Name: "log.level", Type: types.Spec{Kind: types.KindEnum, EnumValues: []string{"debug", "info", "warn", "error"}},
				Presence: registry.PresenceDefault, Default: "info", Description: "log level"},
			{Name: "tracing.enabled", Type: types.Spec{Kind: types.KindBool}, Presence: registry.PresenceDefault,
				Default: "false", Description: "tracing switch"},
		}},
	)
	if err != nil {
		tb.Fatalf("registry: %v", err)
	}
	return r
}

type prob struct {
	pos     int
	setting string
	err     error // sentinel, nil to skip the check
}

type inp struct {
	pos     int
	setting string
	fileRef bool
	value   string
}

// parse runs ParseArgs and checks the invariants every result must satisfy.
func parse(t *testing.T, args ...string) ([]loader.FlagInput, []loader.ArgProblem, map[string]bool) {
	t.Helper()
	in, pr, failed := loader.ParseArgs(testRegistry(t), args)
	last := -1
	for _, p := range pr {
		if p.Pos < 0 || p.Pos >= len(args) {
			t.Errorf("problem position %d outside [0,%d)", p.Pos, len(args))
		}
		if p.Pos <= last {
			t.Errorf("problems are not in strictly increasing argument order (position %d after %d)", p.Pos, last)
		}
		last = p.Pos
		if p.Err == nil {
			t.Errorf("problem at %d has a nil Err", p.Pos)
		}
	}
	last = -1
	for _, i := range in {
		if i.Pos < last {
			t.Errorf("inputs are not in argument order")
		}
		last = i.Pos
		if failed[i.Setting] {
			t.Errorf("input returned for setting %s whose flag source failed", i.Setting)
		}
	}
	for k, v := range failed {
		if !v {
			t.Errorf("failedFlag holds a false entry for %s", k)
		}
	}
	return in, pr, failed
}

func requireProblems(t *testing.T, got []loader.ArgProblem, want []prob) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d problems %v, want %d %v", len(got), brief(got), len(want), briefWant(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Pos != w.pos || g.Setting != w.setting {
			t.Errorf("problem %d = (pos %d, %s), want (pos %d, %s)", i, g.Pos, g.Setting, w.pos, w.setting)
		}
		if w.err != nil && !errors.Is(g.Err, w.err) {
			t.Errorf("problem %d (%s): error %q does not match the expected sentinel %q", i, g.Setting, g.Err, w.err)
		}
	}
}

func brief(ps []loader.ArgProblem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s@%d", p.Setting, p.Pos))
	}
	return out
}

func briefWant(ps []prob) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s@%d", p.setting, p.pos))
	}
	return out
}

func requireInputs(t *testing.T, got []loader.FlagInput, want []inp) {
	t.Helper()
	var g []inp
	for _, i := range got {
		g = append(g, inp{i.Pos, i.Setting, i.FileRef, i.Value})
	}
	if len(g) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("inputs = %d entries %v, want %d entries %v (settings and positions only)", len(g), shapeInp(g), len(want), shapeInp(want))
	}
}

func shapeInp(in []inp) []string {
	var out []string
	for _, i := range in {
		out = append(out, fmt.Sprintf("%s@%d ref=%v vlen=%d", i.setting, i.pos, i.fileRef, len(i.value)))
	}
	return out
}

func requireFailed(t *testing.T, failed map[string]bool, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for k := range failed {
		got[k] = true
	}
	w := map[string]bool{}
	for _, k := range want {
		w[k] = true
	}
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("failedFlag = %v, want %v", keys(got), keys(w))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func noMarker(t *testing.T, probs []loader.ArgProblem, markers ...string) {
	t.Helper()
	for _, p := range probs {
		for _, txt := range []string{p.Err.Error(), p.Setting, fmt.Sprintf("%v", p.Err), fmt.Sprintf("%+v", p.Err), fmt.Sprintf("%#v", p.Err)} {
			for _, m := range markers {
				if strings.Contains(txt, m) {
					t.Errorf("problem at %d carries a marker (supplied text echoed)", p.Pos)
				}
			}
		}
	}
}
