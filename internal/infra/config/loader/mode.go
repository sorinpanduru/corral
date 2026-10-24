// Package loader turns command-line arguments (and, in later tasks, the
// environment and files) into validated settings for the SPEC-003 registry.
//
// Compliance (SPEC-003 FR-11, FR-14, SPEC-001 FR-24): no error carries a
// supplied flag name, value, positional argument or path. Problems are
// constant sentinels plus a record name taken from the registry (or the fixed
// names "unknown" and "argument").
package loader

// Mode is the startup mode selected by the control flags (FR-27).
type Mode int

// Modes.
const (
	ModeRun Mode = iota
	ModeHelp
	ModeVersion
	ModeCheckConfig
)

var (
	helpSpellings    = []string{"--help", "-help", "-h"}
	versionSpellings = []string{"--version", "-version"}
	checkSpellings   = []string{"--check-config", "-check-config"}
)

func isOneOf(a string, set []string) bool {
	for _, s := range set {
		if a == s {
			return true
		}
	}
	return false
}

func isControlSpelling(a string) bool {
	return isOneOf(a, helpSpellings) || isOneOf(a, versionSpellings) || isOneOf(a, checkSpellings)
}

// SelectMode scans args for exact control-flag spellings, with precedence
// help, then version, then check-config (FR-27). A spelling with "=" does not
// count. Everything else is ModeRun.
func SelectMode(args []string) Mode {
	var help, version, check bool
	for _, a := range args {
		switch {
		case isOneOf(a, helpSpellings):
			help = true
		case isOneOf(a, versionSpellings):
			version = true
		case isOneOf(a, checkSpellings):
			check = true
		}
	}
	switch {
	case help:
		return ModeHelp
	case version:
		return ModeVersion
	case check:
		return ModeCheckConfig
	}
	return ModeRun
}
