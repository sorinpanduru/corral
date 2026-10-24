package bson_test

import (
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// walkChunks feeds d to a fresh Walker in the given chunks, then Close. It
// returns the first error from Write or Close.
func walkChunks(d []byte, chunks ...[]byte) error {
	w := bson.NewWalker(nil)
	for _, c := range chunks {
		if err := w.Write(c); err != nil {
			return err
		}
	}
	return w.Close()
}

func sameVerdict(t *testing.T, label string, want, got error) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Fatalf("%s: Validate err=%v, Walker err=%v", label, want, got)
	}
	if want != nil {
		if errors.Is(want, bson.ErrTooDeep) != errors.Is(got, bson.ErrTooDeep) ||
			errors.Is(want, bson.ErrMalformed) != errors.Is(got, bson.ErrMalformed) {
			t.Fatalf("%s: Validate and Walker disagree on the error kind", label)
		}
	}
}

// equivalenceInputs are small documents (<= 4 KiB), valid and invalid.
func equivalenceInputs() map[string][]byte {
	in := map[string][]byte{
		"empty doc":  {5, 0, 0, 0, 0},
		"all types":  allTypes(),
		"nested 50":  nested(50),
		"nested arr": nestedArray(50),
		"dup keys":   doc(eInt32("a", 1), eInt32("a", 2)),
		"utf8":       doc(eString("s", string([]byte{0xFF, 0xFE}))),
		"too deep":   nested(1025),
		"4k string":  doc(eString("s", strings.Repeat("y", 3000))),
	}
	for _, m := range malformedCases() {
		if len(m.doc) <= 4096 {
			in["bad: "+m.name] = m.doc
		}
	}
	return in
}

// FR-7, AC-7: Validate and the Walker agree when the input is split at every
// byte offset (two chunks), and when fed one byte at a time.
func TestWalkerEquivalence_EverySplit_AC7(t *testing.T) {
	for name, d := range equivalenceInputs() {
		t.Run(name, func(t *testing.T) {
			want := bson.Validate(d)
			for i := 0; i <= len(d); i++ {
				got := walkChunks(d, d[:i], d[i:])
				sameVerdict(t, "split", want, got)
			}
			chunks := make([][]byte, len(d))
			for i := range d {
				chunks[i] = d[i : i+1]
			}
			sameVerdict(t, "bytewise", want, walkChunks(d, chunks...))
		})
	}
}

// Empty writes are allowed anywhere.
func TestWalkerEmptyWrites(t *testing.T) {
	d := allTypes()
	if err := walkChunks(d, nil, d[:3], []byte{}, d[3:], nil); err != nil {
		t.Fatalf("empty writes broke a valid document: %v", err)
	}
}

// AC-7: random chunk sizes over 10 MB documents, valid and invalid.
func TestWalkerEquivalence_Large_AC7(t *testing.T) {
	big := strings.Repeat("z", 10<<20)
	valid := doc(eInt32("a", 1), eString("big", big), eDoc("tail", doc(eBool("b", true))))
	invalid := doc(eInt32("a", 1), eString("big", big), eDoc("tail", doc(el(0x08, "b", []byte{5}))))
	for name, d := range map[string][]byte{"valid": valid, "invalid": invalid} {
		t.Run(name, func(t *testing.T) {
			want := bson.Validate(d)
			rng := rand.New(rand.NewSource(7))
			for round := 0; round < 3; round++ {
				w := bson.NewWalker(nil)
				var got error
				for off := 0; off < len(d) && got == nil; {
					n := 1 + rng.Intn(1<<uint(1+rng.Intn(17)))
					if off+n > len(d) {
						n = len(d) - off
					}
					got = w.Write(d[off : off+n])
					off += n
				}
				if got == nil {
					got = w.Close()
				}
				sameVerdict(t, "random chunks", want, got)
			}
		})
	}
}

// A truncated stream is malformed at Close (not at Write); Close with nothing
// written is malformed.
func TestWalkerCloseTruncated_FR7(t *testing.T) {
	d := allTypes()
	w := bson.NewWalker(nil)
	if err := w.Write(d[:len(d)-1]); err != nil {
		t.Fatalf("Write of a valid prefix failed: %v", err)
	}
	if err := w.Close(); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("Close on truncated input: %v", err)
	}
	if err := bson.NewWalker(nil).Close(); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("Close with no input: %v", err)
	}
}

// Bytes after the declared end of the document are malformed.
func TestWalkerTrailingBytes_FR7(t *testing.T) {
	d := append(doc(eInt32("a", 1)), 0x00)
	if err := walkChunks(d, d); !errors.Is(err, bson.ErrMalformed) {
		t.Fatalf("trailing byte accepted: %v", err)
	}
}

