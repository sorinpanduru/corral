package bson_test

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// FR-7 / ADR-0017: the builder produces the same bytes as the independent
// reference encoder for each element kind, and the result validates.
func TestBuilderMatchesReference_FR9(t *testing.T) {
	inner := doc(eInt32("x", 1))
	arr := doc(eString("0", "a"))
	ts := time.Date(2031, 5, 6, 7, 8, 9, 123_000_000, time.UTC)

	b := bson.NewBuilder()
	b.AppendInt32("i32", -7)
	b.AppendInt64("i64", 1<<50)
	b.AppendDouble("dbl", 2.5)
	b.AppendBool("t", true)
	b.AppendBool("f", false)
	b.AppendString("s", "synthetic")
	b.AppendDateTime("dt", ts)
	b.AppendNull("n")
	b.AppendDocument("doc", inner)
	b.AppendArray("arr", arr)
	b.AppendBinary("bin", 4, []byte{1, 2, 3, 4})
	got := b.Bytes()

	want := doc(
		eInt32("i32", -7), eInt64("i64", 1<<50), eDouble("dbl", 2.5), eBool("t", true), eBool("f", false),
		eString("s", "synthetic"), eDateTime("dt", ts.UnixMilli()), eNull("n"),
		eDoc("doc", inner), el(0x04, "arr", arr), eBinary("bin", 4, []byte{1, 2, 3, 4}),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("builder output differs from the reference encoding (got %d bytes, want %d)", len(got), len(want))
	}
	if err := bson.Validate(got); err != nil {
		t.Fatalf("builder output does not validate: %v", err)
	}
}

// An empty builder gives the empty document.
func TestBuilderEmpty(t *testing.T) {
	if got := bson.NewBuilder().Bytes(); !bytes.Equal(got, []byte{5, 0, 0, 0, 0}) {
		t.Fatalf("empty builder bytes = %v", got)
	}
}

// StartDocument/EndDocument and StartArray/EndArray nest, to any depth within
// the cap, and read back through Lookup.
func TestBuilderNesting_FR9(t *testing.T) {
	b := bson.NewBuilder()
	b.AppendString("hello", "world")
	b.StartDocument("a")
	b.AppendInt32("b", 5)
	b.StartArray("c")
	b.AppendString("0", "x")
	b.StartDocument("1")
	b.AppendBool("deep", true)
	b.EndDocument()
	b.EndArray()
	b.EndDocument()
	b.AppendInt32("tail", 9)
	got := b.Bytes()

	want := doc(
		eString("hello", "world"),
		eDoc("a", doc(eInt32("b", 5), eArray("c", eString("0", "x"), eDoc("1", doc(eBool("deep", true)))))),
		eInt32("tail", 9),
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("nested builder output differs from the reference (got %d bytes, want %d)", len(got), len(want))
	}
	if v, ok, err := bson.Lookup(got, "a", "c", "1", "deep"); err != nil || !ok {
		t.Fatalf("Lookup into builder output: ok=%v err=%v", ok, err)
	} else if bv, _ := v.Bool(); !bv {
		t.Fatal("deep = false")
	}
}

// Builder output round-trips: build, read every value back with Lookup.
func TestBuilderRoundTrip_FR9(t *testing.T) {
	b := bson.NewBuilder()
	b.AppendInt32("i32", math.MaxInt32)
	b.AppendInt64("i64", math.MinInt64)
	b.AppendDouble("dbl", -0.125)
	b.AppendString("s", "")
	b.AppendBinary("bin", 0x80, nil)
	d := b.Bytes()
	if v, _ := mustLookup(t, d, "i32").Int32(); v != math.MaxInt32 {
		t.Errorf("i32 = %d", v)
	}
	if v, _ := mustLookup(t, d, "i64").Int64(); v != math.MinInt64 {
		t.Errorf("i64 = %d", v)
	}
	if v, _ := mustLookup(t, d, "dbl").Double(); v != -0.125 {
		t.Errorf("dbl = %v", v)
	}
	if v, ok := mustLookup(t, d, "s").StringValue(); !ok || v != "" {
		t.Errorf("s = %q (%v)", v, ok)
	}
	if sub, data, ok := mustLookup(t, d, "bin").Binary(); !ok || sub != 0x80 || len(data) != 0 {
		t.Errorf("bin sub=%d len=%d ok=%v", sub, len(data), ok)
	}
}

// AppendDocument copies the argument: later changes to it do not alter the
// builder output.
func TestBuilderCopiesInputs(t *testing.T) {
	inner := doc(eInt32("x", 1))
	b := bson.NewBuilder()
	b.AppendDocument("d", inner)
	b.AppendBinary("b", 0, []byte{1, 2, 3})
	before := append([]byte(nil), b.Bytes()...)
	for i := range inner {
		inner[i] = 0xEE
	}
	if !bytes.Equal(before, b.Bytes()) {
		t.Fatal("builder output changed after the input slice was modified")
	}
}

// Dates before the epoch and with sub-millisecond parts encode as
// milliseconds since the epoch (truncated toward UnixMilli).
func TestBuilderDateTime_FR9(t *testing.T) {
	for _, ts := range []time.Time{time.Unix(0, 0), time.Date(1960, 1, 2, 3, 4, 5, 6_000_000, time.UTC), time.Date(2040, 1, 1, 0, 0, 0, 999_999, time.UTC)} {
		b := bson.NewBuilder()
		b.AppendDateTime("d", ts)
		if !bytes.Equal(b.Bytes(), doc(eDateTime("d", ts.UnixMilli()))) {
			t.Fatalf("date %v encoded differently from UnixMilli", ts)
		}
	}
}

// Keys with an embedded NUL cannot be represented and must not corrupt the
// document: the output either still validates or the builder refuses.
func TestBuilderKeyWithNUL_DoesNotCorrupt(t *testing.T) {
	defer func() { _ = recover() }() // refusing by panic is acceptable
	b := bson.NewBuilder()
	b.AppendInt32("a\x00b", 1)
	b.AppendInt32("ok", 2)
	out := b.Bytes()
	if err := bson.Validate(out); err != nil {
		t.Fatalf("builder produced an invalid document for a key with NUL: %v", err)
	}
}

// Large strings do not break the length prefix.
func TestBuilderLargeString(t *testing.T) {
	s := string(bytes.Repeat([]byte("L"), 3<<20))
	b := bson.NewBuilder()
	b.AppendString("s", s)
	d := b.Bytes()
	if err := bson.Validate(d); err != nil {
		t.Fatal(err)
	}
	if v, ok := mustLookup(t, d, "s").StringValue(); !ok || len(v) != len(s) {
		t.Fatalf("string length = %d (%v)", len(v), ok)
	}
}
