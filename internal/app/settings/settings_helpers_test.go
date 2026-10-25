package settings_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// SPEC-003 T10: the SPEC-001 settings under the SPEC-003 mechanism, tested
// through the real registry (settings.Registry()) and the real loader. No
// process globals are read.
//
// Compliance (SPEC-003 FR-11): supplied values are synthetic; failure messages
// print setting names and lengths, never raw values.
//
// Interface decisions (see the test-writer report): the SPEC-001 group is the
// element of settings.Groups() whose Spec is "SPEC-001"; settings.Registry()
// may hold further groups (SPEC-002 will add one), so the tests look settings
// up by name and never count the whole registry.

const spec001 = "SPEC-001"

var spec001Names = []string{"admin.listen", "log.format", "log.level", "tracing.enabled", "tracing.sample_ratio"}

func mustRegistry(tb testing.TB) *registry.Registry {
	tb.Helper()
	r, err := settings.Registry()
	if err != nil {
		tb.Fatalf("settings.Registry: %v", err)
	}
	return r
}

func spec001Group(tb testing.TB) registry.Group {
	tb.Helper()
	for _, g := range settings.Groups() {
		if g.Spec == spec001 {
			return g
		}
	}
	tb.Fatalf("settings.Groups() has no group %q", spec001)
	return registry.Group{}
}

func load(tb testing.TB, args []string, env []string) loader.Result {
	tb.Helper()
	return loader.Load(mustRegistry(tb), loader.Inputs{Args: args, Env: source.NewEnv(env), ReadFile: memfs.New().Read})
}

// onlyFor returns the errors whose setting is one of names (so a later
// SPEC-002 required setting cannot disturb SPEC-001 assertions).
func onlyFor(res loader.Result, names ...string) []error {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
		set[registry.EnvName(n)] = true // the environment spelling of each name
	}
	var out []error
	for _, e := range res.Errors {
		var ce *config.Error
		if errors.As(e, &ce) && set[ce.Setting] {
			out = append(out, e)
		}
	}
	return out
}

func settingsOf(errs []error) []string {
	var out []string
	for _, e := range errs {
		var ce *config.Error
		if errors.As(e, &ce) {
			out = append(out, ce.Setting)
		}
	}
	return out
}

func requireOnly(tb testing.TB, res loader.Result, names []string, want ...string) {
	tb.Helper()
	got := settingsOf(onlyFor(res, names...))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		tb.Fatalf("errors for %v = %v, want %v", names, got, want)
	}
}

// specTable is the SPEC-003 Interfaces table for SPEC-001, written
// independently of the implementation.
func specTable() registry.Group {
	enum := func(vs ...string) types.Spec { return types.Spec{Kind: types.KindEnum, EnumValues: vs} }
	return registry.Group{Spec: spec001, Namespaces: []string{"log", "admin", "tracing"}, Decls: []registry.Decl{
		{Name: "log.format", Type: enum("json", "text"), Presence: registry.PresenceDefault, Default: "json", Description: "Log output format."},
		{Name: "log.level", Type: enum("debug", "info", "warn", "error"), Presence: registry.PresenceDefault, Default: "info", Description: "Minimum log level."},
		{Name: "admin.listen", Type: types.Spec{Kind: types.KindHostPort}, Presence: registry.PresenceDefault, Default: "127.0.0.1:9187", Description: "Admin HTTP listener address."},
		{Name: "tracing.enabled", Type: types.Spec{Kind: types.KindBool}, Presence: registry.PresenceDefault, Default: "false", Description: "Enable OTLP tracing."},
		{Name: "tracing.sample_ratio", Type: types.Spec{Kind: types.KindFloat}, Presence: registry.PresenceDefault, Default: "1.0", Description: "Trace sampling ratio.",
			Rule: func(v any) error {
				if f, _ := v.(float64); f < 0 || f > 1 {
					return fmt.Errorf("out of range")
				}
				return nil
			}},
	}}
}
