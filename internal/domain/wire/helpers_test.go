package wire_test

// Independent, stdlib-only encoders for synthetic wire messages. They share no
// code with internal/domain/wire or internal/domain/bson, so the tests do not
// depend on the code under test to build their inputs. All data is synthetic.
//
// Interface assumptions (also stated in the test-writer report):
//   - wire.ClassifyPrefix(prefix, total): prefix includes the 16-byte header.
//   - wire.NewMsgValidator(h, isReply).Write(p): p holds the bytes after the
//     16-byte header (flagBits onward); the header comes from h.
//   - Errors are inspected through CorralErrorDetail() (ADR-0016), so tests do
//     not depend on how the Err* constructors are spelled.

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/wire"
)

func le32(v int32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(v))
	return b
}
func le64(v int64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))
	return b
}
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
func cstr(s string) []byte { return append([]byte(s), 0) }

func doc(elems ...[]byte) []byte {
	body := cat(elems...)
	return cat(le32(int32(len(body)+5)), body, []byte{0})
}
func el(typ byte, key string, payload []byte) []byte {
	return cat([]byte{typ}, cstr(key), payload)
}
func strPayload(s string) []byte { return cat(le32(int32(len(s)+1)), []byte(s), []byte{0}) }

func eDouble(k string, v float64) []byte { return el(0x01, k, le64(int64(math.Float64bits(v)))) }
func eString(k, v string) []byte         { return el(0x02, k, strPayload(v)) }
func eDoc(k string, d []byte) []byte     { return el(0x03, k, d) }
func eArray(k string, items ...[]byte) []byte {
	return el(0x04, k, doc(items...))
}
func eBinary(k string, sub byte, data []byte) []byte {
	return el(0x05, k, cat(le32(int32(len(data))), []byte{sub}, data))
}
func eBool(k string, v bool) []byte {
	b := byte(0)
	if v {
		b = 1
	}
	return el(0x08, k, []byte{b})
}
func eNull(k string) []byte           { return el(0x0A, k, nil) }
func eInt32(k string, v int32) []byte { return el(0x10, k, le32(v)) }
func eInt64(k string, v int64) []byte { return el(0x12, k, le64(v)) }

// nested builds a document nested n levels (top-level = level 1) in one linear
// pass, with documents ("d") or arrays ("0") as the container.
func nested(n int, arrays bool) []byte {
	leaf := doc(eInt32("leaf", 1))
	if n <= 1 {
		return leaf
	}
	prefix := cat([]byte{0x03}, cstr("d"))
	if arrays {
		prefix = cat([]byte{0x04}, cstr("0"))
	}
	wrapper := 4 + len(prefix) + 1
	out := make([]byte, 0, len(leaf)+(n-1)*wrapper)
	for i := 1; i < n; i++ {
		out = append(out, le32(int32(len(leaf)+(n-i)*wrapper))...)
		out = append(out, prefix...)
	}
	out = append(out, leaf...)
	for i := 1; i < n; i++ {
		out = append(out, 0)
	}
	return out
}

// sec0 and sec1 build OP_MSG sections.
func sec0(d []byte) []byte { return cat([]byte{0}, d) }
func sec1(id string, docs ...[]byte) []byte {
	body := cat(cstr(id), cat(docs...))
	return cat([]byte{1}, le32(int32(len(body)+4)), body)
}

const (
	flagChecksum = 1 << 0
	flagMore     = 1 << 1
	flagExhaust  = 1 << 16
)

func hdrBytes(length, reqID, respTo, op int32) []byte {
	return cat(le32(length), le32(reqID), le32(respTo), le32(op))
}

// opMsg builds a complete OP_MSG: header, flags, sections. A checksum is not
// added; use withCRC.
func opMsg(reqID int32, flags uint32, sections ...[]byte) []byte {
	body := cat(le32(int32(flags)), cat(sections...))
	return cat(hdrBytes(int32(16+len(body)), reqID, 0, 2013), body)
}

// withCRC sets checksumPresent, grows the length by 4 and appends the CRC-32C
// of everything before it. bad flips one bit of the checksum.
func withCRC(msg []byte, bad bool) []byte {
	m := append([]byte(nil), msg...)
	flags := binary.LittleEndian.Uint32(m[16:]) | flagChecksum
	binary.LittleEndian.PutUint32(m[16:], flags)
	binary.LittleEndian.PutUint32(m, uint32(len(m)+4))
	sum := crc32.Checksum(m, crc32.MakeTable(crc32.Castagnoli))
	if bad {
		sum ^= 1
	}
	return append(m, le32(int32(sum))...)
}

// cmdMsg is the common shape: one kind-0 body.
func cmdMsg(elems ...[]byte) []byte { return opMsg(1, 0, sec0(doc(elems...))) }

func opQuery(reqID int32, flags uint32, ns string, skip, ret int32, docs ...[]byte) []byte {
	body := cat(le32(int32(flags)), cstr(ns), le32(skip), le32(ret), cat(docs...))
	return cat(hdrBytes(int32(16+len(body)), reqID, 0, 2004), body)
}

// detail returns the CorralErrorDetail of err, or "" when err has none.
func detail(err error) string {
	var d interface{ CorralErrorDetail() string }
	if errors.As(err, &d) {
		return d.CorralErrorDetail()
	}
	return ""
}

func hdr(msg []byte) wire.Header { return wire.ParseHeader(msg[:16]) }

// run feeds the body of msg to a new validator in chunks of the given size and
// returns the first error from Write or Close, plus the validator.
func run(msg []byte, chunk int) (error, *wire.MsgValidator) {
	v := wire.NewMsgValidator(hdr(msg), false)
	body := msg[16:]
	for off := 0; off < len(body); off += chunk {
		end := off + chunk
		if end > len(body) {
			end = len(body)
		}
		if err := v.Write(body[off:end]); err != nil {
			return err, v
		}
	}
	return v.Close(), v
}

// verdict checks msg against the want detail ("" = accepted) under several
// chunkings, and that Done() agrees.
func verdict(t *testing.T, msg []byte, want string) {
	t.Helper()
	for _, chunk := range []int{len(msg), 1, 3, 7, 64} {
		err, v := run(msg, chunk)
		if want == "" {
			if err != nil {
				t.Fatalf("chunk %d: rejected a valid message: %v (detail %q)", chunk, err, detail(err))
			}
			if !v.Done() {
				t.Fatalf("chunk %d: Done() = false after a clean Close", chunk)
			}
			continue
		}
		if err == nil {
			t.Fatalf("chunk %d: accepted, want detail %q", chunk, want)
		}
		if got := detail(err); got != want {
			t.Fatalf("chunk %d: detail %q, want %q", chunk, got, want)
		}
		if v.Done() {
			t.Fatalf("chunk %d: Done() = true after a failure", chunk)
		}
	}
}

// protoErr is the shape of wire.ProtocolError (ADR-0016), checked by duck
// typing so the test does not depend on whether it is a struct or pointer type.
type protoErr interface {
	error
	Detail() string
	CorralErrorDetail() string
}

func asProtocolError(err error, target *protoErr) bool { return errors.As(err, target) }
