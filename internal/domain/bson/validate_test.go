package bson_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// malformedCase is one invalid document, named for the ADR-0017 rule it breaks.
type malformedCase struct {
	name string
	doc  []byte
}

func patch(d []byte, off int, b ...byte) []byte {
	out := append([]byte(nil), d...)
	copy(out[off:], b)
	return out
}

// malformedCases covers every ADR-0017 rule with at least one input.
func malformedCases() []malformedCase {
	valid := doc(eInt32("a", 1))
	trunc := func(d []byte, n int) []byte { return append([]byte(nil), d[:len(d)-n]...) }
	return []malformedCase{
		{"empty input", nil},
		{"three bytes", []byte{5, 0, 0}},
		{"declared length 0", []byte{0, 0, 0, 0, 0}},
		{"declared length 4", []byte{4, 0, 0, 0, 0}},
		{"declared length 1", []byte{1, 0, 0, 0, 0}},
		{"negative declared length", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0}},
		{"huge declared length", []byte{0xFF, 0xFF, 0xFF, 0x7F, 0}},
		{"declared length above content", patch(valid, 0, byte(len(valid)+1))},
		{"declared length below content", patch(valid, 0, byte(len(valid)-1))},
		{"trailing bytes after document", append(append([]byte(nil), valid...), 0)},
		{"missing terminator", cat(le32(int32(len(valid))), valid[4:len(valid)-1], []byte{1})},
		{"truncated by one", trunc(valid, 1)},
		{"early terminator", cat(le32(int32(len(valid)+4)), []byte{0}, valid[4:])},
		{"unknown type 0x14", doc(el(0x14, "a", le32(1)))},
		{"unknown type 0x20", doc(el(0x20, "a", le32(1)))},
		{"unknown type 0x80", doc(el(0x80, "a", le32(1)))},
		{"unknown type 0xFE", doc(el(0xFE, "a", le32(1)))},
		{"unterminated key", cat(le32(9), []byte{0x10, 'a', 'b', 'c'}, []byte{0})},
		{"string length 0", doc(el(0x02, "a", cat(le32(0), []byte{0})))},
		{"string length negative", doc(el(0x02, "a", cat(le32(-1), []byte{0})))},
		{"string missing NUL", doc(el(0x02, "a", cat(le32(3), []byte("abc"))))},
		{"string length overruns document", doc(el(0x02, "a", cat(le32(100), []byte("ab\x00"))))},
		{"string length overruns by one", doc(el(0x02, "a", cat(le32(4), []byte("ab\x00"))))},
		{"javascript length 0", doc(el(0x0D, "a", cat(le32(0), []byte{0})))},
		{"symbol missing NUL", doc(el(0x0E, "a", cat(le32(2), []byte("ab"))))},
		{"bool byte 2", doc(el(0x08, "a", []byte{2}))},
		{"bool byte 0xFF", doc(el(0x08, "a", []byte{0xFF}))},
		{"nested length overrun", doc(el(0x03, "a", patch(doc(eInt32("x", 1)), 0, 0x40, 0, 0, 0)))},
		{"nested length underrun", doc(el(0x03, "a", patch(doc(eInt32("x", 1)), 0, 6, 0, 0, 0)))},
		{"nested length below 5", doc(el(0x03, "a", []byte{4, 0, 0, 0, 0}))},
		{"nested missing terminator", doc(el(0x03, "a", cat(le32(8), []byte{0x10, 'x', 0, 1, 0, 0, 0})))},
		{"array length overrun", doc(el(0x04, "a", patch(doc(eInt32("0", 1)), 0, 0x40, 0, 0, 0)))},
		{"malformed inside array", doc(el(0x04, "a", doc(el(0x08, "0", []byte{9}))))},
		{"malformed deep inside nested doc", doc(eDoc("a", doc(eDoc("b", doc(el(0x14, "c", nil))))))},
		{"binary length negative", doc(el(0x05, "a", cat(le32(-1), []byte{0})))},
		{"binary length overruns", doc(el(0x05, "a", cat(le32(50), []byte{0, 1})))},
		{"binary missing subtype", doc(el(0x05, "a", le32(0)))},
		{"code-with-scope total too small", doc(el(0x0F, "a", cat(le32(3), strPayload("x"), doc())))},
		{"code-with-scope total overruns", doc(el(0x0F, "a", cat(le32(500), strPayload("x"), doc())))},
		{"code-with-scope total mismatch", doc(el(0x0F, "a", cat(le32(int32(4+len(strPayload("x"))+len(doc())+3)), strPayload("x"), doc())))},
		{"code-with-scope bad string", doc(el(0x0F, "a", cat(le32(13), le32(0), doc())))},
		{"code-with-scope bad scope doc", doc(el(0x0F, "a", cat(le32(int32(4+len(strPayload("x"))+5)), strPayload("x"), []byte{9, 0, 0, 0, 0})))},
		{"code-with-scope scope contains bad element", doc(eCodeWS("a", "x", doc(el(0x08, "b", []byte{7}))))},
		{"int32 truncated", doc(el(0x10, "a", []byte{1, 0, 0}))},
		{"int64 truncated", doc(el(0x12, "a", []byte{1, 2, 3}))},
		{"double truncated", doc(el(0x01, "a", []byte{1, 2, 3, 4}))},
		{"objectid truncated", doc(el(0x07, "a", make([]byte, 11)))},
		{"decimal128 truncated", doc(el(0x13, "a", make([]byte, 15)))},
		{"datetime truncated", doc(el(0x09, "a", make([]byte, 7)))},
		{"regex options missing", doc(el(0x0B, "a", []byte("pq")))},
		{"regex options unterminated", doc(el(0x0B, "a", cat(cstr("p"), []byte("i"))))},
		{"dbpointer missing id bytes", doc(el(0x0C, "a", cat(strPayload("ns"), make([]byte, 5))))},
	}
}

