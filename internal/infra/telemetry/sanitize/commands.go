package sanitize

import "slices"

// OtherCommand is the metric label value for any command name that is not on
// the allowlist (SPEC-001 FR-12).
const OtherCommand = "other"

// commandAllowlist is the central command-name allowlist owned by SPEC-001.
// It is the single place to extend when a later spec amends SPEC-001 (D2).
var commandAllowlist = []string{
	"find", "hello",
	"getMore", "killCursors", "insert", "update", "delete", "findAndModify",
	"aggregate", "count", "distinct", "bulkWrite", "createIndexes",
	"dropIndexes", "listIndexes", "listCollections", "listDatabases", "create",
	"drop", "ping", "buildInfo", "endSessions", "commitTransaction",
	"abortTransaction", "saslStart", "saslContinue", "authenticate",
}

// commandAliases maps legacy spellings to their allowlisted name. Matching is
// case-sensitive: only the listed spellings alias.
var commandAliases = map[string]string{
	"isMaster": "hello",
	"ismaster": "hello",
}

// CommandAllowlist returns a copy of the allowlist, so callers cannot alter it.
func CommandAllowlist() []string {
	return slices.Clone(commandAllowlist)
}

// MetricLabel returns the allowlisted command name (after alias resolution)
// or OtherCommand. The input is matched exactly as received.
func MetricLabel(name string) string {
	if canonical, ok := commandAliases[name]; ok {
		name = canonical
	}
	if slices.Contains(commandAllowlist, name) {
		return name
	}
	return OtherCommand
}
