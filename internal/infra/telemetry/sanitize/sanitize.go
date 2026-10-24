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
// counts toward the limit.
func clean(s string, limit int) string {
	if !utf8.ValidString(s) {
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); {
			r, n := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && n == 1 {
				b.WriteRune(utf8.RuneError)
			} else {
				b.WriteString(s[i : i+n])
			}
			i += n
		}
		s = b.String()
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
