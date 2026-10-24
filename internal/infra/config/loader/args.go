package loader

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

// Constant command-line problem sentinels.
var (
	ErrUnknownFlag         = errors.New("loader: unknown or malformed flag")
	ErrPositional          = errors.New("loader: unexpected positional argument")
	ErrMissingValue        = errors.New("loader: flag has no value")
	ErrRepeated            = errors.New("loader: flag given more than once")
	ErrSecretOnCommandLine = errors.New("loader: secret value on the command line")
	ErrControlFlagValue    = errors.New("loader: control flag takes no value")
)

// FlagInput is a valid single flag occurrence, for later validation.
type FlagInput struct {
	Pos     int    // 0-based index into the arguments
	Setting string // logical setting name
	FileRef bool   // the <name>_file flag
	Value   string
}

// ArgProblem is one group 1 error (FR-13).
type ArgProblem struct {
	Pos     int
	Setting string // record name: a setting, the file flag name, "unknown" or "argument"
	Err     error
}

type occurrence struct {
	pos   int
	hasEq bool
	value string
}

type flagKey struct {
	setting string
	fileRef bool
}

func hasControlOrInvalid(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f {
			return true
		}
	}
	return false
}

// ParseArgs parses args against r (FR-6, FR-27, FR-28). It returns the valid
// single occurrences in argument order, the problems in argument order (at
// most one per argument and one per flag name) and the set of settings whose
// flag source failed. Control spellings are skipped; nothing here validates
// values.
func ParseArgs(r *registry.Registry, args []string) (inputs []FlagInput, problems []ArgProblem, failedFlag map[string]bool) {
	failedFlag = map[string]bool{}
	occs := map[flagKey][]occurrence{}
	var order []flagKey
	addProblem := func(pos int, setting string, err error) {
		problems = append(problems, ArgProblem{Pos: pos, Setting: setting, Err: err})
	}

	for pos, a := range args {
		if !strings.HasPrefix(a, "-") {
			addProblem(pos, config.SettingArgument, ErrPositional)
			continue
		}
		head, value, hasEq := strings.Cut(a, "=")
		if isControlSpelling(a) {
			continue
		}
		if hasEq && isControlSpelling(head) {
			addProblem(pos, config.SettingUnknown, ErrControlFlagValue)
			continue
		}
		name := strings.TrimPrefix(head, "-")
		if strings.HasPrefix(head, "--") {
			name = head[2:]
		}
		if name == "" || hasControlOrInvalid(name) {
			addProblem(pos, config.SettingUnknown, ErrUnknownFlag)
			continue
		}
		s, fileRef, ok := r.ByFlag(name)
		if !ok {
			addProblem(pos, config.SettingUnknown, ErrUnknownFlag)
			continue
		}
		k := flagKey{s.Name, fileRef}
		if _, seen := occs[k]; !seen {
			order = append(order, k)
		}
		occs[k] = append(occs[k], occurrence{pos, hasEq, value})
	}

	valid := map[flagKey]occurrence{}
	for _, k := range order {
		oc := occs[k]
		s, _ := r.Lookup(k.setting)
		recName := s.Name
		if k.fileRef {
			recName = s.FileFlag
		}
		secretLiteral := s.Secret && !k.fileRef
		switch {
		case secretLiteral:
			if first := firstWithEq(oc); first >= 0 {
				addProblem(oc[first].pos, recName, ErrSecretOnCommandLine)
				failedFlag[s.Name] = true
				continue
			}
			fallthrough
		default:
			switch {
			case len(oc) >= 2:
				addProblem(oc[1].pos, recName, ErrRepeated)
				failedFlag[s.Name] = true
			case !oc[0].hasEq:
				addProblem(oc[0].pos, recName, ErrMissingValue)
				failedFlag[s.Name] = true
			case !secretLiteral:
				valid[k] = oc[0]
			}
		}
	}

	for k, oc := range valid {
		if failedFlag[k.setting] {
			continue
		}
		inputs = append(inputs, FlagInput{Pos: oc.pos, Setting: k.setting, FileRef: k.fileRef, Value: oc.value})
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Pos < inputs[j].Pos })
	sort.Slice(problems, func(i, j int) bool { return problems[i].Pos < problems[j].Pos })
	return inputs, problems, failedFlag
}

func firstWithEq(oc []occurrence) int {
	for i, o := range oc {
		if o.hasEq {
			return i
		}
	}
	return -1
}
