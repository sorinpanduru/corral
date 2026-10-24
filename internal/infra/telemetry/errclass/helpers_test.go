package errclass_test

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
)

// newMarker returns a random synthetic marker, unique per call. It stands in
// for FR-24 data (userinfo, errmsg values, panic values). Synthetic only.
func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// fakeServerErr is a synthetic MongoDB server-reply error. It satisfies the
// duck-typed errclass.ServerError contract without any domain type.
type fakeServerErr struct {
	code int32
	name string
	msg  string // plays the role of errmsg and carries a marker
}

func (e *fakeServerErr) Error() string          { return e.msg }
func (e *fakeServerErr) ServerCode() int32      { return e.code }
func (e *fakeServerErr) ServerCodeName() string { return e.name }

// fakeConfigErr satisfies the duck-typed errclass.ConfigError contract.
type fakeConfigErr struct {
	setting string
	cause   error
}

func (e *fakeConfigErr) Error() string         { return "config error for " + e.setting }
func (e *fakeConfigErr) Unwrap() error         { return e.cause }
func (e *fakeConfigErr) ConfigSetting() string { return e.setting }

// timeoutErr is a net.Error-style timeout with no errno or context sentinel.
type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

func attrMap(attrs []slog.Attr) map[string]slog.Value {
	m := make(map[string]slog.Value, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value.Resolve()
	}
	return m
}

// valueText renders an attribute value (any kind) for marker scanning.
func valueText(v slog.Value) string { return v.String() }

func assertNoMarker(tb testing.TB, where, marker, text string) {
	tb.Helper()
	if strings.Contains(text, marker) {
		tb.Fatalf("marker leaked into %s", where)
	}
}
