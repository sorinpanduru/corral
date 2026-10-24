package bson_test

// Independent, stdlib-only reference encoder for building synthetic BSON
// documents. It deliberately shares no code with the package under test, so
// the tests do not depend on the implementation to build their inputs. All
// data is synthetic.

import (
	"encoding/binary"
	"math"
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

// doc wraps already-encoded elements into a document.
func doc(elems ...[]byte) []byte {
	body := cat(elems...)
	return cat(le32(int32(len(body)+5)), body, []byte{0})
}

// el encodes one element: type byte, key cstring, payload.
func el(typ byte, key string, payload []byte) []byte {
	return cat([]byte{typ}, cstr(key), payload)
}

func strPayload(s string) []byte { return cat(le32(int32(len(s)+1)), []byte(s), []byte{0}) }

func eDouble(k string, v float64) []byte {
	return el(0x01, k, le64(int64(math.Float64bits(v))))
}
func eString(k, v string) []byte              { return el(0x02, k, strPayload(v)) }
func eDoc(k string, d []byte) []byte          { return el(0x03, k, d) }
func eArray(k string, items ...[]byte) []byte { return el(0x04, k, doc(items...)) }
func eBinary(k string, sub byte, data []byte) []byte {
	return el(0x05, k, cat(le32(int32(len(data))), []byte{sub}, data))
}
func eUndefined(k string) []byte { return el(0x06, k, nil) }
func eObjectID(k string, id [12]byte) []byte {
	return el(0x07, k, id[:])
}
func eBool(k string, v bool) []byte {
	b := byte(0)
	if v {
		b = 1
	}
	return el(0x08, k, []byte{b})
}
func eDateTime(k string, ms int64) []byte { return el(0x09, k, le64(ms)) }
func eNull(k string) []byte               { return el(0x0A, k, nil) }
func eRegex(k, pat, opts string) []byte   { return el(0x0B, k, cat(cstr(pat), cstr(opts))) }
func eDBPointer(k, ns string) []byte {
	return el(0x0C, k, cat(strPayload(ns), make([]byte, 12)))
}
func eJS(k, code string) []byte  { return el(0x0D, k, strPayload(code)) }
func eSymbol(k, s string) []byte { return el(0x0E, k, strPayload(s)) }
func eCodeWS(k, code string, scope []byte) []byte {
	inner := cat(strPayload(code), scope)
	return el(0x0F, k, cat(le32(int32(len(inner)+4)), inner))
}
func eInt32(k string, v int32) []byte      { return el(0x10, k, le32(v)) }
func eTimestamp(k string, v uint64) []byte { return el(0x11, k, le64(int64(v))) }
func eInt64(k string, v int64) []byte      { return el(0x12, k, le64(v)) }
func eDecimal(k string) []byte             { return el(0x13, k, make([]byte, 16)) }
func eMaxKey(k string) []byte              { return el(0x7F, k, nil) }
func eMinKey(k string) []byte              { return el(0xFF, k, nil) }

// allTypes returns a valid document with one element of every BSON type the
// ADR-0017 rules accept (0x01 to 0x13, 0x7F, 0xFF, including the deprecated
// 0x06, 0x0C and 0x0E).
func allTypes() []byte {
	return doc(
		eDouble("double", 1.5),
		eString("string", "synthetic"),
		eDoc("doc", doc(eInt32("inner", 7))),
		eArray("array", eInt32("0", 1), eString("1", "two")),
		eBinary("bin", 0, []byte{1, 2, 3}),
		eUndefined("undef"),
		eObjectID("oid", [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}),
		eBool("t", true),
		eBool("f", false),
		eDateTime("date", 1700000000000),
		eNull("null"),
		eRegex("re", "^a.*", "i"),
		eDBPointer("dbp", "ns"),
		eJS("js", "return 1"),
		eSymbol("sym", "s"),
		eCodeWS("cws", "return x", doc(eInt32("x", 1))),
		eInt32("i32", -5),
		eTimestamp("ts", 1<<40|3),
		eInt64("i64", math.MinInt64),
		eDecimal("dec"),
		eMaxKey("max"),
		eMinKey("min"),
	)
}

// deep builds a document nested n levels deep in one linear pass: n opening
// headers (each with a placeholder length), the leaf, then n closers with the
// lengths patched from the inside out. wrap(level) returns the element prefix
// (type byte plus key cstring) used to open the child of that level.
func deep(n int, childPrefix []byte) []byte {
	leaf := doc(eInt32("leaf", 1))
	if n <= 1 {
		return leaf
	}
	// Level i (1 = top) has total length leafLen + (n-i)*(5+len(prefix)+0)
	// where each wrapper adds 4 (length) + len(prefix) + 1 (terminator) + the
	// element's own terminator: wrapper = 4 + prefix + child + 1.
	wrapper := 4 + len(childPrefix) + 1
	out := make([]byte, 0, len(leaf)+(n-1)*wrapper)
	for i := 1; i < n; i++ {
		total := len(leaf) + (n-i)*wrapper
		out = append(out, le32(int32(total))...)
		out = append(out, childPrefix...)
	}
	out = append(out, leaf...)
	for i := 1; i < n; i++ {
		out = append(out, 0)
	}
	return out
}

// nested returns a document nested n levels deep, counting the top-level
// document as level 1 (so nested(1) is a flat document holding one scalar).
func nested(n int) []byte { return deep(n, cat([]byte{0x03}, cstr("d"))) }

// nestedArray is nested() with arrays instead of subdocuments.
func nestedArray(n int) []byte { return deep(n, cat([]byte{0x04}, cstr("0"))) }
