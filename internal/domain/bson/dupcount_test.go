package bson_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// SPEC-002 Amendment 1 (FR-60): top-level captures report Count, the number
// of occurrences of the key at the top level; the value is the first one.

func countOf(t *testing.T, w *bson.Walker, i int) int {
	t.Helper()
	v, ok := w.Captured(i)
	if !ok {
		t.Fatalf("capture %d not found", i)
	}
	return v.Count
}

func TestCaptureCount_OneTwoThree_FR60(t *testing.T) {
	cases := []struct {
		name  string
		d     []byte
		count int
		first int32
	}{
		{"one", doc(eInt32("n", 10), eInt32("x", 0)), 1, 10},
		{"two", doc(eInt32("n", 10), eInt32("x", 0), eInt32("n", 20)), 2, 10},
		{"three", doc(eInt32("n", 10), eInt32("n", 20), eString("n", "s")), 3, 10},
		{"three with mixed types, later ones differ", doc(eInt32("n", 10), eDoc("n", doc()), eNull("n")), 3, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, 7, len(tc.d)} {
				w := runCaptures(t, tc.d, []bson.Capture{val("n")}, chunk)
				if got := countOf(t, w, 0); got != tc.count {
					t.Fatalf("chunk %d: Count = %d, want %d", chunk, got, tc.count)
				}
				if got := capInt32(t, w, 0); got != tc.first {
					t.Fatalf("chunk %d: value = %d, want first occurrence %d", chunk, got, tc.first)
				}
			}
		})
	}
}

// A key that occurs only inside a nested document or an array does not count.
func TestCaptureCount_NestedDoesNotCount_FR60(t *testing.T) {
	d := doc(
		eInt32("n", 1),
		eDoc("sub", doc(eInt32("n", 2), eInt32("n", 3))),
		eArray("arr", eDoc("0", doc(eInt32("n", 4)))),
		eArray("n2", eInt32("n", 5)),
	)
	w := runCaptures(t, d, []bson.Capture{val("n")}, 5)
	if got := countOf(t, w, 0); got != 1 {
		t.Fatalf("Count = %d, want 1 (nested occurrences must not count)", got)
	}
	// A top-level key that appears only nested is not found at all.
	d2 := doc(eDoc("sub", doc(eInt32("only", 2))))
	w2 := runCaptures(t, d2, []bson.Capture{val("only")}, 5)
	if _, ok := w2.Captured(0); ok {
		t.Fatal("nested-only key was captured at the top level")
	}
}

// Count is identical under every split of a small document and under random
// chunking of a large one.
func TestCaptureCount_ChunkingIndependent_FR60(t *testing.T) {
	d := doc(eInt32("a", 1), eString("a", strings.Repeat("x", 300)), eDoc("a", doc(eInt32("a", 1))), eInt32("b", 1), eInt32("a", 9))
	for i := 0; i <= len(d); i++ {
		w := bson.NewWalker([]bson.Capture{val("a"), val("b")})
		if err := w.Write(d[:i]); err != nil {
			t.Fatal(err)
		}
		if err := w.Write(d[i:]); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if countOf(t, w, 0) != 4 || countOf(t, w, 1) != 1 {
			t.Fatalf("split %d: counts a=%d b=%d, want 4 and 1", i, countOf(t, w, 0), countOf(t, w, 1))
		}
	}
	// Large: the duplicate follows a 5 MB value.
	big := doc(eInt32("k", 1), eString("pad", strings.Repeat("p", 5<<20)), eInt32("k", 2))
	w := runCaptures(t, big, []bson.Capture{val("k")}, 4099)
	if countOf(t, w, 0) != 2 {
		t.Fatalf("Count after 5 MB value = %d, want 2", countOf(t, w, 0))
	}
}

// Count applies to a capture whose value is too long (the key is still
// counted) and to presence captures.
func TestCaptureCount_LongValuesAndPresence_FR60(t *testing.T) {
	long := strings.Repeat("L", 1000)
	d := doc(eString("v", long), eString("v", "short"), eNull("p"), eNull("p"))
	w := runCaptures(t, d, []bson.Capture{val("v"), {Path: []string{"p"}, Mode: bson.CapturePresence}}, 17)
	v, _ := w.Captured(0)
	if !v.TooLong || v.Count != 2 {
		t.Fatalf("TooLong=%v Count=%d, want true and 2", v.TooLong, v.Count)
	}
	if countOf(t, w, 1) != 2 {
		t.Fatalf("presence Count = %d, want 2", countOf(t, w, 1))
	}
}

// Counting is by exact key bytes.
func TestCaptureCount_KeyExactness_FR60(t *testing.T) {
	d := doc(eInt32("lsid", 1), eInt32("Lsid", 1), eInt32("lsid2", 1), eInt32("lsi", 1), eInt32("lsid", 1))
	w := runCaptures(t, d, []bson.Capture{val("lsid")}, 3)
	if countOf(t, w, 0) != 2 {
		t.Fatalf("Count = %d, want 2", countOf(t, w, 0))
	}
}

