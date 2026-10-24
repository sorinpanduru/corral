package types_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// Compliance (SPEC-003 FR-11, FR-12, SPEC-001 FR-24): every input in this
// package is a synthetic constant. Failure messages print the case index and
// the input length, never the raw input. Markers are random per test and prove
// that no error text carries input.

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

func desc(i int, in string) string { return fmt.Sprintf("case %d (input length %d)", i, len(in)) }

// accept is one accepted input and the value Parse must return.
type accept struct {
	in   string
	want any
}

func requireAccepted(t *testing.T, s types.Spec, cases []accept) {
	t.Helper()
	for i, c := range cases {
		got, err := s.Parse(c.in)
		if err != nil {
			t.Errorf("%s: rejected, want accepted (error class %v)", desc(i, c.in), classOf(err))
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parsed to %T %v, want %T %v", desc(i, c.in), got, got, c.want, c.want)
		}
	}
}

// requireRejected asserts every input is rejected, with an error that matches
// at least one of the sentinels in anyOf (all rejections match at least one
// sentinel), and that the error text never carries the input.
func requireRejected(t *testing.T, s types.Spec, inputs []string, anyOf ...error) {
	t.Helper()
	for i, in := range inputs {
		got, err := s.Parse(in)
		if err == nil {
			t.Errorf("%s: accepted (value %T), want rejected", desc(i, in), got)
			continue
		}
		if len(anyOf) > 0 && !matchesAny(err, anyOf) {
			t.Errorf("%s: error class %v, want one of %v", desc(i, in), classOf(err), classes(anyOf))
		}
		if in != "" && len(in) >= 3 && strings.Contains(err.Error(), in) {
			t.Errorf("%s: error text carries the input", desc(i, in))
		}
	}
}

var allSentinels = []error{
	types.ErrInvalidUTF8, types.ErrControlChar, types.ErrEmpty,
	types.ErrSyntax, types.ErrRange, types.ErrNotInSet,
}

func matchesAny(err error, set []error) bool {
	for _, s := range set {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

func classes(set []error) []string {
	var out []string
	for _, s := range set {
		out = append(out, sentinelName(s))
	}
	return out
}

func sentinelName(e error) string {
	switch e {
	case types.ErrInvalidUTF8:
		return "ErrInvalidUTF8"
	case types.ErrControlChar:
		return "ErrControlChar"
	case types.ErrEmpty:
		return "ErrEmpty"
	case types.ErrSyntax:
		return "ErrSyntax"
	case types.ErrRange:
		return "ErrRange"
	case types.ErrNotInSet:
		return "ErrNotInSet"
	}
	return "other"
}

func classOf(err error) string {
	for _, s := range allSentinels {
		if errors.Is(err, s) {
			return sentinelName(s)
		}
	}
	return "none"
}

// requireIs asserts a rejection with exactly the given sentinel.
func requireIs(t *testing.T, s types.Spec, in string, want error) {
	t.Helper()
	_, err := s.Parse(in)
	if err == nil {
		t.Errorf("input of length %d accepted, want %s", len(in), sentinelName(want))
		return
	}
	if !errors.Is(err, want) {
		t.Errorf("input of length %d: error class %s, want %s", len(in), classOf(err), sentinelName(want))
	}
}

func spec(k types.Kind) types.Spec { return types.Spec{Kind: k} }

func listOf(elem types.Kind) types.Spec { return types.Spec{Kind: types.KindList, Elem: elem} }

// allKinds are the scalar kinds with a minimal well-formed spec each.
func allScalarSpecs() map[string]types.Spec {
	return map[string]types.Spec{
		"string":   spec(types.KindString),
		"text":     spec(types.KindText),
		"bool":     spec(types.KindBool),
		"int":      spec(types.KindInt),
		"float":    spec(types.KindFloat),
		"duration": spec(types.KindDuration),
		"enum":     {Kind: types.KindEnum, EnumValues: []string{"json", "text"}},
		"hostport": spec(types.KindHostPort),
		"list":     listOf(types.KindString),
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
