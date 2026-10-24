// Package bson is the domain BSON reader, streaming validator and builder
// (ADR-0017). It imports the standard library only and uses no recursion:
// nesting is tracked on an explicit stack capped at MaxDepth levels.
//
// Compliance (SPEC-002 FR-9, FR-24): errors are constant sentinels and never
// carry input bytes. Captures keep only bounded scalar copies (strings up to
// MaxCaptureBytes bytes) and nothing else of the message is retained.
package bson

import "errors"

// Type is a BSON element type byte.
type Type byte

// Element types accepted by the validator (ADR-0017), including the
// deprecated 0x06, 0x0C and 0x0E.
const (
	TypeDouble     Type = 0x01
	TypeString     Type = 0x02
	TypeDocument   Type = 0x03
	TypeArray      Type = 0x04
	TypeBinary     Type = 0x05
	TypeUndefined  Type = 0x06
	TypeObjectID   Type = 0x07
	TypeBool       Type = 0x08
	TypeDateTime   Type = 0x09
	TypeNull       Type = 0x0A
	TypeRegex      Type = 0x0B
	TypeDBPointer  Type = 0x0C
	TypeJavaScript Type = 0x0D
	TypeSymbol     Type = 0x0E
	TypeCodeScope  Type = 0x0F
	TypeInt32      Type = 0x10
	TypeTimestamp  Type = 0x11
	TypeInt64      Type = 0x12
	TypeDecimal128 Type = 0x13
	TypeMaxKey     Type = 0x7F
	TypeMinKey     Type = 0xFF
)

const (
	// MaxDepth is the deepest nesting accepted, counting the top-level
	// document as level 1 (ADR-0017).
	MaxDepth = 1024
	// MaxCaptureBytes bounds the bytes a capture keeps (strings, binary
	// data, first keys). Longer values are reported as TooLong.
	MaxCaptureBytes = 256
)

var (
	// ErrMalformed reports a document that breaks an ADR-0017 rule.
	ErrMalformed = errors.New("bson: malformed document")
	// ErrTooDeep reports nesting deeper than MaxDepth.
	ErrTooDeep = errors.New("bson: document nested too deeply")
)

func validType(t Type) bool {
	return (t >= 0x01 && t <= 0x13) || t == TypeMaxKey || t == TypeMinKey
}

// fixedSize returns the payload size of fixed-size types.
func fixedSize(t Type) (int, bool) {
	switch t {
	case TypeDouble, TypeDateTime, TypeTimestamp, TypeInt64:
		return 8, true
	case TypeObjectID:
		return 12, true
	case TypeBool:
		return 1, true
	case TypeInt32:
		return 4, true
	case TypeDecimal128:
		return 16, true
	case TypeUndefined, TypeNull, TypeMaxKey, TypeMinKey:
		return 0, true
	}
	return 0, false
}

// Validate checks that doc is exactly one valid BSON document. It runs the
// same state machine as the Walker, so the two always agree.
func Validate(doc []byte) error {
	w := NewWalker(nil)
	if err := w.Write(doc); err != nil {
		return err
	}
	return w.Close()
}
