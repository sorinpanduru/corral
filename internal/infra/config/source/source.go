// Package source defines the injected inputs of the configuration loader
// (SPEC-003 FR-25): an environment snapshot and the file-reader contract.
//
// Compliance (SPEC-003 FR-11, FR-15): file errors are constant sentinels that
// never carry the path or the file contents.
package source

import (
	"errors"
	"strings"
)

// Size limits for files read through ReadFile.
const (
	// MaxSecretFileBytes is the FR-15 limit for a secret file (64 KiB).
	MaxSecretFileBytes int64 = 65536
	// MaxPEMFileBytes is the FR-19 limit for a PEM file (4 MiB).
	MaxPEMFileBytes int64 = 4194304
)

// Constant failure sentinels of a ReadFile. Their text carries no path.
var (
	// ErrUnreadable is a file that is missing or cannot be read.
	ErrUnreadable = errors.New("file is not readable")
	// ErrNotRegular is a path that does not resolve to a regular file.
	ErrNotRegular = errors.New("path is not a regular file")
	// ErrTooLarge is a file larger than the allowed maximum.
	ErrTooLarge = errors.New("file is too large")
)

// ReadFile reads the file at path, at most maxBytes bytes. A file longer than
// maxBytes is ErrTooLarge. Implementations return only the sentinels above
// (possibly wrapped) and never an error carrying the path.
type ReadFile func(path string, maxBytes int64) ([]byte, error)

// Env is an immutable snapshot of environment entries. The zero value is an
// empty environment.
type Env struct {
	values map[string]string
	names  []string
}

// NewEnv builds a snapshot from KEY=VALUE entries. The first occurrence of a
// key wins; entries without "=" or with an empty key are ignored.
func NewEnv(entries []string) Env {
	e := Env{values: make(map[string]string, len(entries))}
	for _, kv := range entries {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, dup := e.values[k]; dup {
			continue
		}
		e.values[k] = v
		e.names = append(e.names, k)
	}
	return e
}

// Lookup returns the value for name (case-sensitive) and whether it is
// present. An empty value is present; see IsSet for the "unset" rule.
func (e Env) Lookup(name string) (string, bool) {
	v, ok := e.values[name]
	return v, ok
}

// Names returns the distinct keys in first-occurrence order, as a copy.
func (e Env) Names() []string {
	return append([]string(nil), e.names...)
}

// IsSet reports whether v counts as set: not empty or whitespace only.
func IsSet(v string) bool { return strings.TrimSpace(v) != "" }
