// Package types implements the SPEC-003 FR-12 setting type set: one parser per
// type with an exact accepted syntax.
//
// Compliance (SPEC-003 FR-11, FR-13, SPEC-001 FR-24, GDPR data minimisation):
// every error is a constant sentinel. No error carries the input text, a part
// of it, or its length, because a rejected value may be a secret.
package types

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Constant failure sentinels. A rejected value yields exactly one of these
// (FR-13), the first failing check of its type.
var (
	ErrInvalidUTF8 = errors.New("value is not valid UTF-8")
	ErrControlChar = errors.New("value contains a control character")
	ErrEmpty       = errors.New("value is empty")
	ErrSyntax      = errors.New("value has invalid syntax")
	ErrRange       = errors.New("value is out of range")
	ErrNotInSet    = errors.New("value is not in the allowed set")

	// ErrSpec reports a malformed Spec (a programmer error, never input).
	ErrSpec = errors.New("malformed type specification")
)

// Kind identifies a FR-12 type. The zero Kind is invalid.
type Kind int

// The FR-12 type set.
const (
	KindString Kind = iota + 1
	KindText
	KindBool
	KindInt
	KindFloat
	KindDuration
	KindEnum
	KindHostPort
	KindList
)

var kindNames = map[Kind]string{
	KindString: "string", KindText: "text", KindBool: "bool", KindInt: "int",
	KindFloat: "float", KindDuration: "duration", KindEnum: "enum",
	KindHostPort: "hostport", KindList: "list",
}

// String returns the FR-12 type name.
func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return "invalid"
}

// Spec describes one setting type.
type Spec struct {
	Kind Kind
	// Elem is the element kind of a list; it must be zero otherwise.
	Elem Kind
	// EnumValues is the declared set of an enum (or list of enum).
	EnumValues        []string
	EnumCaseSensitive bool
	// AllowEmpty permits the empty value for string, text and list.
	AllowEmpty bool
	// AllowZero permits a zero duration (or zero list elements).
	AllowZero bool
}

func isScalar(k Kind) bool {
	switch k {
	case KindString, KindBool, KindInt, KindFloat, KindDuration, KindEnum, KindHostPort:
		return true
	}
	return false
}

// Check reports whether the Spec itself is well-formed.
func (s Spec) Check() error {
	kind := s.Kind
	if kind == KindList {
		if !isScalar(s.Elem) {
			return ErrSpec
		}
		kind = s.Elem
	} else {
		if s.Elem != 0 || !isScalar(kind) && kind != KindText {
			return ErrSpec
		}
	}
	if s.AllowEmpty && s.Kind != KindString && s.Kind != KindText && s.Kind != KindList {
		return ErrSpec
	}
	if s.AllowZero && kind != KindDuration {
		return ErrSpec
	}
	if kind == KindEnum {
		return s.checkEnum()
	}
	if len(s.EnumValues) != 0 || s.EnumCaseSensitive {
		return ErrSpec
	}
	return nil
}

func (s Spec) checkEnum() error {
	if len(s.EnumValues) == 0 {
		return ErrSpec
	}
	for i, a := range s.EnumValues {
		if a == "" {
			return ErrSpec
		}
		for _, b := range s.EnumValues[i+1:] {
			if a == b || !s.EnumCaseSensitive && asciiEqualFold(a, b) {
				return ErrSpec
			}
		}
	}
	return nil
}

// Describe returns the type text used in help, for example
// "enum (json, text)" or "list of duration".
func (s Spec) Describe() string {
	switch s.Kind {
	case KindEnum:
		return "enum (" + strings.Join(s.EnumValues, ", ") + ")"
	case KindList:
		return "list of " + s.elemSpec().Describe()
	}
	return s.Kind.String()
}

func (s Spec) elemSpec() Spec {
	return Spec{
		Kind: s.Elem, EnumValues: s.EnumValues, EnumCaseSensitive: s.EnumCaseSensitive,
		AllowZero: s.AllowZero,
	}
}

// Parse validates raw and returns its typed value: string (string, text, enum,
// hostport), bool, int64, float64, time.Duration, or a slice of those for a
// list. The error is a constant sentinel.
func (s Spec) Parse(raw string) (any, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(raw) {
		return nil, ErrInvalidUTF8
	}
	return s.parse(raw)
}

func (s Spec) parse(raw string) (any, error) {
	switch s.Kind {
	case KindString:
		return parseString(raw, s.AllowEmpty, false)
	case KindText:
		return parseString(raw, s.AllowEmpty, true)
	case KindBool:
		return parseBool(raw)
	case KindInt:
		return parseInt(raw)
	case KindFloat:
		return parseFloat(raw)
	case KindDuration:
		return parseDuration(raw, s.AllowZero)
	case KindEnum:
		return parseEnum(raw, s.EnumValues, s.EnumCaseSensitive)
	case KindHostPort:
		return parseHostPort(raw)
	case KindList:
		return s.parseList(raw)
	}
	return nil, ErrSpec
}

func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// asciiEqualFold folds ASCII letters only, so U+212A (Kelvin) and U+0131
// never match k and i.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
