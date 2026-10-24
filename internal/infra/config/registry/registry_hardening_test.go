package registry_test

import (
	"reflect"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// Hardening of New (SPEC-003 T3, iteration 2). Synthetic names only.

func TestNew_RefusesDefaultTextWithoutDefaultPresence(t *testing.T) {
	m := newMarker(t)
	for _, p := range []registry.Presence{registry.PresenceRequired, registry.PresenceOptional} {
		d := opt("a.b")
		d.Presence = p
		d.Default = m
		requireRefused(t, []registry.Group{one(d)}, []string{"a.b"}, m)
	}
}

func TestNew_RefusesDuplicateRuleID(t *testing.T) {
	g := one(opt("a.x"), opt("a.y"))
	g.Rules = []registry.CrossRule{okRule("dup", []string{"a.x"}, "a.x"), okRule("dup", []string{"a.y"}, "a.y")}
	requireRefused(t, []registry.Group{g}, []string{"dup"})
	g1 := grp("S1", []string{"a"}, opt("a.x"))
	g1.Rules = []registry.CrossRule{okRule("dup", []string{"a.x"}, "a.x")}
	g2 := grp("S2", []string{"b"}, opt("b.y"))
	g2.Rules = []registry.CrossRule{okRule("dup", []string{"b.y"}, "b.y")}
	requireRefused(t, []registry.Group{g1, g2}, []string{"dup"})
}

func TestNew_RefusesEmptyGroupSpec(t *testing.T) {
	requireRefused(t, []registry.Group{grp("", []string{"a"}, opt("a.x"))}, []string{"group"})
}

func TestRegistry_ReturnsDeepCopies(t *testing.T) {
	enum := types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}
	lst := types.Spec{Kind: types.KindList, Elem: types.KindString}
	g := one(
		withDefault("a.e", enum, "json"),
		withDefault("a.l", lst, "x,y"),
		opt("a.o"),
	)
	g.Rules = []registry.CrossRule{okRule("r", []string{"a.e", "a.o"}, "a.e")}
	r := mustNew(t, g)

	// Mutating inputs after New does not change the registry.
	enum.EnumValues[0] = "tampered"
	g.Rules[0].Involves[0] = "tampered"

	mutate := func(s registry.Setting) {
		if len(s.Type.EnumValues) > 0 {
			s.Type.EnumValues[0] = "tampered"
		}
		if l, ok := s.DefaultValue.([]string); ok {
			l[0] = "tampered"
		}
	}
	for _, s := range r.Settings() {
		mutate(s)
	}
	if s, ok := r.Lookup("a.e"); ok {
		mutate(s)
	}
	if s, ok := r.Lookup("a.l"); ok {
		mutate(s)
	}
	if s, _, ok := r.ByFlag("a.e"); ok {
		mutate(s)
	}
	if s, _, ok := r.ByEnv("CORRAL_A_L"); ok {
		mutate(s)
	}
	r.CrossRules()[0].Involves[0] = "tampered"

	e, _ := r.Lookup("a.e")
	if !reflect.DeepEqual(e.Type.EnumValues, []string{"json", "text"}) {
		t.Errorf("enum values changed: %v", e.Type.EnumValues)
	}
	l, _ := r.Lookup("a.l")
	if !reflect.DeepEqual(l.DefaultValue, []string{"x", "y"}) {
		t.Errorf("default list changed: %v", l.DefaultValue)
	}
	if got := r.CrossRules()[0].Involves; !reflect.DeepEqual(got, []string{"a.e", "a.o"}) {
		t.Errorf("involves changed: %v", got)
	}
}
