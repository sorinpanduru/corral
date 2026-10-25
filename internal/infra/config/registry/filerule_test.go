package registry_test

import (
	"errors"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// ADR-0020, SPEC-002 FR-44/FR-45: a File rule is allowed only on non-secret
// string settings (registry build check).

var errCheck = errors.New("synthetic check failure")

func pemDecl(name string) registry.Decl {
	return registry.Decl{
		Name: name, Type: types.Spec{Kind: types.KindString}, Presence: registry.PresenceOptional,
		Description: "synthetic file-content setting",
		File:        &registry.FileRule{MaxBytes: 4 << 20, Check: func(content []byte) error { return nil }},
	}
}

func build(d registry.Decl) (*registry.Registry, error) {
	return registry.New(registry.Group{Spec: "T", Namespaces: []string{"t"}, Decls: []registry.Decl{d}})
}

func TestFileRuleAcceptedOnPlainString_ADR0020(t *testing.T) {
	r, err := build(pemDecl("t.pem"))
	if err != nil {
		t.Fatalf("registry rejected a File rule on a non-secret string: %v", err)
	}
	s, ok := r.Lookup("t.pem")
	if !ok || s.File == nil {
		t.Fatal("the registered setting lost its File rule")
	}
	if s.File.MaxBytes != 4<<20 {
		t.Fatalf("MaxBytes = %d", s.File.MaxBytes)
	}
	if err := s.File.Check([]byte("x")); err != nil {
		t.Fatalf("Check not preserved: %v", err)
	}
	// Settings() and Lookup return consistent File rules.
	for _, x := range r.Settings() {
		if x.Name == "t.pem" && x.File == nil {
			t.Fatal("Settings() lost the File rule")
		}
	}
}

func TestFileRuleRejectedOnSecret_ADR0020(t *testing.T) {
	d := pemDecl("t.pem")
	d.Secret = true
	if _, err := build(d); err == nil {
		t.Fatal("a File rule on a secret setting was accepted")
	}
}

func TestFileRuleRejectedOnNonStringTypes_ADR0020(t *testing.T) {
	for name, spec := range map[string]types.Spec{
		"int":      {Kind: types.KindInt},
		"bool":     {Kind: types.KindBool},
		"duration": {Kind: types.KindDuration},
		"float":    {Kind: types.KindFloat},
		"list":     {Kind: types.KindList, Elem: types.KindString},
		"enum":     {Kind: types.KindEnum, EnumValues: []string{"a", "b"}},
		"hostport": {Kind: types.KindHostPort},
	} {
		t.Run(name, func(t *testing.T) {
			d := pemDecl("t.pem")
			d.Type = spec
			if _, err := build(d); err == nil {
				t.Fatalf("a File rule on a %s setting was accepted", name)
			}
		})
	}
}

// The build error does not carry anything but the setting name.
func TestFileRuleErrorNamesTheSetting_ADR0020(t *testing.T) {
	d := pemDecl("t.pem")
	d.Secret = true
	_, err := build(d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); len(got) == 0 {
		t.Fatal("empty error text")
	}
}

// FileValue carries a path and the content read.
func TestFileValueShape_ADR0020(t *testing.T) {
	v := registry.FileValue{Path: "/synthetic/p", Content: []byte("c")}
	if v.Path != "/synthetic/p" || string(v.Content) != "c" {
		t.Fatalf("FileValue = %+v", v)
	}
}

// Settings without a File rule are unaffected.
func TestNoFileRuleByDefault_ADR0020(t *testing.T) {
	d := pemDecl("t.plain")
	d.File = nil
	r, err := build(d)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Lookup("t.plain"); s.File != nil {
		t.Fatal("File rule appeared from nowhere")
	}
}
