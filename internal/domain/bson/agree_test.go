package bson_test

import (
	"math/rand"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// Review finding F2: for duplicate keys at any level the Walker must pick the
// same element as Lookup (the first occurrence at every level of the path).

func scalarString(t *testing.T, e bson.Element) (string, bool) {
	switch e.Type() {
	case bson.Type(0x03), bson.Type(0x04):
		return "", false
	}
	if v, ok := e.Int32(); ok {
		return "i32:" + itoa(int64(v)), true
	}
	if v, ok := e.Int64(); ok {
		return "i64:" + itoa(v), true
	}
	if v, ok := e.Bool(); ok {
		if v {
			return "bool:1", true
		}
		return "bool:0", true
	}
	if v, ok := e.StringValue(); ok {
		return "str:" + v, true
	}
	return "other", true
}

func valueString(v bson.Value) string {
	if x, ok := v.Int32(); ok {
		return "i32:" + itoa(int64(x))
	}
	if x, ok := v.Int64(); ok {
		return "i64:" + itoa(x)
	}
	if x, ok := v.Bool(); ok {
		if x {
			return "bool:1"
		}
		return "bool:0"
	}
	if x, ok := v.StringValue(); ok {
		return "str:" + x
	}
	return "other"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// agree compares presence and scalar value for one path on one valid document.
func agree(t testing.TB, d []byte, path []string, chunk int) {
	t.Helper()
	le, found, err := bson.Lookup(d, path...)
	if err != nil {
		t.Fatalf("Lookup error on a valid document: %v", err)
	}
	// Presence capture.
	wp := bson.NewWalker([]bson.Capture{{Path: path, Mode: bson.CapturePresence}, {Path: path, Mode: bson.CaptureValue}})
	for off := 0; off < len(d); off += chunk {
		end := off + chunk
		if end > len(d) {
			end = len(d)
		}
		if err := wp.Write(d[off:end]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := wp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, pres := wp.Captured(0)
	if pres != found {
		t.Fatalf("path %v: Walker presence=%v, Lookup found=%v (chunk %d)", path, pres, found, chunk)
	}
	if !found {
		if _, ok := wp.Captured(1); ok {
			t.Fatalf("path %v: Walker value captured but Lookup found nothing", path)
		}
		return
	}
	if want, scalar := scalarString(nil, le); scalar {
		v, ok := wp.Captured(1)
		if !ok {
			t.Fatalf("path %v: scalar not captured though Lookup found it", path)
		}
		if got := valueString(v); got != want && !v.TooLong {
			t.Fatalf("path %v: Walker value %q, Lookup value %q", path, got, want)
		}
	}
}

// F2 examples: duplicate intermediate keys, both orders, scalar first or last.
func TestWalkerAgreesWithLookupOnDuplicateIntermediates_F2(t *testing.T) {
	cases := map[string]struct {
		d     []byte
		path  []string
		found bool
	}{
		"first container lacks the key":      {doc(eDoc("cursor", doc(eInt32("x", 1))), eDoc("cursor", doc(eInt64("id", 7)))), []string{"cursor", "id"}, false},
		"first container has the key":        {doc(eDoc("cursor", doc(eInt64("id", 7))), eDoc("cursor", doc(eInt32("x", 1)))), []string{"cursor", "id"}, true},
		"both containers have different ids": {doc(eDoc("cursor", doc(eInt64("id", 1))), eDoc("cursor", doc(eInt64("id", 2)))), []string{"cursor", "id"}, true},
		"scalar then container":              {doc(eInt32("cursor", 5), eDoc("cursor", doc(eInt64("id", 7)))), []string{"cursor", "id"}, false},
		"container then scalar":              {doc(eDoc("cursor", doc(eInt64("id", 7))), eInt32("cursor", 5)), []string{"cursor", "id"}, true},
		"null then container":                {doc(eNull("cursor"), eDoc("cursor", doc(eInt64("id", 7)))), []string{"cursor", "id"}, false},
		"array then document":                {doc(eArray("a", eInt32("0", 1)), eDoc("a", doc(eInt32("0", 9)))), []string{"a", "0"}, true},
		"document then array":                {doc(eDoc("a", doc(eInt32("x", 1))), eArray("a", eInt32("0", 9))), []string{"a", "0"}, false},
		"three levels, middle duplicated":    {doc(eDoc("a", doc(eDoc("b", doc(eInt32("c", 1))), eDoc("b", doc(eInt32("d", 2))))), eDoc("a", doc(eDoc("b", doc(eInt32("d", 3)))))), []string{"a", "b", "d"}, false},
		"three levels, first complete":       {doc(eDoc("a", doc(eDoc("b", doc(eInt32("d", 2))), eDoc("b", doc(eInt32("c", 1))))), eDoc("a", doc(eDoc("b", doc(eInt32("c", 9)))))), []string{"a", "b", "d"}, true},
		"leaf duplicated":                    {doc(eDoc("cursor", doc(eInt64("id", 1), eInt64("id", 2)))), []string{"cursor", "id"}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, found, err := bson.Lookup(tc.d, tc.path...)
			if err != nil || found != tc.found {
				t.Fatalf("Lookup found=%v err=%v, want found=%v (first-wins at every level)", found, err, tc.found)
			}
			for _, chunk := range []int{1, 3, 8, len(tc.d)} {
				agree(t, tc.d, tc.path, chunk)
			}
			w := runCaptures(t, tc.d, []bson.Capture{{Path: tc.path, Mode: bson.CapturePresence}}, 5)
			if _, ok := w.Captured(0); ok != tc.found {
				t.Fatalf("Walker presence = %v, want %v", ok, tc.found)
			}
		})
	}
}

// The concrete example from the review: {cursor:{x:1}, cursor:{id:7}}.
func TestWalkerDoesNotFindIdInSecondCursor_F2(t *testing.T) {
	d := doc(eDoc("cursor", doc(eInt32("x", 1))), eDoc("cursor", doc(eInt64("id", 7))))
	if _, ok, _ := bson.Lookup(d, "cursor", "id"); ok {
		t.Fatal("Lookup found cursor.id in the second cursor")
	}
	w := runCaptures(t, d, []bson.Capture{val("cursor", "id")}, 4)
	if _, ok := w.Captured(0); ok {
		t.Fatal("Walker captured cursor.id from the second cursor (not first-wins)")
	}
}

// randDoc builds a random valid document over a tiny key alphabet so
// duplicates at every level are common.
func randDoc(r *rand.Rand, depth int) []byte {
	keys := []string{"a", "b", "c", "0", "1"}
	n := r.Intn(5)
	var elems [][]byte
	for i := 0; i < n; i++ {
		k := keys[r.Intn(len(keys))]
		switch c := r.Intn(8); {
		case c < 2 && depth < 4:
			elems = append(elems, eDoc(k, randDoc(r, depth+1)))
		case c == 2 && depth < 4:
			elems = append(elems, el(0x04, k, randDoc(r, depth+1)))
		case c == 3:
			elems = append(elems, eInt32(k, int32(r.Intn(100))))
		case c == 4:
			elems = append(elems, eInt64(k, int64(r.Intn(100))))
		case c == 5:
			elems = append(elems, eString(k, string(rune('p'+r.Intn(5)))))
		case c == 6:
			elems = append(elems, eBool(k, r.Intn(2) == 0))
		default:
			elems = append(elems, eNull(k))
		}
	}
	return doc(elems...)
}

func allPaths() [][]string {
	var out [][]string
	ks := []string{"a", "b", "c", "0", "1"}
	for _, a := range ks {
		out = append(out, []string{a})
		for _, b := range ks {
			out = append(out, []string{a, b})
			for _, c := range ks {
				out = append(out, []string{a, b, c})
			}
		}
	}
	return out
}

// Randomised agreement: 1,500 documents x every path up to three segments,
// under two chunkings.
func TestWalkerAgreesWithLookupRandom_F2(t *testing.T) {
	r := rand.New(rand.NewSource(20261024))
	paths := allPaths()
	for i := 0; i < 1500; i++ {
		d := randDoc(r, 0)
		for _, p := range paths {
			agree(t, d, p, len(d)+1)
		}
		// A second chunking on a sample of paths.
		for j := 0; j < 5; j++ {
			agree(t, d, paths[r.Intn(len(paths))], 1+r.Intn(7))
		}
	}
}
