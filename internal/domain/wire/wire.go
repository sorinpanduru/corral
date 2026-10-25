// Package wire is the domain model of the MongoDB wire protocol as corral
// needs it (ADR-0017): headers and opcode checks, streaming OP_MSG
// validation, request classification from a bounded prefix, header rewriting,
// request-ID correlation and builders for corral's own messages.
//
// It imports the standard library and other domain packages only. It never
// does I/O.
//
// Compliance (SPEC-002 FR-9, FR-24; GDPR data minimisation): message bytes are
// never kept beyond what a result type lists, and never appear in errors.
// Request, Handshake and ReplyFields hold only the fields FR-9 allows.
package wire

import "encoding/binary"

// OpCode is the signed 32-bit wire opcode.
type OpCode int32

// Opcodes.
const (
	OpReply       OpCode = 1
	OpUpdate      OpCode = 2001
	OpInsert      OpCode = 2002
	OpReserved    OpCode = 2003
	OpQuery       OpCode = 2004
	OpGetMore     OpCode = 2005
	OpDelete      OpCode = 2006
	OpKillCursors OpCode = 2007
	OpCompressed  OpCode = 2012
	OpMsg         OpCode = 2013
)

// OP_MSG flag bits.
const (
	FlagChecksumPresent uint32 = 1 << 0
	FlagMoreToCome      uint32 = 1 << 1
	FlagExhaustAllowed  uint32 = 1 << 16
)

const (
	// DefaultMaxMessageSize is the maximum when the upstream reports none.
	DefaultMaxMessageSize = 48_000_000

	headerLen       = 16
	minOpMsgLen     = 26
	minOpQueryLen   = 34
	minOtherOpLen   = 16
	requiredFlagMsk = 0xFFFC // bits 2 to 15 must be zero
)

// Header is the 16-byte message header. All fields are signed little endian.
type Header struct {
	Length     int32
	RequestID  int32
	ResponseTo int32
	OpCode     OpCode
}

// ParseHeader reads a header from the first 16 bytes of b. b must hold at
// least 16 bytes.
func ParseHeader(b []byte) Header {
	return Header{
		Length:     int32(binary.LittleEndian.Uint32(b)),
		RequestID:  int32(binary.LittleEndian.Uint32(b[4:])),
		ResponseTo: int32(binary.LittleEndian.Uint32(b[8:])),
		OpCode:     OpCode(int32(binary.LittleEndian.Uint32(b[12:]))),
	}
}

// AppendTo appends the 16 header bytes to dst.
func (h Header) AppendTo(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.Length))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.RequestID))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.ResponseTo))
	return binary.LittleEndian.AppendUint32(dst, uint32(int32(h.OpCode)))
}

// Error details (ADR-0016 tokens; the infrastructure maps them to classes).
const (
	detailMalformed  = "malformed_message"
	detailTooLarge   = "message_too_large"
	detailUnsupOp    = "unsupported_opcode"
	detailChecksum   = "checksum_mismatch"
	detailUnexpected = "unexpected_response"
)

// ProtocolError is a client or upstream protocol violation. Its text is
// constant: it never carries message bytes.
type ProtocolError struct{ detail string }

// Detail returns the ADR-0016 detail token.
func (e *ProtocolError) Detail() string { return e.detail }

// CorralErrorDetail implements the errclass.DetailedError contract.
func (e *ProtocolError) CorralErrorDetail() string { return e.detail }

func (e *ProtocolError) Error() string { return "wire: protocol error: " + e.detail }

// Is makes errors.Is(err, wire.ErrMalformed()) match on the detail.
func (e *ProtocolError) Is(target error) bool {
	t, ok := target.(*ProtocolError)
	return ok && t.detail == e.detail
}

// Error constructors.
func ErrUnsupportedOpcode() *ProtocolError  { return &ProtocolError{detailUnsupOp} }
func ErrMessageTooLarge() *ProtocolError    { return &ProtocolError{detailTooLarge} }
func ErrMalformed() *ProtocolError          { return &ProtocolError{detailMalformed} }
func ErrChecksum() *ProtocolError           { return &ProtocolError{detailChecksum} }
func ErrUnexpectedResponse() *ProtocolError { return &ProtocolError{detailUnexpected} }

// CheckClientHeader applies the FR-5 header checks in their fixed order:
// length below 16 (or negative), length above maxLen, opcode not accepted,
// length below the opcode's minimum. maxLen <= 0 means DefaultMaxMessageSize.
func CheckClientHeader(h Header, maxLen int32) error {
	if maxLen <= 0 {
		maxLen = DefaultMaxMessageSize
	}
	if h.Length < headerLen {
		return ErrMalformed()
	}
	if h.Length > maxLen {
		return ErrMessageTooLarge()
	}
	var min int32
	switch h.OpCode {
	case OpMsg:
		min = minOpMsgLen
	case OpQuery:
		min = minOpQueryLen
	default:
		return ErrUnsupportedOpcode()
	}
	if h.Length < min {
		return ErrMalformed()
	}
	return nil
}

// CheckReplyHeader applies the FR-58 checks to an upstream reply header: the
// same length bounds, and the opcode must be OP_MSG.
func CheckReplyHeader(h Header, maxLen int32) error {
	if maxLen <= 0 {
		maxLen = DefaultMaxMessageSize
	}
	if h.Length < headerLen {
		return ErrMalformed()
	}
	if h.Length > maxLen {
		return ErrMessageTooLarge()
	}
	if h.OpCode != OpMsg {
		// FR-58: unexpected_response is for moreToCome and unmatched
		// responseTo; any other opcode is a malformed header.
		return ErrMalformed()
	}
	if h.Length < minOpMsgLen {
		return ErrMalformed()
	}
	return nil
}
