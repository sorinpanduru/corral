package bson_test

import (
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// Interface assumption (stated in the report): bson.Value has the field
// TooLong and the scalar accessors of bson.Element (Int32, Int64, Double,
// Number, Bool, StringValue, Binary). Its type tag is not used. For
// CaptureFirstKey the captured Value is a string holding the key. All Value
// access goes through the helpers below.

func runCaptures(t *testing.T, d []byte, caps []bson.Capture, chunk int) *bson.Walker {
	t.Helper()
	w := bson.NewWalker(caps)
	for off := 0; off < len(d); off += chunk {
		end := off + chunk
		if end > len(d) {
			end = len(d)
		}
		if err := w.Write(d[off:end]); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	return w
}

func capString(t *testing.T, w *bson.Walker, i int) string {
	t.Helper()
	v, ok := w.Captured(i)
	if !ok {
		t.Fatalf("capture %d not found", i)
	}
	s, ok := v.StringValue()
	if !ok {
		t.Fatalf("capture %d is not a string", i)
	}
	return s
}

func capInt32(t *testing.T, w *bson.Walker, i int) int32 {
	t.Helper()
	v, ok := w.Captured(i)
	if !ok {
		t.Fatalf("capture %d not found", i)
	}
	n, ok := v.Int32()
	if !ok {
		t.Fatalf("capture %d is not an int32", i)
	}
	return n
}

func val(path ...string) bson.Capture { return bson.Capture{Path: path, Mode: bson.CaptureValue} }

// FR-58: `ok` is captured after a 5 MB `cursor` field, regardless of chunking.
func TestCaptureOkAfterLargeCursor_FR58(t *testing.T) {
	big := strings.Repeat("c", 5<<20)
	d := doc(
		eDoc("cursor", doc(eInt64("id", 0), eString("ns", "db.coll"), eArray("firstBatch", eDoc("0", doc(eString("v", big)))))),
		eDouble("ok", 1),
	)
	for _, chunk := range []int{1 << 10, 64 << 10, len(d)} {
		w := runCaptures(t, d, []bson.Capture{val("ok"), val("cursor", "id")}, chunk)
		v, ok := w.Captured(0)
		if !ok {
			t.Fatalf("chunk %d: ok not captured", chunk)
		}
		f, ok := v.Double()
		if !ok || f != 1 {
			t.Fatalf("chunk %d: ok = %v (%v)", chunk, f, ok)
		}
		v, ok = w.Captured(1)
		if !ok {
			t.Fatalf("chunk %d: cursor.id not captured", chunk)
		}
		if n, ok := v.Int64(); !ok || n != 0 {
			t.Fatalf("chunk %d: cursor.id = %d (%v)", chunk, n, ok)
		}
	}
}

// FR-58: int32/int64/double all read through Number for ok-like fields.
func TestCaptureNumber_FR58(t *testing.T) {
	d := doc(eInt32("a", 1), eInt64("b", 2), eDouble("c", 3.5), eString("d", "x"))
	w := runCaptures(t, d, []bson.Capture{val("a"), val("b"), val("c"), val("d")}, 2)
	want := []float64{1, 2, 3.5}
	for i, wf := range want {
		v, _ := w.Captured(i)
		if f, ok := v.Number(); !ok || f != wf {
			t.Fatalf("capture %d Number() = %v (%v), want %v", i, f, ok, wf)
		}
	}
	v, _ := w.Captured(3)
	if _, ok := v.Number(); ok {
		t.Fatal("Number() ok for a string value")
	}
}

// FR-58: writeConcernError.code and cursor.id (nested paths).
func TestCaptureNestedPaths_FR58(t *testing.T) {
	d := doc(
		eDouble("ok", 1),
		eDoc("writeConcernError", doc(eInt32("code", 64), eString("codeName", "WriteConcernFailed"), eString("errmsg", "synthetic"))),
		eDoc("cursor", doc(eInt64("id", 1234567890123))),
	)
	w := runCaptures(t, d, []bson.Capture{val("writeConcernError", "code"), val("writeConcernError", "codeName"), val("cursor", "id")}, 7)
	if n := capInt32(t, w, 0); n != 64 {
		t.Fatalf("writeConcernError.code = %d", n)
	}
	if s := capString(t, w, 1); s != "WriteConcernFailed" {
		t.Fatalf("writeConcernError.codeName length = %d", len(s))
	}
	v, _ := w.Captured(2)
	if n, ok := v.Int64(); !ok || n != 1234567890123 {
		t.Fatalf("cursor.id = %d (%v)", n, ok)
	}
}

// FR-9: lsid.id is binary subtype 4 (UUID).
func TestCaptureLsidBinary_FR9(t *testing.T) {
	id := make([]byte, 16)
	for i := range id {
		id[i] = byte(i + 1)
	}
	d := doc(eString("find", "c"), eDoc("lsid", doc(eBinary("id", 4, id))), eString("$db", "d"))
	w := runCaptures(t, d, []bson.Capture{val("lsid", "id")}, 3)
	v, ok := w.Captured(0)
	if !ok {
		t.Fatal("lsid.id not captured")
	}
	sub, data, ok := v.Binary()
	if !ok || sub != 4 || string(data) != string(id) {
		t.Fatalf("lsid.id subtype=%d len=%d ok=%v", sub, len(data), ok)
	}
}

// FR-9: the first key of pipeline.0 (the first stage name of an aggregate).
func TestCaptureFirstKey_FR9(t *testing.T) {
	d := doc(
		eString("aggregate", "c"),
		eArray("pipeline", eDoc("0", doc(eDoc("$match", doc()), eInt32("other", 1))), eDoc("1", doc(eInt32("$limit", 1)))),
		eString("$db", "d"),
	)
	w := runCaptures(t, d, []bson.Capture{{Path: []string{"pipeline", "0"}, Mode: bson.CaptureFirstKey}}, 5)
	if s := capString(t, w, 0); s != "$match" {
		t.Fatalf("first key = %q, want $match", s)
	}
	// Empty stage document: nothing to capture.
	d2 := doc(eArray("pipeline", eDoc("0", doc())))
	w2 := runCaptures(t, d2, []bson.Capture{{Path: []string{"pipeline", "0"}, Mode: bson.CaptureFirstKey}}, 5)
	if _, ok := w2.Captured(0); ok {
		t.Fatal("first key captured from an empty document")
	}
}

// CapturePresence reports existence without a value.
func TestCapturePresence_FR9(t *testing.T) {
	d := doc(eNull("a"), eDoc("b", doc(eInt32("c", 1))), eString("s", strings.Repeat("p", 5000)))
	caps := []bson.Capture{
		{Path: []string{"a"}, Mode: bson.CapturePresence},
		{Path: []string{"b", "c"}, Mode: bson.CapturePresence},
		{Path: []string{"s"}, Mode: bson.CapturePresence},
		{Path: []string{"missing"}, Mode: bson.CapturePresence},
		{Path: []string{"b", "missing"}, Mode: bson.CapturePresence},
	}
	w := runCaptures(t, d, caps, 11)
	for i, want := range []bool{true, true, true, false, false} {
		if _, ok := w.Captured(i); ok != want {
			t.Fatalf("presence %d = %v, want %v", i, ok, want)
		}
	}
}

// Missing paths and paths through a non-document are simply not found, not
// errors.
func TestCaptureNotFound_FR58(t *testing.T) {
	d := doc(eInt32("cursor", 5), eString("s", "x"))
	w := runCaptures(t, d, []bson.Capture{val("ok"), val("cursor", "id"), val("s", "x"), val("cursor", "id", "deeper")}, 4)
	for i := 0; i < 4; i++ {
		if _, ok := w.Captured(i); ok {
			t.Fatalf("capture %d found, want not found", i)
		}
	}
	// Out-of-range index is not found.
	if _, ok := w.Captured(99); ok {
		t.Fatal("Captured(99) ok")
	}
	if _, ok := w.Captured(-1); ok {
		t.Fatal("Captured(-1) ok")
	}
}

// Captures only descend into documents and arrays; a capture is a scalar copy.
// CaptureValue on a document or array path reports found-or-not without data.
func TestCaptureValueOnNonScalar_FR58(t *testing.T) {
	d := doc(eDoc("cursor", doc(eInt64("id", 9))), eArray("arr", eInt32("0", 1)))
	w := runCaptures(t, d, []bson.Capture{val("cursor"), val("arr")}, 6)
	for i := 0; i < 2; i++ {
		if v, ok := w.Captured(i); ok {
			if _, isStr := v.StringValue(); isStr {
				t.Fatalf("capture %d of a container looks like a string", i)
			}
			if _, isNum := v.Number(); isNum {
				t.Fatalf("capture %d of a container looks numeric", i)
			}
		}
	}
}

// ADR-0017: strings up to 256 bytes are kept; longer strings are reported as
// present and too long, with no data.
func TestCaptureStringBound_ADR0017(t *testing.T) {
	for _, tc := range []struct {
		n       int
		tooLong bool
	}{{0, false}, {1, false}, {255, false}, {256, false}, {257, true}, {1024, true}, {1 << 20, true}} {
		s := strings.Repeat("q", tc.n)
		d := doc(eString("v", s))
		w := runCaptures(t, d, []bson.Capture{val("v")}, 13)
		v, ok := w.Captured(0)
		if !ok {
			t.Fatalf("len %d: not captured", tc.n)
		}
		if v.TooLong != tc.tooLong {
			t.Fatalf("len %d: TooLong = %v, want %v", tc.n, v.TooLong, tc.tooLong)
		}
		got, _ := v.StringValue()
		if tc.tooLong && got != "" {
			t.Fatalf("len %d: %d bytes of data kept for a too-long string", tc.n, len(got))
		}
		if !tc.tooLong && got != s {
			t.Fatalf("len %d: captured %d bytes", tc.n, len(got))
		}
	}
}

// Duplicate keys: the first occurrence wins (matching Lookup).
func TestCaptureDuplicateKeyFirstWins_ADR0017(t *testing.T) {
	d := doc(eInt32("n", 1), eInt32("n", 2))
	w := runCaptures(t, d, []bson.Capture{val("n")}, 100)
	if n := capInt32(t, w, 0); n != 1 {
		t.Fatalf("duplicate key capture = %d, want the first (1)", n)
	}
}

// A capture matches only at its own depth and path: a same-named key in a
// sibling or nested document does not match.
func TestCapturePathExact_FR58(t *testing.T) {
	d := doc(eDoc("other", doc(eInt32("ok", 7))), eDoc("cursor", doc(eDoc("deep", doc(eInt64("id", 8))))), eDouble("ok", 1))
	w := runCaptures(t, d, []bson.Capture{val("ok"), val("cursor", "id")}, 9)
	v, ok := w.Captured(0)
	if f, isD := v.Double(); !ok || !isD || f != 1 {
		t.Fatalf("top-level ok captured the nested value")
	}
	if _, ok := w.Captured(1); ok {
		t.Fatal("cursor.id matched cursor.deep.id")
	}
}

// Bytes after a failed validation: captures from a rejected stream are not
// relied on, but the error is reported.
func TestCaptureWithMalformedDocument_FR7(t *testing.T) {
	d := doc(eDouble("ok", 1), el(0x08, "b", []byte{4}))
	w := bson.NewWalker([]bson.Capture{val("ok")})
	err := w.Write(d)
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		t.Fatal("malformed document accepted with captures configured")
	}
}

// Captures never change the verdict: every capture configuration agrees with
// Validate on the same inputs.
func TestCapturesDoNotChangeVerdict_AC7(t *testing.T) {
	caps := []bson.Capture{val("a"), val("d", "k"), {Path: []string{"array", "0"}, Mode: bson.CaptureFirstKey}, {Path: []string{"string"}, Mode: bson.CapturePresence}}
	for name, d := range equivalenceInputs() {
		want := bson.Validate(d)
		w := bson.NewWalker(caps)
		err := w.Write(d)
		if err == nil {
			err = w.Close()
		}
		sameVerdict(t, name, want, err)
	}
}
