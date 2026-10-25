// Package sanitize bounds and cleans client-supplied strings before they reach
// logs, spans or metric labels (SPEC-001 FR-12, FR-25).
package sanitize

import (
	"strings"
	"unicode/utf8"
)

const (
	maxCommandLen       = 64
	maxAppNameLen       = 128
	maxDriverNameLen    = 64
	maxDriverVersionLen = 64
)

// CommandForTelemetry returns the command name as received (no alias, no
// allowlist) with invalid UTF-8 replaced and length limited to 64 bytes.
func CommandForTelemetry(name string) string { return clean(name, maxCommandLen) }

// AppName sanitises the client application name (<= 128 bytes).
func AppName(s string) string { return clean(s, maxAppNameLen) }

// DriverName sanitises the client driver name (<= 64 bytes).
func DriverName(s string) string { return clean(s, maxDriverNameLen) }

// DriverVersion sanitises the client driver version (<= 64 bytes).
func DriverVersion(s string) string { return clean(s, maxDriverVersionLen) }

// clean replaces each invalid UTF-8 byte with U+FFFD, then truncates to at
// most limit bytes on a rune boundary. Replacement comes first, so U+FFFD
// counts toward the limit. Work and allocation are bounded by limit, not by
// the input length, so oversized client input cannot be used for resource
// exhaustion.
func clean(s string, limit int) string {
	// Every input byte yields at least one output byte, so output offset >=
	// input offset. Only the first limit+UTFMax input bytes can influence the
	// kept output; a rune cut by this prefix starts beyond limit and is dropped.
	if len(s) > limit+utf8.UTFMax {
		s = s[:limit+utf8.UTFMax]
	}
	if len(s) <= limit && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(limit + utf8.UTFMax)
	for _, r := range s { // yields RuneError once per invalid byte
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CollectionForTelemetry returns a collection name for logs and spans: each
// invalid UTF-8 byte is replaced by U+FFFD, and the name is not truncated
// (SPEC-001 FR-40). Collection names never become metric labels.
func CollectionForTelemetry(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*utf8.UTFMax)
	for _, r := range s { // RuneError once per invalid byte
		b.WriteRune(r)
	}
	return b.String()
}
