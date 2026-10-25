package wire_test

import (
	"math"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func TestOpcodeConstants_FR6(t *testing.T) {
	for op, want := range map[wire.OpCode]int32{wire.OpMsg: 2013, wire.OpQuery: 2004, wire.OpReply: 1, wire.OpCompressed: 2012} {
		if int32(op) != want {
			t.Errorf("opcode constant = %d, want %d", op, want)
		}
	}
	if wire.DefaultMaxMessageSize != 48_000_000 {
		t.Fatalf("DefaultMaxMessageSize = %d", wire.DefaultMaxMessageSize)
	}
}

// FR-5: the header is little-endian with signed length and opcode; AppendTo
// round-trips.
func TestHeaderRoundTrip_FR5(t *testing.T) {
	for _, h := range []wire.Header{
		{Length: 26, RequestID: 1, ResponseTo: 2, OpCode: wire.OpMsg},
		{Length: -1, RequestID: -7, ResponseTo: math.MinInt32, OpCode: wire.OpCode(-5)},
		{Length: math.MaxInt32, RequestID: math.MaxInt32, ResponseTo: 0, OpCode: wire.OpQuery},
	} {
		b := h.AppendTo(nil)
		if len(b) != 16 {
			t.Fatalf("AppendTo wrote %d bytes", len(b))
		}
		if got := wire.ParseHeader(b); got != h {
			t.Fatalf("round trip: %+v != %+v", got, h)
		}
	}
	// Appends to an existing slice.
	pre := []byte{9, 9}
	out := (wire.Header{Length: 16}).AppendTo(pre)
	if len(out) != 18 || out[0] != 9 || out[1] != 9 {
		t.Fatal("AppendTo did not append")
	}
	// Little-endian layout.
	b := hdrBytes(0x01020304, 5, 6, 2013)
	h := wire.ParseHeader(b)
	if h.Length != 0x01020304 || h.RequestID != 5 || h.ResponseTo != 6 || h.OpCode != wire.OpMsg {
		t.Fatalf("ParseHeader = %+v", h)
	}
	if h := wire.ParseHeader(hdrBytes(-1, 0, 0, -2)); h.Length != -1 || h.OpCode != wire.OpCode(-2) {
		t.Fatalf("signed fields lost: %+v", h)
	}
}

const max = wire.DefaultMaxMessageSize

// AC-4, AC-37, FR-5/FR-6: client header checks. Exact details per case.
func TestCheckClientHeader_AC37(t *testing.T) {
	cases := []struct {
		name string
		h    wire.Header
		max  int32
		want string
	}{
		// Minimum lengths: accepted at the minimum, malformed below it.
		{"OP_MSG at 26", wire.Header{Length: 26, OpCode: wire.OpMsg}, max, ""},
		{"OP_MSG at 25", wire.Header{Length: 25, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"OP_MSG at 16", wire.Header{Length: 16, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"OP_QUERY at 34", wire.Header{Length: 34, OpCode: wire.OpQuery}, max, ""},
		{"OP_QUERY at 33", wire.Header{Length: 33, OpCode: wire.OpQuery}, max, "malformed_message"},
		{"OP_QUERY at 26", wire.Header{Length: 26, OpCode: wire.OpQuery}, max, "malformed_message"},
		// Below 16, negative: malformed whatever the opcode.
		{"length -1", wire.Header{Length: -1, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"length 0", wire.Header{Length: 0, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"length 15", wire.Header{Length: 15, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"length INT32_MIN", wire.Header{Length: math.MinInt32, OpCode: wire.OpMsg}, max, "malformed_message"},
		{"length 15 with OP_COMPRESSED", wire.Header{Length: 15, OpCode: wire.OpCompressed}, max, "malformed_message"},
		{"length 15 with unknown opcode", wire.Header{Length: 15, OpCode: 9999}, max, "malformed_message"},
		// Too large.
		{"length at max", wire.Header{Length: max, OpCode: wire.OpMsg}, max, ""},
		{"length max+1", wire.Header{Length: max + 1, OpCode: wire.OpMsg}, max, "message_too_large"},
		{"length INT32_MAX", wire.Header{Length: math.MaxInt32, OpCode: wire.OpMsg}, max, "message_too_large"},
		{"custom max", wire.Header{Length: 1001, OpCode: wire.OpMsg}, 1000, "message_too_large"},
		{"custom max boundary", wire.Header{Length: 1000, OpCode: wire.OpMsg}, 1000, ""},
		// Opcodes.
		{"OP_COMPRESSED", wire.Header{Length: 100, OpCode: wire.OpCompressed}, max, "unsupported_opcode"},
		{"OP_REPLY", wire.Header{Length: 100, OpCode: wire.OpReply}, max, "unsupported_opcode"},
		{"OP_UPDATE", wire.Header{Length: 100, OpCode: 2001}, max, "unsupported_opcode"},
		{"OP_INSERT", wire.Header{Length: 100, OpCode: 2002}, max, "unsupported_opcode"},
		{"reserved 2003", wire.Header{Length: 100, OpCode: 2003}, max, "unsupported_opcode"},
		{"OP_GET_MORE", wire.Header{Length: 100, OpCode: 2005}, max, "unsupported_opcode"},
		{"OP_DELETE", wire.Header{Length: 100, OpCode: 2006}, max, "unsupported_opcode"},
		{"OP_KILL_CURSORS", wire.Header{Length: 100, OpCode: 2007}, max, "unsupported_opcode"},
		{"unknown opcode", wire.Header{Length: 100, OpCode: 4242}, max, "unsupported_opcode"},
		{"negative opcode", wire.Header{Length: 100, OpCode: -1}, max, "unsupported_opcode"},
		{"opcode 0", wire.Header{Length: 100, OpCode: 0}, max, "unsupported_opcode"},
		{"legacy opcode at exactly 16", wire.Header{Length: 16, OpCode: 2001}, max, "unsupported_opcode"},
		// Order of checks: the first failing check decides.
		{"unknown opcode with length 10", wire.Header{Length: 10, OpCode: 4242}, max, "malformed_message"},
		{"unknown opcode with 60,000,000", wire.Header{Length: 60_000_000, OpCode: 4242}, max, "message_too_large"},
		{"OP_COMPRESSED with 60,000,000", wire.Header{Length: 60_000_000, OpCode: wire.OpCompressed}, max, "message_too_large"},
		{"OP_MSG 20 and over max-limit custom", wire.Header{Length: 20, OpCode: wire.OpMsg}, 10, "message_too_large"},
		{"negative beats too large with tiny max", wire.Header{Length: -5, OpCode: wire.OpMsg}, 1, "malformed_message"},
		// "GET " read as a length: 542,393,671 fails check 2 (FR-5, AC-34).
		{"HTTP GET", wire.ParseHeader([]byte("GET / HTTP/1.1\r\n")), max, "message_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := wire.CheckClientHeader(tc.h, tc.max)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("rejected: %v (%q)", err, detail(err))
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want %q", tc.want)
			}
			if got := detail(err); got != tc.want {
				t.Fatalf("detail %q, want %q", got, tc.want)
			}
		})
	}
}

// ProtocolError is an error with Detail() and CorralErrorDetail() agreeing.
func TestProtocolErrorDetails_ADR0016(t *testing.T) {
	err := wire.CheckClientHeader(wire.Header{Length: 5, OpCode: wire.OpMsg}, max)
	var pe protoErr
	if !asProtocolError(err, &pe) {
		t.Fatalf("error %T is not a wire.ProtocolError", err)
	}
	if pe.Detail() != "malformed_message" || pe.CorralErrorDetail() != "malformed_message" {
		t.Fatalf("Detail=%q CorralErrorDetail=%q", pe.Detail(), pe.CorralErrorDetail())
	}
	if pe.Error() == "" {
		t.Fatal("empty error text")
	}
}

// Reply headers (FR-58): length bounds and opcode.
func TestCheckReplyHeader_FR58(t *testing.T) {
	ok := wire.Header{Length: 100, OpCode: wire.OpMsg}
	if err := wire.CheckReplyHeader(ok, max); err != nil {
		t.Fatalf("valid reply header rejected: %v", err)
	}
	if err := wire.CheckReplyHeader(wire.Header{Length: max + 1, OpCode: wire.OpMsg}, max); detail(err) != "message_too_large" {
		t.Fatalf("too large: %q", detail(err))
	}
	for _, l := range []int32{-1, 0, 15, 25} {
		if err := wire.CheckReplyHeader(wire.Header{Length: l, OpCode: wire.OpMsg}, max); detail(err) != "malformed_message" {
			t.Fatalf("length %d: %q", l, detail(err))
		}
	}
	if err := wire.CheckReplyHeader(wire.Header{Length: 100, OpCode: wire.OpQuery}, max); err == nil {
		t.Fatal("a non-OP_MSG upstream reply was accepted")
	}
}
