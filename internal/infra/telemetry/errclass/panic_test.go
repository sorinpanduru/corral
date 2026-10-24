package errclass_test

import (
	"errors"
	"syscall"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

// FR-29: FromPanic returns an error whose Error() is a constant that never
// reflects the panic value (FR-24: recovered panic values are never emitted).
func TestFromPanic_ErrorIsConstant_FR29(t *testing.T) {
	m1, m2 := newMarker(t), newMarker(t)
	e1 := errclass.FromPanic(m1, nil)
	e2 := errclass.FromPanic(m2, []byte("synthetic stack"))
	if e1 == nil || e2 == nil {
		t.Fatal("FromPanic returned nil")
	}
	assertNoMarker(t, "Error() of string panic", m1, e1.Error())
	assertNoMarker(t, "Error() of string panic", m2, e2.Error())
	if e1.Error() != e2.Error() {
		t.Fatalf("Error() differs between panics: %q vs %q", e1.Error(), e2.Error())
	}
	if e1.Error() == "" {
		t.Fatal("Error() is empty")
	}
}

// Panic values of any type are discarded: an error value is not kept in the
// chain, and fmt-style values do not leak.
func TestFromPanic_DiscardsValue_FR29(t *testing.T) {
	m := newMarker(t)
	inner := errors.New("boom " + m)
	err := errclass.FromPanic(inner, nil)
	assertNoMarker(t, "Error()", m, err.Error())
	if errors.Is(err, inner) {
		t.Fatal("panic error value is reachable through the error chain")
	}
	if errors.Is(errclass.FromPanic(syscall.ECONNREFUSED, nil), syscall.ECONNREFUSED) {
		t.Fatal("errno panic value is reachable through the error chain")
	}
	for _, v := range []any{m, 42, struct{ S string }{m}, &struct{ S string }{m}, []string{m}, nil} {
		e := errclass.FromPanic(v, nil)
		if e == nil {
			t.Fatalf("FromPanic(%T) returned nil", v)
		}
		assertNoMarker(t, "Error()", m, e.Error())
	}
}

// FR-29: the stack is optional and is kept when supplied.
func TestStack_Optional_FR29(t *testing.T) {
	stack := []byte("goroutine 1 [running]:\nsynthetic.frame()")
	got, ok := errclass.Stack(errclass.FromPanic("v", stack))
	if !ok || string(got) != string(stack) {
		t.Fatalf("Stack = (%q, %v)", got, ok)
	}
	if _, ok := errclass.Stack(errclass.FromPanic("v", nil)); ok {
		t.Fatal("Stack reported ok with no stack supplied")
	}
	if _, ok := errclass.Stack(errors.New("plain")); ok {
		t.Fatal("Stack reported ok for a non-panic error")
	}
}
