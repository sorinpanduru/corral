package bson_test

import (
	"errors"
	"math"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

func mustLookup(t *testing.T, d []byte, path ...string) bson.Element {
	t.Helper()
	e, ok, err := bson.Lookup(d, path...)
	if err != nil || !ok {
		t.Fatalf("Lookup(%v) ok=%v err=%v", path, ok, err)
	}
	return e
}

// FR-9: FirstElement returns the first key and type (the command name).
func TestFirstElement_FR9(t *testing.T) {
	d := doc(eString("find", "coll"), eString("$db", "d"))
	e, err := bson.FirstElement(d)
	if err != nil {
		t.Fatal(err)
	}
	if e.Key() != "find" || e.Type() != bson.Type(0x02) {
		t.Fatalf("first element key=%q type=%#x", e.Key(), byte(e.Type()))
	}
	if s, ok := e.StringValue(); !ok || s != "coll" {
		t.Fatalf("first element value = %q (%v)", s, ok)
	}
}

// FR-7: FirstElement on an empty or malformed document is an error.
func TestFirstElementErrors_FR7(t *testing.T) {
	for name, d := range map[string][]byte{
		"empty doc": {5, 0, 0, 0, 0}, "nil": nil, "short": {1, 2},
		"bad length": {0xFF, 0, 0, 0, 0},
		"bad type":   doc(el(0x14, "a", nil)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := bson.FirstElement(d); err == nil {
				t.Fatal("no error")
			}
		})
	}
	if _, err := bson.FirstElement(doc(el(0x14, "a", nil))); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("bad type: %v", err)
	}
}

// FR-9: every accessor returns its value for its own type and ok=false for
// every other type.
func TestElementAccessors_FR9(t *testing.T) {
	oid := [12]byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 11, 12}
	d := allTypes()

	if v, ok := mustLookup(t, d, "i32").Int32(); !ok || v != -5 {
		t.Errorf("Int32 = %d (%v)", v, ok)
	}
	if v, ok := mustLookup(t, d, "i64").Int64(); !ok || v != math.MinInt64 {
		t.Errorf("Int64 = %d (%v)", v, ok)
	}
	if v, ok := mustLookup(t, d, "double").Double(); !ok || v != 1.5 {
		t.Errorf("Double = %v (%v)", v, ok)
	}
	if v, ok := mustLookup(t, d, "t").Bool(); !ok || !v {
		t.Errorf("Bool true = %v (%v)", v, ok)
	}
	if v, ok := mustLookup(t, d, "f").Bool(); !ok || v {
		t.Errorf("Bool false = %v (%v)", v, ok)
	}
	if v, ok := mustLookup(t, d, "string").StringValue(); !ok || v != "synthetic" {
		t.Errorf("StringValue length = %d (%v)", len(v), ok)
	}
	if sub, data, ok := mustLookup(t, d, "bin").Binary(); !ok || sub != 0 || string(data) != "\x01\x02\x03" {
		t.Errorf("Binary sub=%d len=%d ok=%v", sub, len(data), ok)
	}
	d2 := doc(eObjectID("oid", oid))
	if v, ok := mustLookup(t, d2, "oid").ObjectID(); !ok || v != oid {
		t.Errorf("ObjectID mismatch (%v)", ok)
	}
	if sub, ok := mustLookup(t, doc(eDoc("x", doc(eInt32("inner", 7)))), "x").Document(); !ok || len(sub) == 0 {
		t.Errorf("Document ok=%v", ok)
	} else if err := bson.Validate(sub); err != nil {
		t.Errorf("Document() is not a valid sub-document: %v", err)
	}
	if arr, ok := mustLookup(t, d, "array").Array(); !ok || bson.Validate(arr) != nil {
		t.Errorf("Array ok=%v", ok)
	}
}

func TestElementAccessorsRejectOtherTypes_FR9(t *testing.T) {
	d := allTypes()
	str := mustLookup(t, d, "string")
	i32 := mustLookup(t, d, "i32")
	i64 := mustLookup(t, d, "i64")
	dbl := mustLookup(t, d, "double")
	b := mustLookup(t, d, "t")
	sub := mustLookup(t, d, "doc")
	arr := mustLookup(t, d, "array")
	bin := mustLookup(t, d, "bin")
	oid := mustLookup(t, d, "oid")

	if _, ok := str.Int32(); ok {
		t.Error("Int32 ok on string")
	}
	if _, ok := i32.Int64(); ok {
		t.Error("Int64 ok on int32")
	}
	if _, ok := i64.Int32(); ok {
		t.Error("Int32 ok on int64")
	}
	if _, ok := i32.Double(); ok {
		t.Error("Double ok on int32")
	}
	if _, ok := dbl.Int32(); ok {
		t.Error("Int32 ok on double")
	}
	if _, ok := i32.Bool(); ok {
		t.Error("Bool ok on int32")
	}
	if _, ok := b.StringValue(); ok {
		t.Error("StringValue ok on bool")
	}
	if _, ok := i32.StringValue(); ok {
		t.Error("StringValue ok on int32")
	}
	if _, ok := arr.Document(); ok {
		t.Error("Document ok on array")
	}
	if _, ok := sub.Array(); ok {
		t.Error("Array ok on document")
	}
	if _, ok := str.Document(); ok {
		t.Error("Document ok on string")
	}
	if _, _, ok := str.Binary(); ok {
		t.Error("Binary ok on string")
	}
	if _, ok := bin.ObjectID(); ok {
		t.Error("ObjectID ok on binary")
	}
	if _, _, ok := oid.Binary(); ok {
		t.Error("Binary ok on objectid")
	}
	if _, ok := str.Number(); ok {
		t.Error("Number ok on string")
	}
	if _, ok := b.Number(); ok {
		t.Error("Number ok on bool")
	}
	if _, ok := sub.Number(); ok {
		t.Error("Number ok on document")
	}
}

