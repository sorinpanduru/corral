package bson_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

// Regression for the 32-bit overflow in payload sizing (review finding F1).
// A declared length near MaxInt32 must be rejected as malformed on every
// platform: on a 32-bit int, `4 + length` wraps negative and a naive bounds
// check passes. No input may panic. Run on 32-bit with:
//
//	GOARCH=386 go test ./internal/domain/bson
//
// The cases are valid Go on 64-bit too, where they guard the same bound.

// bigLens are declared lengths that overflow int32 arithmetic when a small
// constant is added.
var bigLens = []int32{0x7FFFFFFF, 0x7FFFFFFE, 0x7FFFFFFD, 0x7FFFFFFC, 0x7FFFFFFB, 0x7FFFFFF0, 0x7FFFFF00, 0x40000000}

func overflowDocs() map[string][]byte {
	out := map[string][]byte{}
	for _, l := range bigLens {
		tag := string(rune('a' + int(l&0xF)))
		out["string "+tag+hex32(l)] = doc(el(0x02, "a", cat(le32(l), []byte("xy\x00"))))
		out["javascript "+hex32(l)] = doc(el(0x0D, "a", cat(le32(l), []byte("xy\x00"))))
		out["symbol "+hex32(l)] = doc(el(0x0E, "a", cat(le32(l), []byte("xy\x00"))))
		out["dbpointer "+hex32(l)] = doc(el(0x0C, "a", cat(le32(l), []byte("xy\x00"), make([]byte, 12))))
		out["binary "+hex32(l)] = doc(el(0x05, "a", cat(le32(l), []byte{0, 1, 2, 3})))
		out["document "+hex32(l)] = doc(el(0x03, "a", cat(le32(l), []byte{0, 0, 0, 0, 0})))
		out["array "+hex32(l)] = doc(el(0x04, "a", cat(le32(l), []byte{0, 0, 0, 0, 0})))
		out["codews "+hex32(l)] = doc(el(0x0F, "a", cat(le32(l), strPayload("x"), doc())))
		out["codews string "+hex32(l)] = doc(el(0x0F, "a", cat(le32(20), le32(l), []byte("x\x00"), doc())))
		out["top-level length "+hex32(l)] = cat(le32(l), []byte{0x10, 'a', 0, 1, 0, 0, 0, 0})
		out["nested in doc "+hex32(l)] = doc(eDoc("o", doc(el(0x02, "s", cat(le32(l), []byte("x\x00"))))))
	}
	// Negative lengths too (the high bit set).
	for _, l := range []int32{-1, -2, -4, -5, -0x7FFFFFFF, -0x80000000} {
		out["neg string "+hex32(l)] = doc(el(0x02, "a", cat(le32(l), []byte("xy\x00"))))
		out["neg binary "+hex32(l)] = doc(el(0x05, "a", cat(le32(l), []byte{0, 1})))
		out["neg doc "+hex32(l)] = doc(el(0x03, "a", cat(le32(l), []byte{0, 0, 0, 0, 0})))
		out["neg codews "+hex32(l)] = doc(el(0x0F, "a", cat(le32(l), strPayload("x"), doc())))
	}
	return out
}

func hex32(l int32) string {
	const d = "0123456789abcdef"
	u := uint32(l)
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = d[u&0xF]
		u >>= 4
	}
	return string(b)
}

func noPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked on oversize length: %v", name, r)
		}
	}()
	f()
}

func TestOversizeLengthsRejected_F1(t *testing.T) {
	for name, d := range overflowDocs() {
		t.Run(name, func(t *testing.T) {
			noPanic(t, "Validate", func() {
				if err := bson.Validate(d); !errors.Is(err, bson.ErrMalformed) {
					t.Fatalf("Validate: %v, want ErrMalformed", err)
				}
			})
			noPanic(t, "Walker", func() {
				w := bson.NewWalker([]bson.Capture{{Path: []string{"a"}, Mode: bson.CaptureValue}})
				err := w.Write(d)
				if err == nil {
					err = w.Close()
				}
				if !errors.Is(err, bson.ErrMalformed) {
					t.Fatalf("Walker: %v, want ErrMalformed", err)
				}
			})
			noPanic(t, "Walker bytewise", func() {
				w := bson.NewWalker(nil)
				var err error
				for i := range d {
					if err = w.Write(d[i : i+1]); err != nil {
						break
					}
				}
				if err == nil {
					err = w.Close()
				}
				if !errors.Is(err, bson.ErrMalformed) {
					t.Fatalf("Walker bytewise: %v, want ErrMalformed", err)
				}
			})
			noPanic(t, "FirstElement", func() {
				// For "nested in doc" the first element is a well-formed container
				// whose inner element is oversize; FirstElement need not look
				// inside. Every other case puts the oversize length in the first
				// element or the header.
				if strings.HasPrefix(name, "nested in doc") {
					return
				}
				if _, err := bson.FirstElement(d); err == nil {
					t.Fatal("FirstElement accepted an oversize length")
				}
			})
			noPanic(t, "Lookup", func() {
				_, ok, err := bson.Lookup(d, "a")
				if ok && err == nil {
					t.Fatal("Lookup found an element behind an oversize length")
				}
			})
			noPanic(t, "Lookup deep", func() {
				_, _, _ = bson.Lookup(d, "o", "s")
			})
		})
	}
}

// Valid maximum-ish sizes still work: a 3 MiB string is fine, so the guard is
// not simply "reject anything large".
func TestLargeValidStillAccepted_F1(t *testing.T) {
	d := doc(eString("s", string(make([]byte, 3<<20))), eBinary("b", 0, make([]byte, 3<<20)))
	if err := bson.Validate(d); err != nil {
		t.Fatal(err)
	}
	if v, ok := mustLookup(t, d, "s").StringValue(); !ok || len(v) != 3<<20 {
		t.Fatal("large string not read back")
	}
}
