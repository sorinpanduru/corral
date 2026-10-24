package errclass_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

type markedCase struct {
	name       string
	err        error
	class      string
	detail     string
	server     bool
	code       string
	codeName   string
	panicStack bool
}

// markedCases covers every class the spec lists for AC-35 that has a
// constructible error, each carrying a marker where FR-24 data could leak.
func markedCases(m string) []markedCase {
	u := "http://user:" + m + "@collector.invalid:4318/v1/traces"
	return []markedCase{
		{name: "exporter url error", err: &url.Error{Op: "Post", URL: u, Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}, class: "network", detail: "ECONNREFUSED"},
		{name: "exporter opaque", err: fmt.Errorf("export to %s failed: %w", u, errors.New("opaque "+m)), class: "internal", detail: "other"},
		{name: "net.OpError", err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, class: "network", detail: "ECONNRESET"},
		{name: "text with marker around timeout", err: fmt.Errorf("%s: %w", m, context.DeadlineExceeded), class: "timeout", detail: "deadline_exceeded"},
		{name: "server reply", err: &fakeServerErr{code: 11000, name: "DuplicateKey", msg: "E11000 duplicate key { a: \"" + m + "\" }"}, class: "server", detail: "other", server: true, code: "11000", codeName: "DuplicateKey"},
		{name: "protocol", err: &fakeProtocolErr{msg: "collector rejected: " + m}, class: "protocol", detail: "other"},
		{name: "protocol wrapped", err: fmt.Errorf("export %s: %w", m, &fakeProtocolErr{msg: "bad reply " + m}), class: "protocol", detail: "other"},
		{name: "config", err: &fakeConfigErr{setting: "admin.listen", cause: errors.New("value " + m)}, class: "config", detail: "other"},
		{name: "panic", err: errclass.FromPanic(m, []byte("goroutine 1 [running]:\nsynthetic")), class: "internal", detail: "other", panicStack: true},
	}
}

// AC-35 / FR-29: LogAttrs yields error=<class>, corral.error.detail=<detail>,
// uses only FR-39 allowlisted keys, no groups, and never the marker.
func TestLogAttrs_AC35(t *testing.T) {
	m := newMarker(t)
	for _, tc := range markedCases(m) {
		t.Run(tc.name, func(t *testing.T) {
			attrs := errclass.LogAttrs(tc.err)
			got := attrMap(attrs)
			if v, ok := got[attr.Error]; !ok || v.Kind() != slog.KindString || v.String() != tc.class {
				t.Fatalf("error attr = %v (present=%v), want %q", v, ok, tc.class)
			}
			if v, ok := got[attr.ErrorDetail]; !ok || v.Kind() != slog.KindString || v.String() != tc.detail {
				t.Fatalf("%s attr = %v (present=%v), want %q", attr.ErrorDetail, v, ok, tc.detail)
			}
			for _, a := range attrs {
				if !logging.AllowedKey(a.Key) {
					t.Errorf("key %q is outside the FR-39 allowlist", a.Key)
				}
				if a.Value.Resolve().Kind() == slog.KindGroup {
					t.Errorf("attribute %q is a group", a.Key)
				}
				assertNoMarker(t, "attr key "+a.Key, m, a.Key)
				assertNoMarker(t, "attr value of "+a.Key, m, valueText(a.Value))
			}
			// Detail value comes from the enum.
			found := false
			for _, d := range errclass.Details() {
				found = found || string(d) == got[attr.ErrorDetail].String()
			}
			if !found {
				t.Errorf("detail %q not in Details()", got[attr.ErrorDetail].String())
			}
			// Server attributes only on server errors, as strings.
			code, hasCode := got[attr.DBResponseStatusCode]
			name, hasName := got[attr.DBResponseCodeName]
			if tc.server {
				if !hasCode || code.Kind() != slog.KindString || code.String() != tc.code {
					t.Errorf("%s = %v, want string %q", attr.DBResponseStatusCode, code, tc.code)
				}
				if !hasName || name.String() != tc.codeName {
					t.Errorf("%s = %v, want %q", attr.DBResponseCodeName, name, tc.codeName)
				}
			} else if hasCode || hasName {
				t.Error("server attributes present on a non-server error")
			}
			// Only these keys may appear (stack allowed for panics).
			allowed := map[string]bool{attr.Error: true, attr.ErrorDetail: true, attr.DBResponseStatusCode: true, attr.DBResponseCodeName: true, attr.Stack: true}
			for k := range got {
				if !allowed[k] {
					t.Errorf("unexpected key %q", k)
				}
			}
			if _, has := got[attr.Stack]; has && !tc.panicStack {
				t.Error("stack attribute present on a non-panic error")
			}
		})
	}
}

// FR-29: a stack attribute, when present for a panic, is a string.
func TestLogAttrs_PanicStackType_FR29(t *testing.T) {
	got := attrMap(errclass.LogAttrs(errclass.FromPanic("v", []byte("goroutine 1 [running]:\nsynthetic"))))
	if v, ok := got[attr.Stack]; ok && v.Kind() != slog.KindString {
		t.Fatalf("stack kind = %v, want string", v.Kind())
	}
}

// AC-35 end to end through the real logging handler: the written record has
// error=<class>, the detail, and no marker anywhere in the output.
func TestLogAttrs_ThroughHandler_NoMarker_AC35(t *testing.T) {
	m := newMarker(t)
	for _, format := range []logging.Format{logging.FormatJSON, logging.FormatText} {
		for _, tc := range markedCases(m) {
			t.Run(fmt.Sprintf("%v/%s", format, tc.name), func(t *testing.T) {
				var buf bytes.Buffer
				f := logging.NewFactory(logging.NewHandler(&buf, format, slog.LevelDebug))
				l, err := f.Logger(logging.Config)
				if err != nil {
					t.Fatal(err)
				}
				l.LogAttrs(context.Background(), slog.LevelError, "synthetic failure", errclass.LogAttrs(tc.err)...)
				out := buf.String()
				assertNoMarker(t, "log output", m, out)
				if format == logging.FormatJSON {
					var rec map[string]any
					if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
						t.Fatalf("not JSON: %v", err)
					}
					if rec[attr.Error] != tc.class {
						t.Errorf("error = %v, want %q", rec[attr.Error], tc.class)
					}
					if rec[attr.ErrorDetail] != tc.detail {
						t.Errorf("%s = %v, want %q", attr.ErrorDetail, rec[attr.ErrorDetail], tc.detail)
					}
					if tc.server && rec[attr.DBResponseStatusCode] != tc.code {
						t.Errorf("%s = %v, want %q", attr.DBResponseStatusCode, rec[attr.DBResponseStatusCode], tc.code)
					}
				}
			})
		}
	}
}

// FR-29: an error that is not a server error never gets server attributes,
// even when its text looks like one.
func TestLogAttrs_NoServerAttrsForLookalike(t *testing.T) {
	got := attrMap(errclass.LogAttrs(errors.New("code 11000 DuplicateKey")))
	if _, ok := got[attr.DBResponseStatusCode]; ok {
		t.Fatal("status code derived from error text")
	}
	if _, ok := got[attr.DBResponseCodeName]; ok {
		t.Fatal("code name derived from error text")
	}
}
