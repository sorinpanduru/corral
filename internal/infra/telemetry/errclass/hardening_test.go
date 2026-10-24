package errclass_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// Documented cap for the stack attribute (implementation constant stackLimit).
const stackCap = 32 * 1024

// D3 precedence: panic > config > server > protocol > errno/sentinels.
func TestClassify_Precedence_D3(t *testing.T) {
	m := newMarker(t)
	server := &fakeServerErr{code: 11000, name: "DuplicateKey", msg: "dup " + m}
	runCases(t, []classCase{
		{"config over server", &fakeConfigErr{setting: "admin.listen", cause: server}, "config", "other"},
		{"server over protocol", &fakeProtocolErr{msg: "p " + m, cause: server}, "server", "other"},
		{"config over protocol", &fakeConfigErr{setting: "x", cause: &fakeProtocolErr{msg: "p"}}, "config", "other"},
		{"panic over config", &fakeConfigErr{setting: "x", cause: errclass.FromPanic(m, nil)}, "internal", "other"},
	})
}

// FR-29 coverage: tls alert, pointer x509 errors, joined timeout, nil.
func TestClassify_ExtraCoverage_FR29(t *testing.T) {
	runCases(t, []classCase{
		{"tls alert", tls.AlertError(40), "network", "tls_handshake"},
		{"x509 ptr unknown authority", &x509.UnknownAuthorityError{}, "network", "tls_handshake"},
		{"x509 ptr hostname", &x509.HostnameError{Host: "x.invalid"}, "network", "tls_handshake"},
		{"x509 ptr invalid", &x509.CertificateInvalidError{Reason: x509.Expired}, "network", "tls_handshake"},
		{"join with timeout member", errors.Join(errors.New("first"), timeoutErr{msg: "t"}), "timeout", "ETIMEDOUT"},
	})
	if c, d := inEnums(t, nil); c != "internal" || d != "other" {
		t.Fatalf("Classify(nil) = (%s, %s), want (internal, other)", c, d)
	}
}

// Decision: nil handling.
func TestNilHandling(t *testing.T) {
	if got := errclass.LogAttrs(nil); len(got) != 0 {
		t.Fatalf("LogAttrs(nil) = %v, want empty", got)
	}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	errclass.RecordSpan(span, nil)
	span.End()
	s := rec.Ended()[0]
	if s.Status().Code != codes.Unset || s.Status().Description != "" {
		t.Fatalf("status = %+v, want unset", s.Status())
	}
	if len(s.Attributes()) != 0 || len(s.Events()) != 0 {
		t.Fatalf("RecordSpan(nil) changed span: attrs=%d events=%d", len(s.Attributes()), len(s.Events()))
	}
}

// Hardening: server code names must match ^[A-Za-z0-9_]{1,64}$ or the
// code-name attribute is omitted (the code number is still emitted).
func TestServerCodeName_Validated(t *testing.T) {
	m := newMarker(t)
	bad := map[string]string{
		"space":      "Dup Key " + m,
		"punct":      "Dup;" + m + "!",
		"long":       strings.Repeat("A", 65),
		"long+mark":  m + strings.Repeat("A", 64),
		"empty":      "",
		"newline":    "Dup\n" + m,
		"non-ascii":  "Dupé",
		"dash":       "Dup-Key",
		"marker-sep": "A." + m,
	}
	for name, cn := range bad {
		t.Run(name, func(t *testing.T) {
			err := &fakeServerErr{code: 11000, name: cn, msg: "errmsg " + m}
			got := attrMap(errclass.LogAttrs(err))
			if _, ok := got[attr.DBResponseCodeName]; ok {
				t.Fatalf("code name %q emitted in log attrs", cn)
			}
			if v, ok := got[attr.DBResponseStatusCode]; !ok || v.String() != "11000" {
				t.Fatalf("status code = %v (present=%v), want 11000", v, ok)
			}
			if c, d := inEnums(t, err); c != "server" || d != "other" {
				t.Fatalf("Classify = (%s, %s)", c, d)
			}
			s := recordOne(t, err)
			for _, kv := range s.Attributes() {
				if string(kv.Key) == attr.DBResponseCodeName {
					t.Fatal("code name emitted on span")
				}
				assertNoMarker(t, "span attr", m, kv.Value.Emit())
			}
			var buf bytes.Buffer
			l, _ := logging.NewFactory(logging.NewHandler(&buf, logging.FormatJSON, slog.LevelDebug)).Logger(logging.Config)
			l.LogAttrs(context.Background(), slog.LevelError, "synthetic", errclass.LogAttrs(err)...)
			assertNoMarker(t, "handler output", m, buf.String())
			if strings.Contains(buf.String(), cn) && cn != "" && cn != "A" {
				t.Fatalf("code name leaked into output")
			}
		})
	}
	// Boundary and ordinary values are emitted unchanged.
	for _, cn := range []string{"DuplicateKey", "A", "a_B_9", strings.Repeat("Z", 64)} {
		got := attrMap(errclass.LogAttrs(&fakeServerErr{code: 1, name: cn, msg: "x"}))
		if v, ok := got[attr.DBResponseCodeName]; !ok || v.String() != cn {
			t.Errorf("valid code name %q not emitted (got %v, present=%v)", cn, v, ok)
		}
		s := recordOne(t, &fakeServerErr{code: 1, name: cn, msg: "x"})
		found := false
		for _, kv := range s.Attributes() {
			if string(kv.Key) == attr.DBResponseCodeName && kv.Value.AsString() == cn {
				found = true
			}
		}
		if !found {
			t.Errorf("valid code name %q missing on span", cn)
		}
	}
}

