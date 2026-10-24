// Package registry holds the SPEC-003 settings declarations (FR-1, FR-2,
// FR-3, FR-14, FR-30): per-spec groups of declarations, derived flag and
// environment names, and the build-time checks that refuse an inconsistent
// declaration set.
//
// Compliance (SPEC-003 FR-11, FR-14): error text from New names settings
// (programmer constants) and problem kinds only. It never carries a default
// value or the text of a rule error.
package registry

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// Presence says whether a setting has a default, is required, or is optional.
type Presence int

// Presence values.
const (
	PresenceDefault Presence = iota + 1
	PresenceRequired
	PresenceOptional
)

// Decl declares one setting.
type Decl struct {
	Name        string
	Type        types.Spec
	Presence    Presence
	Default     string // text; only for PresenceDefault
	Secret      bool
	Description string
	Rule        func(v any) error
}

// CrossRule is a rule over several settings (FR-30). Primary names the setting
// a failure is reported under; it must be among Involves.
type CrossRule struct {
	ID             string
	Involves       []string
	Primary        string
	AppliesToUnset bool
	Check          func(get func(name string) (v any, set bool)) error
}

// Group is the declarations of one spec.
type Group struct {
	Spec       string
	Namespaces []string
	Decls      []Decl
	Rules      []CrossRule
}

// Setting is a registered declaration with its derived names.
type Setting struct {
	Decl
	Flag, Env, FileFlag, FileEnv string
	// DefaultValue is the parsed default (nil unless PresenceDefault).
	DefaultValue any
}

type ref struct {
	idx     int
	fileRef bool
}

// Registry is an immutable, validated set of settings.
type Registry struct {
	settings []Setting
	byName   map[string]int
	byFlag   map[string]ref
	byEnv    map[string]ref
	rules    []CrossRule
}

// EnvName returns the environment variable for a logical name.
func EnvName(name string) string {
	return "CORRAL_" + strings.ToUpper(strings.ReplaceAll(name, ".", "_"))
}

type derived struct{ flags, envs []string }

func derive(d Decl) derived {
	e := EnvName(d.Name)
	r := derived{flags: []string{d.Name}, envs: []string{e}}
	if d.Secret {
		r.flags = append(r.flags, d.Name+"_file")
		r.envs = append(r.envs, e+"_FILE")
	}
	return r
}

func collisions(decls []Decl) (names []string, byDerived map[string][]string) {
	count := map[string]int{}
	owners := map[string][]string{}
	for _, d := range decls {
		dv := derive(d)
		for _, f := range dv.flags {
			count[f]++
			owners[f] = append(owners[f], d.Name)
		}
		for _, e := range dv.envs {
			count[e]++
			owners[e] = append(owners[e], d.Name)
		}
	}
	byDerived = map[string][]string{}
	for n, c := range count {
		if c > 1 {
			names = append(names, n)
			byDerived[n] = owners[n]
		}
	}
	sort.Strings(names)
	return names, byDerived
}

// DerivedNameConflicts returns the derived flag and environment names (the
// <name>_file and CORRAL_<NAME>_FILE names of secrets included) claimed more
// than once, sorted and without duplicates. It does not apply the segment
// rules, so it can check the derived-name rule alone.
func DerivedNameConflicts(decls []Decl) []string {
	n, _ := collisions(decls)
	return n
}

func isNameSegment(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func validName(name string) bool {
	segs := strings.Split(name, ".")
	if len(segs) < 2 {
		return false
	}
	for _, s := range segs {
		if !isNameSegment(s) {
			return false
		}
	}
	last := segs[len(segs)-1]
	return last != "file" && !strings.HasSuffix(last, "_file")
}

func validDescription(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f || r == 0xfffd {
			return false
		}
	}
	return true
}

type problems map[string]struct{}

func (p problems) add(kind, name string) { p[kind+": "+name] = struct{}{} }

// New validates the groups and builds a Registry. On failure it returns one
// error listing every problem kind with the offending setting names, and a nil
// Registry. It never panics on malformed declarations.
func New(groups ...Group) (*Registry, error) {
	p := problems{}
	var decls []Decl
	groupOf := map[string]int{} // declaration name -> index in decls (first)
	nsOwner := map[string]string{}
	for i, g := range groups {
		if g.Spec == "" {
			p.add("group without spec", "group #"+strconv.Itoa(i+1))
		}
		for _, ns := range g.Namespaces {
			if !isNameSegment(ns) {
				p.add("invalid namespace", ns)
				continue
			}
			if _, dup := nsOwner[ns]; dup {
				p.add("namespace claimed twice", ns)
				continue
			}
			nsOwner[ns] = g.Spec
		}
		for _, d := range g.Decls {
			decls = append(decls, d)
			checkDecl(p, g, d)
			if _, dup := groupOf[d.Name]; dup {
				p.add("duplicate name", d.Name)
			} else {
				groupOf[d.Name] = len(decls) - 1
			}
		}
	}
	names, owners := collisions(decls)
	for _, n := range names {
		for _, o := range owners[n] {
			p.add("derived name collision", o)
		}
	}
	ruleIDs := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.Rules {
			checkRule(p, r, groupOf)
			if r.ID != "" && ruleIDs[r.ID] {
				p.add("duplicate rule id", r.ID)
			}
			ruleIDs[r.ID] = true
		}
	}
	if len(p) > 0 {
		lines := make([]string, 0, len(p))
		for l := range p {
			lines = append(lines, l)
		}
		sort.Strings(lines)
		return nil, errors.New("invalid settings declarations: " + strings.Join(lines, "; "))
	}
	return build(groups, decls), nil
}

