package types_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// ---- list ----

func TestList_StringElements(t *testing.T) {
	s := listOf(types.KindString)
	requireAccepted(t, s, []accept{
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a, b", []string{"a", "b"}},
		{" a ,\tb\t", []string{"a", "b"}},
		{"a ,  b ,c", []string{"a", "b", "c"}},
		{"a b,c d", []string{"a b", "c d"}}, // inner spaces kept
		{"a, b", []string{"a", " b"}},       // only ASCII space and tab are trimmed
	})
}

func TestList_RejectsEmptyElements(t *testing.T) {
	requireRejected(t, listOf(types.KindString), []string{
		"a,,b", "a,", ",a", ",", ",,", "a, ,b", "a,\t,b", "a,   ",
	}, types.ErrEmpty, types.ErrSyntax)
}

// Trimming covers ASCII space and tab only: a newline around an element is a
// control character in a string element.
func TestList_DoesNotTrimOtherWhitespace(t *testing.T) {
	requireRejected(t, listOf(types.KindString), []string{"a,\nb", "a\n,b", "a,\rb", "a,b\x0b"},
		types.ErrControlChar)
}

// AC-4 (Amendment 1): a U+00A0 at the edge of an element is kept, not
// trimmed (FR-12 trims ASCII space and tab only), so an int list with NBSP
// around its elements is rejected while the same list with ASCII spaces
// is accepted.
func TestList_NBSPAtElementEdgeIsNotTrimmed(t *testing.T) {
	ints := listOf(types.KindInt)
	requireAccepted(t, ints, []accept{{"1, 2", []int64{1, 2}}})
	requireRejected(t, ints, []string{"1,\u00a02", "\u00a01,2", "1\u00a0,2", "1,2\u00a0", "1, \u00a02"},
		types.ErrSyntax)
	// For a string list the NBSP stays part of the element.
	requireAccepted(t, listOf(types.KindString), []accept{
		{"a,\u00a0b", []string{"a", "\u00a0b"}},
		{"\u00a0a\u00a0,b", []string{"\u00a0a\u00a0", "b"}},
	})
}

func TestList_EmptyListOnlyWithAllowEmpty(t *testing.T) {
	requireIs(t, listOf(types.KindString), "", types.ErrEmpty)
	s := types.Spec{Kind: types.KindList, Elem: types.KindString, AllowEmpty: true}
	got, err := s.Parse("")
	if err != nil {
		t.Fatalf("AllowEmpty: %v", err)
	}
	if v, ok := got.([]string); !ok || len(v) != 0 {
		t.Fatalf("AllowEmpty: got %T of len %d", got, len(v))
	}
	// AllowEmpty does not allow empty elements.
	requireRejected(t, s, []string{"a,,b", ","}, types.ErrEmpty, types.ErrSyntax)
}

func TestList_TypedElements(t *testing.T) {
	requireAccepted(t, listOf(types.KindInt), []accept{
		{"1, -2,3", []int64{1, -2, 3}},
	})
	requireAccepted(t, listOf(types.KindBool), []accept{
		{"true, FALSE", []bool{true, false}},
	})
	requireAccepted(t, listOf(types.KindFloat), []accept{
		{".5, 1e-3", []float64{0.5, 0.001}},
	})
	requireAccepted(t, listOf(types.KindDuration), []accept{
		{"250ms, 1m30s", []time.Duration{250 * time.Millisecond, 90 * time.Second}},
	})
	requireAccepted(t, listOf(types.KindHostPort), []accept{
		{"127.0.0.1:1, [::1]:2", []string{"127.0.0.1:1", "[::1]:2"}},
	})
	en := types.Spec{Kind: types.KindList, Elem: types.KindEnum, EnumValues: []string{"json", "text"}}
	requireAccepted(t, en, []accept{{"JSON, text", []string{"json", "text"}}})
}

// Each element is parsed by the element type, with the element type's errors.
func TestList_ElementErrors(t *testing.T) {
	requireRejected(t, listOf(types.KindInt), []string{"1,+2", "1,x", "1,0x1", "1,9223372036854775808"},
		types.ErrSyntax, types.ErrRange)
	requireRejected(t, listOf(types.KindBool), []string{"true,1", "yes,true"}, types.ErrSyntax)
	requireRejected(t, listOf(types.KindDuration), []string{"1s,5", "0s,1s", "1s,1µs"}, types.ErrSyntax, types.ErrRange)
	requireRejected(t, listOf(types.KindHostPort), []string{"127.0.0.1:1,:2", "127.0.0.1:01"}, types.ErrSyntax, types.ErrRange, types.ErrEmpty)
	requireIs(t, listOf(types.KindString), "a,b\x00", types.ErrControlChar)
	requireIs(t, listOf(types.KindString), "a,\xff", types.ErrInvalidUTF8)
	requireIs(t, types.Spec{Kind: types.KindList, Elem: types.KindEnum, EnumValues: []string{"a"}}, "a,b", types.ErrNotInSet)
}