// Hardening: the stack attr is capped at 32 KiB (stackLimit), truncated at a
// line boundary where practical.
func TestStack_Capped(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 1<<20; i++ {
		fmt.Fprintf(&sb, "synthetic.frame%d()\n\t/src/file.go:%d\n", i, i)
	}
	big := sb.String()
	st := attrMap(errclass.LogAttrs(errclass.FromPanic("v", []byte(big))))[attr.Stack]
	got := st.String()
	if st.Kind() != slog.KindString || got == "" {
		t.Fatalf("stack attr missing or not a string (kind %v)", st.Kind())
	}
	if len(got) > stackCap {
		t.Fatalf("stack attr is %d bytes, cap is %d", len(got), stackCap)
	}
	if !strings.HasPrefix(big, got) {
		t.Fatal("capped stack is not a prefix of the original")
	}
	if len(got) < stackCap-256 {
		t.Fatalf("stack truncated too aggressively: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "\n") && big[len(got)] != '\n' {
		t.Fatal("stack not truncated at a line boundary")
	}
	// A small stack is kept intact.
	small := "goroutine 1 [running]:\nsynthetic()\n"
	if v := attrMap(errclass.LogAttrs(errclass.FromPanic("v", []byte(small))))[attr.Stack]; v.String() != small {
		t.Fatalf("small stack altered: %q", v.String())
	}
}

type selfCycle struct{}

func (e *selfCycle) Error() string { return "cycle" }
func (e *selfCycle) Unwrap() error { return e }

type pairA struct{ b *pairB }
type pairB struct{ a *pairA }

func (e *pairA) Error() string { return "a" }
func (e *pairA) Unwrap() error { return e.b }
func (e *pairB) Error() string { return "b" }
func (e *pairB) Unwrap() error { return e.a }

type joinCycle struct{}

func (e *joinCycle) Error() string   { return "join cycle" }
func (e *joinCycle) Unwrap() []error { return []error{e, errors.New("leaf")} }

// Hardening: cyclic Unwrap chains must neither hang nor overflow the stack.
func TestCyclicUnwrap_Terminates(t *testing.T) {
	a := &pairA{}
	a.b = &pairB{a: a}
	cases := map[string]error{"self": &selfCycle{}, "pair": a, "join": &joinCycle{}}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan [2]string, 1)
			go func() {
				c, d := errclass.Classify(err)
				_ = errclass.LogAttrs(err)
				rec := tracetest.NewSpanRecorder()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
				_, span := tp.Tracer("t").Start(context.Background(), "s")
				errclass.RecordSpan(span, err)
				span.End()
				_ = tp.Shutdown(context.Background())
				done <- [2]string{string(c), string(d)}
			}()
			select {
			case r := <-done:
				if r[0] != "internal" || r[1] != "other" {
					t.Fatalf("Classify = (%s, %s), want (internal, other)", r[0], r[1])
				}
			case <-time.After(5 * time.Second):
				t.Fatal("classification did not terminate on a cyclic chain")
			}
		})
	}
}
