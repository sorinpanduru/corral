package bson_test

import (
	"errors"
	"testing"

	"github.com/sorinpanduru/corral/internal/domain/bson"
)

func seedCorpus(f *testing.F) {
	for _, d := range equivalenceInputs() {
		if len(d) <= 4096 {
			f.Add(d, uint16(len(d)/2))
		}
	}
	f.Add([]byte{}, uint16(0))
	f.Add([]byte{5, 0, 0, 0, 0}, uint16(2))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0x7F, 0}, uint16(1))
	f.Add(nested(1024), uint16(500))
	f.Add(nested(1025), uint16(500))
}

// AC-7: Validate never panics, and its result is one of nil, ErrMalformed or
// ErrTooDeep.
func FuzzValidate(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, data []byte, _ uint16) {
		err := bson.Validate(data)
		if err != nil && !errors.Is(err, bson.ErrMalformed) && !errors.Is(err, bson.ErrTooDeep) {
			t.Fatalf("unexpected error kind: %v", err)
		}
	})
}

// AC-7: the Walker never panics and always agrees with Validate, however the
// input is split.
func FuzzWalker(f *testing.F) {
	seedCorpus(f)
	f.Fuzz(func(t *testing.T, data []byte, split uint16) {
		want := bson.Validate(data)
		i := 0
		if len(data) > 0 {
			i = int(split) % (len(data) + 1)
		}
		got := walkChunks(data, data[:i], data[i:])
		sameVerdict(t, "fuzz", want, got)
	})
}