func checkDecl(p problems, g Group, d Decl) {
	if !validName(d.Name) {
		p.add("invalid name", d.Name)
	}
	first, _, _ := strings.Cut(d.Name, ".")
	owned := false
	for _, ns := range g.Namespaces {
		if ns == first {
			owned = true
		}
	}
	if !owned {
		p.add("namespace not owned by group", d.Name)
	}
	if !validDescription(d.Description) {
		p.add("invalid description", d.Name)
	}
	specOK := d.Type.Check() == nil
	if !specOK {
		p.add("invalid type", d.Name)
	}
	switch d.Presence {
	case PresenceDefault:
		if d.Secret {
			p.add("secret with default", d.Name)
		}
		if specOK {
			v, err := d.Type.Parse(d.Default)
			if err != nil {
				p.add("default does not parse", d.Name)
			} else if d.Rule != nil && d.Rule(v) != nil {
				p.add("default fails rule", d.Name)
			}
		}
	case PresenceRequired, PresenceOptional:
		if d.Default != "" {
			p.add("default text without default presence", d.Name)
		}
	default:
		p.add("invalid presence", d.Name)
	}
}

func checkRule(p problems, r CrossRule, known map[string]int) {
	id := r.ID
	if id == "" {
		id = "(unnamed rule)"
		p.add("rule without id", id)
	}
	if len(r.Involves) == 0 {
		p.add("rule involves nothing", id)
	}
	primary := false
	for _, n := range r.Involves {
		if _, ok := known[n]; !ok {
			p.add("rule involves unregistered setting", id)
		}
		if n == r.Primary {
			primary = true
		}
	}
	if !primary {
		p.add("rule primary not among involved", id)
	}
	if r.Check == nil {
		p.add("rule without check", id)
	}
}

func build(groups []Group, decls []Decl) *Registry {
	r := &Registry{
		byName: map[string]int{}, byFlag: map[string]ref{}, byEnv: map[string]ref{},
	}
	for _, d := range decls {
		d.Type.EnumValues = append([]string(nil), d.Type.EnumValues...)
		s := Setting{Decl: d, Flag: d.Name, Env: EnvName(d.Name)}
		if d.Secret {
			s.FileFlag, s.FileEnv = d.Name+"_file", s.Env+"_FILE"
		}
		if d.Presence == PresenceDefault {
			s.DefaultValue, _ = d.Type.Parse(d.Default)
		}
		r.settings = append(r.settings, s)
	}
	sort.Slice(r.settings, func(i, j int) bool { return r.settings[i].Name < r.settings[j].Name })
	for i, s := range r.settings {
		r.byName[s.Name] = i
		r.byFlag[s.Flag] = ref{i, false}
		r.byEnv[s.Env] = ref{i, false}
		if s.Secret {
			r.byFlag[s.FileFlag] = ref{i, true}
			r.byEnv[s.FileEnv] = ref{i, true}
		}
	}
	for _, g := range groups {
		for _, c := range g.Rules {
			c.Involves = append([]string(nil), c.Involves...)
			r.rules = append(r.rules, c)
		}
	}
	return r
}

// Settings returns every setting sorted by name, as a copy.
func (r *Registry) Settings() []Setting {
	out := make([]Setting, len(r.settings))
	for i, s := range r.settings {
		out[i] = cloneSetting(s)
	}
	return out
}

// cloneSetting copies the slices a caller could mutate. DefaultValue lists are
// cloned too.
func cloneSetting(s Setting) Setting {
	s.Type.EnumValues = append([]string(nil), s.Type.EnumValues...)
	if s.Type.EnumValues != nil && len(s.Type.EnumValues) == 0 {
		s.Type.EnumValues = nil
	}
	switch v := s.DefaultValue.(type) {
	case []string:
		s.DefaultValue = append([]string{}, v...)
	case []int64:
		s.DefaultValue = append([]int64{}, v...)
	case []bool:
		s.DefaultValue = append([]bool{}, v...)
	case []float64:
		s.DefaultValue = append([]float64{}, v...)
	case []time.Duration:
		s.DefaultValue = append([]time.Duration{}, v...)
	}
	return s
}

// Lookup finds a setting by logical name.
func (r *Registry) Lookup(name string) (Setting, bool) {
	i, ok := r.byName[name]
	if !ok {
		return Setting{}, false
	}
	return cloneSetting(r.settings[i]), true
}

// ByFlag finds a setting by flag name (without dashes). fileRef reports that
// the name is the secret's <name>_file flag.
func (r *Registry) ByFlag(flag string) (s Setting, fileRef bool, ok bool) {
	e, ok := r.byFlag[flag]
	if !ok {
		return Setting{}, false, false
	}
	return cloneSetting(r.settings[e.idx]), e.fileRef, true
}

// ByEnv finds a setting by environment variable name; fileRef reports the
// secret's <ENV>_FILE variable.
func (r *Registry) ByEnv(env string) (s Setting, fileRef bool, ok bool) {
	e, ok := r.byEnv[env]
	if !ok {
		return Setting{}, false, false
	}
	return cloneSetting(r.settings[e.idx]), e.fileRef, true
}

// CrossRules returns the cross rules in declaration order, as a copy.
func (r *Registry) CrossRules() []CrossRule {
	out := make([]CrossRule, len(r.rules))
	for i, c := range r.rules {
		c.Involves = append([]string(nil), c.Involves...)
		out[i] = c
	}
	return out
}
