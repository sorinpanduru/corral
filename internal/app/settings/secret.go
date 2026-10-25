package settings

import (
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Secret holds a secret setting value. It prints as [redacted] in every
// formatting path (String, GoString, any fmt verb, slog and JSON); only Bytes
// returns the value, for the code that needs it (SPEC-002 FR-24).
type Secret struct{ b []byte }

// NewSecret wraps a value.
func NewSecret(b []byte) Secret { return Secret{b: b} }

// Bytes returns the secret value.
func (s Secret) Bytes() []byte { return s.b }

// String returns [redacted].
func (Secret) String() string { return redacted }

// GoString returns [redacted].
func (Secret) GoString() string { return redacted }

// Format writes [redacted] for every verb.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue returns [redacted] for slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText returns [redacted] for encoders.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON returns "[redacted]".
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
