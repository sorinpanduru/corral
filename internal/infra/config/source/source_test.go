package source_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// Compliance (SPEC-003 FR-7, FR-25): synthetic entries only. The snapshot
// holds names and values, so failure messages print names and lengths, never
// values.

// FR-7: first occurrence of a key wins (matches os.LookupEnv).
func TestEnv_FirstOccurrenceWins(t *testing.T) {
	e := source.NewEnv([]string{"A=first", "B=1", "A=second", "A=third"})
	v, ok := e.Lookup("A")
	if !ok || v != "first" {
		t.Fatalf("Lookup(A) = (len %d, %v), want the first occurrence", len(v), ok)
	}
	// The first occurrence wins even when it is empty.
	e = source.NewEnv([]string{"A=", "A=x"})
	v, ok = e.Lookup("A")
	if !ok || v != "" {
		t.Fatalf("Lookup(A) = (len %d, %v), want (empty, true)", len(v), ok)
	}
}

// FR-7: entries without "=" are ignored.
func TestEnv_EntryWithoutEqualsIgnored(t *testing.T) {
	e := source.NewEnv([]string{"NOEQUALS", "B=1"})
	if _, ok := e.Lookup("NOEQUALS"); ok {
		t.Fatal("entry without = was found")
	}
	if got := e.Names(); !reflect.DeepEqual(got, []string{"B"}) {
		t.Fatalf("Names() = %v, want [B]", got)
	}
	// An ignored entry does not shadow or block a later real one.
	e = source.NewEnv([]string{"A", "A=1"})
	if v, ok := e.Lookup("A"); !ok || v != "1" {
		t.Fatalf("Lookup(A) = (len %d, %v), want the later A=1", len(v), ok)
	}
}

// Only the first "=" splits: the value keeps any later "=".
func TestEnv_ValueMayContainEquals(t *testing.T) {
	e := source.NewEnv([]string{"A=b=c", "B==", "C=x=y=z"})
	for name, want := range map[string]string{"A": "b=c", "B": "=", "C": "x=y=z"} {
		if v, ok := e.Lookup(name); !ok || v != want {
			t.Errorf("Lookup(%s) = (len %d, %v), want len %d", name, len(v), ok, len(want))
		}
	}
}

// Assumption (minimal shape): the empty key (`=x`) is never found and never
// listed, as with os.LookupEnv("").
func TestEnv_EmptyKeyNeverFound(t *testing.T) {
	e := source.NewEnv([]string{"=x", "A=1"})
	if v, ok := e.Lookup(""); ok {
		t.Fatalf("Lookup(\"\") found a value of length %d", len(v))
	}
	if got := e.Names(); !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("Names() = %v, want [A]", got)
	}
}

// FR-7: key match is case-sensitive.
func TestEnv_CaseSensitive(t *testing.T) {
	e := source.NewEnv([]string{"Corral_X=1", "CORRAL_X=2"})
	if v, ok := e.Lookup("CORRAL_X"); !ok || v != "2" {
		t.Fatalf("Lookup(CORRAL_X) wrong")
	}
	if v, ok := e.Lookup("Corral_X"); !ok || v != "1" {
		t.Fatalf("Lookup(Corral_X) wrong")
	}
	if _, ok := e.Lookup("corral_x"); ok {
		t.Fatal("lookup is not case-sensitive")
	}
}

// FR-7: values are returned as given, with no trimming; the empty rule
// ("empty means unset") is source.IsSet's job, not Lookup's.
func TestEnv_LookupDoesNotTrimOrFilter(t *testing.T) {
	e := source.NewEnv([]string{"A= x ", "B=", "C=   ", "D=\t"})
	if v, ok := e.Lookup("A"); !ok || v != " x " {
		t.Errorf("A: len %d ok %v, want untrimmed", len(v), ok)
	}
	for _, k := range []string{"B", "C", "D"} {
		if _, ok := e.Lookup(k); !ok {
			t.Errorf("%s: empty or blank value must still be found by Lookup", k)
		}
	}
	if v, _ := e.Lookup("C"); v != "   " {
		t.Errorf("C: value was altered")
	}
}

func TestEnv_Missing(t *testing.T) {
	e := source.NewEnv([]string{"A=1"})
	if v, ok := e.Lookup("B"); ok || v != "" {
		t.Fatalf("Lookup(B) = (len %d, %v)", len(v), ok)
	}
}

// FR-8: Names lists distinct keys in first-occurrence order.
func TestEnv_NamesDistinctInFirstOccurrenceOrder(t *testing.T) {
	e := source.NewEnv([]string{"Z=1", "A=2", "Z=3", "NOEQ", "M=4", "A=5", "=junk", "CORRAL_Q="})
	want := []string{"Z", "A", "M", "CORRAL_Q"}
	if got := e.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestEnv_EmptyAndNil(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		e := source.NewEnv(in)
		if n := len(e.Names()); n != 0 {
			t.Fatalf("Names() has %d entries, want 0", n)
		}
		if _, ok := e.Lookup("A"); ok {
			t.Fatal("found a key in an empty environment")
		}
	}
}

// The snapshot is immutable: later changes to the input slice or to the slice
// returned by Names do not leak in (deterministic loading, FR-25).
func TestEnv_Snapshot(t *testing.T) {
	in := []string{"A=1", "B=2"}
	e := source.NewEnv(in)
	in[0] = "A=changed"
	in[1] = "C=3"
	if v, _ := e.Lookup("A"); v != "1" {
		t.Error("NewEnv aliases its input slice")
	}
	if _, ok := e.Lookup("C"); ok {
		t.Error("NewEnv aliases its input slice")
	}
	names := e.Names()
	names[0] = "TAMPERED"
	if got := e.Names(); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("Names() exposes internal state: %v", got)
	}
}

// Env is usable as a value (zero value must not panic).
func TestEnv_ZeroValue(t *testing.T) {
	var e source.Env
	if _, ok := e.Lookup("A"); ok {
		t.Fatal("zero Env found a key")
	}
	if len(e.Names()) != 0 {
		t.Fatal("zero Env has names")
	}
}

// FR-7 / SPEC-001 FR-41: "set" means strings.TrimSpace(v) != "".
func TestIsSet(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false}, {" ", false}, {"  ", false}, {"\t", false}, {"\n", false}, {"\r\n", false},
		{" \t\n ", false}, {" ", false}, {"\u0085", false}, {" ", false},
		{" a ", true}, {"a", true}, {"0", true}, {"false", true}, {"\x00", true}, {"_", true},
	}
	for i, c := range cases {
		if got := source.IsSet(c.v); got != c.want {
			t.Errorf("case %d (length %d): IsSet = %v, want %v", i, len(c.v), got, c.want)
		}
		if want := strings.TrimSpace(c.v) != ""; source.IsSet(c.v) != want {
			t.Errorf("case %d: IsSet disagrees with TrimSpace rule", i)
		}
	}
}
