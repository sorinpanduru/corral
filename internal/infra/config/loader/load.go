package loader

import (
	"errors"
	"sort"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// Constant sentinels wrapped by the *config.Error values in Result.Errors.
// None carries a value, a path or file content.
var (
	ErrInvalidValue    = errors.New("loader: invalid value")
	ErrMissingRequired = errors.New("loader: required setting not provided")
	ErrLiteralAndFile  = errors.New("loader: literal and file reference both set")
	ErrFileUnreadable  = errors.New("loader: file not readable")
	ErrFileNotRegular  = errors.New("loader: file is not a regular file")
	ErrFileTooLarge    = errors.New("loader: file too large")
	ErrEmptyPath       = errors.New("loader: empty file path")
	ErrCrossRule       = errors.New("loader: cross-setting rule violated")
)

// Source says where an effective value came from.
type Source int

// Sources. Precedence is flag, then env, then default; nothing depends on the
// numeric order of these constants.
const (
	SourceDefault Source = iota
	SourceFlag
	SourceEnv
	SourceUnset
)

// Inputs are the injected loader inputs (FR-25): no process globals are read.
type Inputs struct {
	Args     []string
	Env      source.Env
	ReadFile source.ReadFile
}

// Value is the effective value of one setting. A failed setting keeps its
// winning Source with Set=false; a missing required setting is SourceUnset.
type Value struct {
	Set     bool
	V       any
	Source  Source
	FileRef bool
}

// Result is the outcome of Load. Values has an entry for every registered
// setting; a failed or unset setting has Set=false.
type Result struct {
	Values map[string]Value
	// Supplied lists, sorted, the settings whose value came from a flag or the
	// environment (FR-20).
	Supplied []string
	// UnknownEnv is the rendered, sorted, deduplicated FR-8 list.
	UnknownEnv []string
	// Errors are *config.Error values in FR-13 order (groups 1 to 3).
	Errors []error

	failed map[string]bool
}

// Failed reports whether the setting's winning source failed (ADR-0012).
func (r Result) Failed(name string) bool { return r.failed[name] }

// Get returns the effective value of a set setting.
func (r Result) Get(name string) (any, bool) {
	v, ok := r.Values[name]
	if !ok || !v.Set {
		return nil, false
	}
	return v.V, true
}

// srcState is the state of one source (flag or env) of one setting.
type srcState struct {
	present bool // an input exists (usable or failed)
	failed  bool
	value   any
	fileRef bool
}

type record struct {
	setting string
	err     error
}

// Load resolves every registered setting from flags, environment and
// defaults, validates every provided input, and evaluates the cross rules
// (FR-4 to FR-15, FR-28, FR-30, ADR-0012).
func Load(r *registry.Registry, in Inputs) Result {
	flagInputs, problems, failedFlag := ParseArgs(r, in.Args)

	var g1 []error
	for _, p := range problems {
		g1 = append(g1, config.NewError(p.Setting, p.Err))
	}
	byFlag := map[string][]FlagInput{}
	for _, fi := range flagInputs {
		byFlag[fi.Setting] = append(byFlag[fi.Setting], fi)
	}

	settings := r.Settings()
	var g2 []record
	res := Result{Values: make(map[string]Value, len(settings)), failed: map[string]bool{}}
	spelled := map[string]string{} // setting -> name used in a cross-rule record
	unsetOK := map[string]bool{}   // optional or default settings that may be unset
	missing := map[string]bool{}

	for _, s := range settings {
		fail := func(name string, err error) { g2 = append(g2, record{name, config.NewError(name, err)}) }

		flag := srcState{present: failedFlag[s.Name], failed: failedFlag[s.Name]}
		for _, fi := range byFlag[s.Name] {
			flag.present = true
			name := s.Name
			if fi.FileRef {
				name = s.FileFlag
			}
			v, err := evalInput(s, fi.Value, fi.FileRef, in.ReadFile)
			if err != nil {
				fail(name, err)
				flag.failed = true
				continue
			}
			flag.value, flag.fileRef = v, fi.FileRef
		}

		env := evalEnv(s, in, fail)

		var winner srcState
		src := SourceUnset
		switch {
		case flag.present:
			winner, src = flag, SourceFlag
		case env.present:
			winner, src = env, SourceEnv
		}

		if src != SourceUnset && !winner.failed && s.File != nil {
			// ADR-0020: only the winning path is read, once.
			name := s.Name
			if src == SourceEnv {
				name = s.Env
			}
			if fv, err := readFileRule(s, winner.value, in.ReadFile); err != nil {
				fail(name, err)
				winner.failed = true
			} else {
				winner.value = fv
			}
		}

		val := Value{Source: src}
		switch {
		case src != SourceUnset && winner.failed:
			res.failed[s.Name] = true
		case src != SourceUnset:
			val = Value{Set: true, V: winner.value, Source: src, FileRef: winner.fileRef}
			res.Supplied = append(res.Supplied, s.Name)
		case s.Presence == registry.PresenceDefault:
			val = Value{Set: true, V: s.DefaultValue, Source: SourceDefault}
		case s.Presence == registry.PresenceRequired:
			fail(s.Name, ErrMissingRequired)
			missing[s.Name] = true
		}
		res.Values[s.Name] = val
		spelled[s.Name] = s.Name
		if val.Set && val.Source == SourceEnv && !val.FileRef {
			spelled[s.Name] = s.Env
		}
		unsetOK[s.Name] = s.Presence == registry.PresenceOptional
	}

	sort.SliceStable(g2, func(i, j int) bool { return g2[i].setting < g2[j].setting })

	var g3 []record
	for _, rule := range r.CrossRules() {
		if skipRule(rule, res, missing, unsetOK) {
			continue
		}
		get := func(name string) (any, bool) { return res.Get(name) }
		if rule.Check(get) != nil {
			name := spelled[rule.Primary] // New guarantees the primary is registered
			g3 = append(g3, record{name, config.NewError(name, ErrCrossRule)})
		}
	}
	sort.SliceStable(g3, func(i, j int) bool { return g3[i].setting < g3[j].setting })

	res.Errors = append(res.Errors, g1...)
	for _, x := range g2 {
		res.Errors = append(res.Errors, x.err)
	}
	for _, x := range g3 {
		res.Errors = append(res.Errors, x.err)
	}
	sort.Strings(res.Supplied)
	res.UnknownEnv = unknownEnv(r, in.Env)
	return res
}

func skipRule(rule registry.CrossRule, res Result, missing, optional map[string]bool) bool {
	for _, n := range rule.Involves {
		if res.failed[n] || missing[n] {
			return true
		}
		if optional[n] && !res.Values[n].Set && !rule.AppliesToUnset {
			return true
		}
	}
	return false
}

// Note on file reads (FR-15, FR-17): each file reference is read once. A flag
// reference and an env reference to the same path are two references and are
// each read; do not merge or cache them across sources.

// evalEnv evaluates the environment source of s. Records go through fail.
func evalEnv(s registry.Setting, in Inputs, fail func(string, error)) srcState {
	var st srcState
	lit, litSet := in.Env.Lookup(s.Env)
	litSet = litSet && source.IsSet(lit)
	var file string
	var fileSet bool
	if s.Secret {
		file, fileSet = in.Env.Lookup(s.FileEnv)
		fileSet = fileSet && source.IsSet(file)
	}
	switch {
	case litSet && fileSet:
		fail(s.Env, ErrLiteralAndFile)
		return srcState{present: true, failed: true}
	case litSet:
		st.present = true
		v, err := evalInput(s, lit, false, in.ReadFile)
		if err != nil {
			fail(s.Env, err)
			st.failed = true
		}
		st.value = v
	case fileSet:
		st.present, st.fileRef = true, true
		v, err := evalInput(s, file, true, in.ReadFile)
		if err != nil {
			fail(s.FileEnv, err)
			st.failed = true
		}
		st.value = v
	}
	return st
}

// evalInput parses (and, for a file reference, reads) one input. The error is
// a constant sentinel.
func evalInput(s registry.Setting, raw string, fileRef bool, read source.ReadFile) (any, error) {
	if fileRef {
		if raw == "" {
			return nil, ErrEmptyPath
		}
		b, err := read(raw, source.MaxSecretFileBytes)
		switch {
		case err == nil:
		case errors.Is(err, source.ErrNotRegular):
			return nil, ErrFileNotRegular
		case errors.Is(err, source.ErrTooLarge):
			return nil, ErrFileTooLarge
		default:
			return nil, ErrFileUnreadable
		}
		raw = string(b)
		if strings.HasSuffix(raw, "\r\n") {
			raw = raw[:len(raw)-2]
		} else if strings.HasSuffix(raw, "\n") {
			raw = raw[:len(raw)-1]
		}
	}
	v, err := s.Type.Parse(raw)
	if err != nil {
		return nil, ErrInvalidValue
	}
	if s.Rule != nil && s.Rule(v) != nil {
		return nil, ErrInvalidValue
	}
	return v, nil
}

const envPrefix = "CORRAL_"
const invalidNameToken = "CORRAL_<invalid-name>"

// unknownEnv lists set CORRAL_ variables that are not registered names (FR-8).
// Names with a byte outside [A-Za-z0-9_] are rendered as one fixed token.
func unknownEnv(r *registry.Registry, env source.Env) []string {
	seen := map[string]bool{}
	for _, name := range env.Names() {
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		if v, _ := env.Lookup(name); !source.IsSet(v) {
			continue
		}
		if _, _, ok := r.ByEnv(name); ok {
			continue
		}
		if validEnvName(name) {
			seen[name] = true
		} else {
			seen[invalidNameToken] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func validEnvName(n string) bool {
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