// FR-9: Number accepts int32, int64 and double.
func TestElementNumber_FR9(t *testing.T) {
	d := doc(eInt32("a", -3), eInt64("b", 1<<40), eDouble("c", 0.25))
	for key, want := range map[string]float64{"a": -3, "b": 1 << 40, "c": 0.25} {
		if v, ok := mustLookup(t, d, key).Number(); !ok || v != want {
			t.Errorf("Number(%s) = %v (%v), want %v", key, v, ok, want)
		}
	}
}

// Element types report the BSON type byte.
func TestElementType_FR9(t *testing.T) {
	d := allTypes()
	want := map[string]byte{
		"double": 0x01, "string": 0x02, "doc": 0x03, "array": 0x04, "bin": 0x05, "undef": 0x06,
		"oid": 0x07, "t": 0x08, "date": 0x09, "null": 0x0A, "re": 0x0B, "dbp": 0x0C, "js": 0x0D,
		"sym": 0x0E, "cws": 0x0F, "i32": 0x10, "ts": 0x11, "i64": 0x12, "dec": 0x13, "max": 0x7F, "min": 0xFF,
	}
	for k, typ := range want {
		e := mustLookup(t, d, k)
		if e.Key() != k || byte(e.Type()) != typ {
			t.Errorf("%s: key=%q type=%#x, want %#x", k, e.Key(), byte(e.Type()), typ)
		}
	}
}

// FR-58: elements are views into the document, not copies.
func TestElementIsView_FR58(t *testing.T) {
	d := doc(eDoc("sub", doc(eInt32("n", 1))))
	e := mustLookup(t, d, "sub")
	sub, ok := e.Document()
	if !ok {
		t.Fatal("not a document")
	}
	sub[len(sub)-1] ^= 0x01 // mutate the terminator through the view
	if d[len(d)-2] == 0 {
		t.Fatal("Document() returned a copy, not a view of the input")
	}
}

// Lookup follows document paths and array indexes ("0", "1").
func TestLookupPaths_FR9(t *testing.T) {
	d := doc(
		eString("find", "c"),
		eDoc("lsid", doc(eBinary("id", 4, make([]byte, 16)))),
		eArray("pipeline", eDoc("0", doc(eInt32("$limit", 5))), eDoc("1", doc(eInt32("$skip", 6)))),
	)
	if sub, _, ok := mustLookup(t, d, "lsid", "id").Binary(); !ok || sub != 4 {
		t.Error("lsid.id subtype 4 not found")
	}
	if n, ok := mustLookup(t, d, "pipeline", "1", "$skip").Int32(); !ok || n != 6 {
		t.Errorf("pipeline.1.$skip = %d (%v)", n, ok)
	}
	if e := mustLookup(t, d, "pipeline", "0"); e.Type() != bson.Type(0x03) {
		t.Errorf("pipeline.0 type = %#x", byte(e.Type()))
	}
	// No path means the whole lookup is not meaningful: just not found.
	if _, ok, err := bson.Lookup(d); ok || err != nil {
		t.Errorf("Lookup with empty path ok=%v err=%v", ok, err)
	}
}

// Missing keys and paths through a scalar are (zero, false, nil).
func TestLookupNotFound_FR9(t *testing.T) {
	d := doc(eInt32("a", 1), eDoc("b", doc(eInt32("c", 1))))
	for _, path := range [][]string{{"zzz"}, {"b", "zzz"}, {"a", "x"}, {"b", "c", "d"}, {"A"}} {
		if _, ok, err := bson.Lookup(d, path...); ok || err != nil {
			t.Errorf("Lookup(%v) ok=%v err=%v, want not found", path, ok, err)
		}
	}
}

// Lookup on a malformed document before the target element is an error.
func TestLookupMalformed_FR7(t *testing.T) {
	d := doc(el(0x14, "bad", nil), eInt32("a", 1))
	if _, _, err := bson.Lookup(d, "a"); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("Lookup in a malformed document: %v", err)
	}
	if _, _, err := bson.Lookup([]byte{1, 2, 3}, "a"); err == nil {
		t.Fatal("Lookup in 3 bytes: no error")
	}
}

// Duplicate keys: Lookup returns the first occurrence (the server's rule).
func TestLookupDuplicateKeyFirstWins_ADR0017(t *testing.T) {
	d := doc(eInt32("a", 1), eInt32("a", 2))
	if n, _ := mustLookup(t, d, "a").Int32(); n != 1 {
		t.Fatalf("duplicate key lookup = %d, want the first (1)", n)
	}
}

// Keys are matched byte-exactly; embedded-NUL-free keys only.
func TestLookupKeyCaseSensitive(t *testing.T) {
	d := doc(eInt32("ok", 1))
	if _, ok, _ := bson.Lookup(d, "OK"); ok {
		t.Fatal("key match is not case sensitive")
	}
}