func firstKeyRepeats(t *testing.T, d []byte, chunk int) int {
	t.Helper()
	w := runCaptures(t, d, nil, chunk)
	return w.TopLevelKeyEqualsFirst()
}

// TopLevelKeyEqualsFirst counts later top-level keys equal to the first key
// (the command name); nested and array occurrences do not count.
func TestTopLevelKeyEqualsFirst_FR60(t *testing.T) {
	cases := []struct {
		name string
		d    []byte
		want int
	}{
		{"no repeat", doc(eString("find", "c"), eString("$db", "d")), 0},
		{"one repeat", doc(eString("find", "c"), eString("$db", "d"), eString("find", "x")), 1},
		{"two repeats, different types", doc(eString("find", "c"), eInt32("find", 1), eDoc("find", doc())), 2},
		{"nested only", doc(eString("find", "c"), eDoc("o", doc(eString("find", "x"))), eArray("a", eString("find", "y"))), 0},
		{"similar keys", doc(eString("find", "c"), eString("Find", "x"), eString("find2", "x"), eString("fin", "x")), 0},
		{"single element", doc(eString("ping", "")), 0},
		{"empty doc", doc(), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "empty doc" {
				w := runCaptures(t, tc.d, nil, 1)
				if w.TopLevelKeyEqualsFirst() != 0 {
					t.Fatal("empty doc has repeats")
				}
				return
			}
			for _, chunk := range []int{1, 2, 5, len(tc.d)} {
				if got := firstKeyRepeats(t, tc.d, chunk); got != tc.want {
					t.Fatalf("chunk %d: TopLevelKeyEqualsFirst = %d, want %d", chunk, got, tc.want)
				}
			}
		})
	}
}

// Keys are compared up to the 256-byte bound: a long first key repeated is
// still counted, and keys that differ only after the bound are treated as
// equal (documented ADR-0017 bound).
func TestTopLevelKeyEqualsFirst_LongKeys_FR60(t *testing.T) {
	k := strings.Repeat("k", 300)
	d := doc(eInt32(k, 1), eInt32(k, 2), eInt32("other", 3))
	if got := firstKeyRepeats(t, d, 11); got != 1 {
		t.Fatalf("long repeated key: %d, want 1", got)
	}
}

// Duplicates in general are still accepted by Validate and the Walker, and
// Lookup stays first-wins.
func TestDuplicatesStillAccepted_FR60(t *testing.T) {
	d := doc(eString("find", "c"), eString("find", "d"), eDoc("x", doc(eInt32("k", 1), eInt32("k", 2))), eInt32("lsid", 1), eInt32("lsid", 2))
	if err := bson.Validate(d); err != nil {
		t.Fatalf("Validate rejected duplicates: %v", err)
	}
	if err := walkChunks(d, d); err != nil {
		t.Fatalf("Walker rejected duplicates: %v", err)
	}
	if s, _ := mustLookup(t, d, "find").StringValue(); s != "c" {
		t.Fatalf("Lookup returned %q, want the first occurrence", s)
	}
	if n, _ := mustLookup(t, d, "lsid").Int32(); n != 1 {
		t.Fatalf("Lookup(lsid) = %d, want 1", n)
	}
}

// A malformed document with duplicates is still malformed (counting does not
// mask validation).
func TestDuplicateCountsDoNotMaskMalformed_FR60(t *testing.T) {
	d := doc(eInt32("n", 1), eInt32("n", 2), el(0x08, "b", []byte{7}))
	w := bson.NewWalker([]bson.Capture{val("n")})
	err := w.Write(d)
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		t.Fatal("malformed document accepted")
	}
}

// Mixed documents and arrays nested 1,024 deep are accepted and 1,025 is
// rejected (FR-60: arrays count like documents).
func TestDepthMixedDocsAndArrays_FR60(t *testing.T) {
	build := func(n int) []byte {
		d := doc(eInt32("leaf", 1))
		for i := 1; i < n; i++ {
			if i%2 == 0 {
				d = doc(el(0x04, "0", d))
			} else {
				d = doc(eDoc("d", d))
			}
		}
		return d
	}
	if err := bson.Validate(build(1024)); err != nil {
		t.Fatalf("mixed depth 1024 rejected: %v", err)
	}
	if err := walkChunks(nil, build(1024)); err != nil {
		t.Fatalf("walker mixed depth 1024 rejected: %v", err)
	}
	d := build(1025)
	if err := bson.Validate(d); !errors.Is(err, bson.ErrTooDeep) {
		t.Fatalf("mixed depth 1025 via Validate: %v", err)
	}
	if err := walkChunks(nil, d); !errors.Is(err, bson.ErrTooDeep) {
		t.Fatalf("mixed depth 1025 via Walker: %v", err)
	}
}