func TestList_ElementFlagsApply(t *testing.T) {
	s := types.Spec{Kind: types.KindList, Elem: types.KindDuration, AllowZero: true}
	requireAccepted(t, s, []accept{{"0s, 5s", []time.Duration{0, 5 * time.Second}}})
}

// ---- Spec.Check ----

func TestCheck_AcceptsWellFormedSpecs(t *testing.T) {
	good := []types.Spec{
		spec(types.KindString), spec(types.KindText), spec(types.KindBool), spec(types.KindInt),
		spec(types.KindFloat), spec(types.KindDuration), spec(types.KindHostPort),
		{Kind: types.KindString, AllowEmpty: true},
		{Kind: types.KindText, AllowEmpty: true},
		{Kind: types.KindDuration, AllowZero: true},
		{Kind: types.KindEnum, EnumValues: []string{"a", "b"}},
		{Kind: types.KindEnum, EnumValues: []string{"a", "A"}, EnumCaseSensitive: true},
		listOf(types.KindString), listOf(types.KindInt), listOf(types.KindBool), listOf(types.KindFloat),
		listOf(types.KindDuration), listOf(types.KindHostPort),
		{Kind: types.KindList, Elem: types.KindString, AllowEmpty: true},
		{Kind: types.KindList, Elem: types.KindEnum, EnumValues: []string{"a"}},
	}
	for i, s := range good {
		if err := s.Check(); err != nil {
			t.Errorf("good spec %d rejected: %v", i, err)
		}
	}
}

func TestCheck_RejectsMalformedSpecs(t *testing.T) {
	bad := map[string]types.Spec{
		"enum without values":         {Kind: types.KindEnum},
		"enum empty set":              {Kind: types.KindEnum, EnumValues: []string{}},
		"enum duplicate":              {Kind: types.KindEnum, EnumValues: []string{"a", "a"}},
		"enum case-fold duplicate":    {Kind: types.KindEnum, EnumValues: []string{"a", "A"}},
		"enum empty member":           {Kind: types.KindEnum, EnumValues: []string{"a", ""}},
		"enum values on string":       {Kind: types.KindString, EnumValues: []string{"a"}},
		"enum values on int":          {Kind: types.KindInt, EnumValues: []string{"a"}},
		"case-sensitive on string":    {Kind: types.KindString, EnumCaseSensitive: true},
		"allow-zero on string":        {Kind: types.KindString, AllowZero: true},
		"allow-zero on int":           {Kind: types.KindInt, AllowZero: true},
		"allow-empty on bool":         {Kind: types.KindBool, AllowEmpty: true},
		"allow-empty on int":          {Kind: types.KindInt, AllowEmpty: true},
		"allow-empty on duration":     {Kind: types.KindDuration, AllowEmpty: true},
		"unknown kind":                {Kind: types.Kind(100)},
		"list of list":                {Kind: types.KindList, Elem: types.KindList},
		"list of text":                {Kind: types.KindList, Elem: types.KindText},
		"list of unknown":             {Kind: types.KindList, Elem: types.Kind(100)},
		"list of enum without values": {Kind: types.KindList, Elem: types.KindEnum},
	}
	for name, s := range bad {
		if err := s.Check(); err == nil {
			t.Errorf("%s: Check() = nil, want an error", name)
		}
	}
}

// A spec that fails Check must not panic when parsing; it returns an error.
func TestParse_MalformedSpecDoesNotPanic(t *testing.T) {
	for _, s := range []types.Spec{
		{Kind: types.Kind(100)},
		{Kind: types.KindEnum},
		{Kind: types.KindList, Elem: types.KindList},
		{Kind: types.KindList, Elem: types.KindText},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse panicked for a malformed spec")
				}
			}()
			if _, err := s.Parse("x"); err == nil {
				t.Errorf("Parse on a malformed spec returned no error")
			}
		}()
	}
}

func TestSentinelsAreDistinctConstants(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range allSentinels {
		if e == nil || e.Error() == "" {
			t.Fatal("nil or empty sentinel")
		}
		if seen[e.Error()] {
			t.Errorf("two sentinels share a text")
		}
		seen[e.Error()] = true
	}
	for i, a := range allSentinels {
		for j, b := range allSentinels {
			if i != j && errors.Is(a, b) {
				t.Errorf("sentinels %d and %d are not distinct", i, j)
			}
		}
	}
}