// ADR-0017: one malformed input per rule is rejected with ErrMalformed.
func TestValidateRejectsMalformed_FR7(t *testing.T) {
	for _, tc := range malformedCases() {
		t.Run(tc.name, func(t *testing.T) {
			err := bson.Validate(tc.doc)
			if err == nil {
				t.Fatalf("Validate accepted a malformed document (len %d)", len(tc.doc))
			}
			if !errors.Is(err, bson.ErrMalformed) {
				t.Fatalf("error is not ErrMalformed")
			}
		})
	}
}

// FR-7: documents with every element type are accepted.
func TestValidateAcceptsAllTypes_FR7(t *testing.T) {
	if err := bson.Validate(allTypes()); err != nil {
		t.Fatalf("Validate rejected a valid all-types document: %v", err)
	}
}

func TestValidateAcceptsEachTypeAlone_FR7(t *testing.T) {
	elems := map[string][]byte{
		"double": eDouble("a", math.Inf(1)), "string": eString("a", ""), "empty doc": eDoc("a", doc()),
		"empty array": eArray("a"), "binary empty": eBinary("a", 0x80, nil), "binary uuid": eBinary("a", 4, make([]byte, 16)),
		"undefined": eUndefined("a"), "oid": eObjectID("a", [12]byte{}), "bool": eBool("a", true),
		"date": eDateTime("a", -1), "null": eNull("a"), "regex": eRegex("a", "", ""),
		"dbpointer": eDBPointer("a", ""), "js": eJS("a", ""), "symbol": eSymbol("a", ""),
		"codews": eCodeWS("a", "", doc()), "int32": eInt32("a", math.MaxInt32), "timestamp": eTimestamp("a", math.MaxUint64),
		"int64": eInt64("a", 1), "decimal": eDecimal("a"), "maxkey": eMaxKey("a"), "minkey": eMinKey("a"),
		"empty key": eInt32("", 1),
	}
	for name, e := range elems {
		t.Run(name, func(t *testing.T) {
			if err := bson.Validate(doc(e)); err != nil {
				t.Fatalf("Validate rejected a valid document: %v", err)
			}
		})
	}
}