// Errors are sticky: after a failed Write every later Write and Close fails.
func TestWalkerErrorIsSticky_FR7(t *testing.T) {
	w := bson.NewWalker(nil)
	if err := w.Write([]byte{3, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("length 3 not rejected on Write")
	}
	if err := w.Write(doc(eInt32("a", 1))); err == nil {
		t.Fatal("Write after failure succeeded")
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close after failure succeeded")
	}
}

// Depth cap through the Walker: 1024 accepted, 1025 rejected with ErrTooDeep.
func TestWalkerDepth_ADR0017(t *testing.T) {
	if err := walkChunks(nil, nested(1024)); err != nil {
		t.Fatalf("depth 1024 rejected: %v", err)
	}
	if err := walkChunks(nil, nested(1025)); !errors.Is(err, bson.ErrTooDeep) {
		t.Fatalf("depth 1025: %v", err)
	}
	if err := walkChunks(nil, nestedArray(1025)); !errors.Is(err, bson.ErrTooDeep) {
		t.Fatalf("array depth 1025: %v", err)
	}
	// Rejected while streaming, not buffered to the end: the error appears on
	// a Write before the last byte is fed.
	d := nested(100000)
	w := bson.NewWalker(nil)
	var err error
	for off := 0; off < len(d) && err == nil; off += 1024 {
		end := off + 1024
		if end > len(d) {
			end = len(d)
		}
		err = w.Write(d[off:end])
	}
	if !errors.Is(err, bson.ErrTooDeep) {
		t.Fatalf("depth 100000 via Write: %v", err)
	}
}

// Consumed counts the bytes accepted so far.
func TestWalkerConsumed(t *testing.T) {
	d := allTypes()
	w := bson.NewWalker(nil)
	if w.Consumed() != 0 {
		t.Fatalf("Consumed() = %d before any write", w.Consumed())
	}
	_ = w.Write(d[:10])
	if w.Consumed() != 10 {
		t.Fatalf("Consumed() = %d, want 10", w.Consumed())
	}
	_ = w.Write(d[10:])
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Consumed() != int64(len(d)) {
		t.Fatalf("Consumed() = %d, want %d", w.Consumed(), len(d))
	}
}

// Write must not retain or modify the caller's buffer: overwrite it after
// each Write and captured values must still be right.
func TestWalkerDoesNotRetainChunks(t *testing.T) {
	d := doc(eString("s", "synthetic-value"), eInt32("n", 42))
	w := bson.NewWalker([]bson.Capture{
		{Path: []string{"s"}, Mode: bson.CaptureValue},
		{Path: []string{"n"}, Mode: bson.CaptureValue},
	})
	buf := make([]byte, 3)
	for off := 0; off < len(d); off += 3 {
		end := off + 3
		if end > len(d) {
			end = len(d)
		}
		n := copy(buf, d[off:end])
		if err := w.Write(buf[:n]); err != nil {
			t.Fatal(err)
		}
		for i := range buf {
			buf[i] = 0xAA
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if s := capString(t, w, 0); s != "synthetic-value" {
		t.Fatalf("captured string changed after buffer reuse")
	}
	if n := capInt32(t, w, 1); n != 42 {
		t.Fatalf("captured int32 = %d", n)
	}
}

// AC-29: Write allocates O(1): the allocation count for a large document does
// not grow with its size.
func TestWalkerAllocsConstant_AC29(t *testing.T) {
	measure := func(size int) float64 {
		d := doc(eString("big", strings.Repeat("a", size)), eDoc("x", doc(eInt32("n", 1))))
		return testing.AllocsPerRun(5, func() {
			w := bson.NewWalker(nil)
			for off := 0; off < len(d); off += 4096 {
				end := off + 4096
				if end > len(d) {
					end = len(d)
				}
				if err := w.Write(d[off:end]); err != nil {
					panic("unexpected walker error")
				}
			}
			if err := w.Close(); err != nil {
				panic("unexpected walker error")
			}
		})
	}
	small, large := measure(64<<10), measure(8<<20)
	if large > small+2 {
		t.Fatalf("allocations grew with document size: %v (64 KiB) vs %v (8 MiB)", small, large)
	}
	if large > 32 {
		t.Fatalf("allocations per large document = %v, want a small constant", large)
	}
}

// Deep nesting does not allocate per level without bound: memory per level is
// constant, so 1,024 levels stay within a small fixed allocation count.
func TestWalkerAllocsDeep_AC29(t *testing.T) {
	d := nested(1024)
	got := testing.AllocsPerRun(5, func() {
		w := bson.NewWalker(nil)
		if err := w.Write(d); err != nil {
			panic("unexpected walker error")
		}
		if err := w.Close(); err != nil {
			panic("unexpected walker error")
		}
	})
	if got > 64 {
		t.Fatalf("allocations for a 1024-level document = %v, want a small constant", got)
	}
}