func TestValidateEmptyDocument_FR7(t *testing.T) {
	if err := bson.Validate([]byte{5, 0, 0, 0, 0}); err != nil {
		t.Fatalf("Validate rejected the empty document: %v", err)
	}
}

// Nested documents and arrays are validated recursively.
func TestValidateNested_FR7(t *testing.T) {
	d := doc(eDoc("a", doc(eArray("b", eDoc("0", doc(eString("c", "x")))))))
	if err := bson.Validate(d); err != nil {
		t.Fatalf("Validate rejected a nested document: %v", err)
	}
}

// ADR-0017 (as implemented per the plan): string contents are not UTF-8
// checked, because the server accepts what drivers send. Synthetic bytes only.
// Flip this test if the Owner amends ADR-0017 to require UTF-8.
func TestValidateDoesNotCheckUTF8_ADR0017(t *testing.T) {
	bad := string([]byte{0xC3, 0x28, 0xFF, 0xFE})
	if err := bson.Validate(doc(eString("s", bad), eJS("j", bad))); err != nil {
		t.Fatalf("Validate rejected invalid UTF-8 string content: %v", err)
	}
}

// Duplicate keys are accepted (the server does so too); validation does not
// reject them, at the top level or nested.
func TestValidateAcceptsDuplicateKeys_ADR0017(t *testing.T) {
	d := doc(eInt32("a", 1), eInt32("a", 2), eDoc("d", doc(eInt32("k", 1), eString("k", "x"))))
	if err := bson.Validate(d); err != nil {
		t.Fatalf("Validate rejected duplicate keys: %v", err)
	}
}

// FR-7: errors are constant sentinels that never contain input bytes.
func TestErrorsCarryNoInput_FR9(t *testing.T) {
	marker := "SYNTHETIC-MARKER-7f3a"
	d := doc(eString("a", marker), el(0x14, marker, nil))
	err := bson.Validate(d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatal("error text contains input bytes")
	}
	if err.Error() != bson.ErrMalformed.Error() {
		t.Fatalf("error text differs from the sentinel text")
	}
	if bson.ErrMalformed.Error() == "" || bson.ErrTooDeep.Error() == "" || errors.Is(bson.ErrMalformed, bson.ErrTooDeep) {
		t.Fatal("sentinels must be distinct and non-empty")
	}
}

// Validate does not modify its input.
func TestValidateDoesNotMutate(t *testing.T) {
	d := allTypes()
	cp := append([]byte(nil), d...)
	_ = bson.Validate(d)
	if string(d) != string(cp) {
		t.Fatal("Validate modified its input")
	}
}

// ADR-0017 depth cap: 1,024 levels accepted, 1,025 rejected with ErrTooDeep
// (levels count the top-level document as level 1).
func TestValidateDepth_ADR0017(t *testing.T) {
	for name, build := range map[string]func(int) []byte{"documents": nested, "arrays": nestedArray} {
		t.Run(name, func(t *testing.T) {
			if err := bson.Validate(build(1024)); err != nil {
				t.Fatalf("depth 1024 rejected: %v", err)
			}
			err := bson.Validate(build(1025))
			if !errors.Is(err, bson.ErrTooDeep) {
				t.Fatalf("depth 1025: err is not ErrTooDeep (got %v)", err)
			}
			// Far beyond the cap must not blow the stack or take long.
			err = bson.Validate(build(100000))
			if !errors.Is(err, bson.ErrTooDeep) {
				t.Fatalf("depth 100000: err is not ErrTooDeep (got %v)", err)
			}
		})
	}
}

// A large valid document (10 MB string) validates.
func TestValidateLargeDocument(t *testing.T) {
	d := doc(eString("big", strings.Repeat("x", 10<<20)), eInt32("n", 1))
	if err := bson.Validate(d); err != nil {
		t.Fatalf("Validate rejected a 10 MB document: %v", err)
	}
}

// A malformed element at the very end of a large document is still found.
func TestValidateLargeDocumentLateFailure(t *testing.T) {
	d := doc(eString("big", strings.Repeat("x", 10<<20)), el(0x08, "b", []byte{3}))
	if err := bson.Validate(d); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("late malformed element not found: %v", err)
	}
}
