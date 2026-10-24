package errclass_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

// fakeDetailedErr is a synthetic DetailedError (ADR-0016). Its text carries a
// marker to prove the classifier never reads it.
type fakeDetailedErr struct {
	detail string
	msg    string
	cause  error
}

func (e *fakeDetailedErr) Error() string             { return e.msg }
func (e *fakeDetailedErr) CorralErrorDetail() string { return e.detail }
func (e *fakeDetailedErr) Unwrap() error             { return e.cause }

// amendment3 is the SPEC-001 FR-29 table of Amendment 3 details and their
// fixed classes.
var amendment3 = []struct {
	detail errclass.Detail
	token  string
	class  errclass.Class
}{
	{errclass.DetailPoolWaitTimeout, "pool_wait_timeout", errclass.ClassTimeout},
	{errclass.DetailPoolQueueFull, "pool_queue_full", errclass.ClassRejected},
	{errclass.DetailUpstreamUnavailable, "upstream_unavailable", errclass.ClassNetwork},
	{errclass.DetailUpstreamAuthFailed, "upstream_auth_failed", errclass.ClassServer},
	{errclass.DetailUpstreamIncompatible, "upstream_incompatible", errclass.ClassConfig},
	{errclass.DetailShuttingDown, "shutting_down", errclass.ClassRejected},
	{errclass.DetailClientAuthUnsupported, "client_auth_unsupported", errclass.ClassRejected},
	{errclass.DetailCommandBlocked, "command_blocked", errclass.ClassRejected},
	{errclass.DetailHandshakeRejected, "handshake_rejected", errclass.ClassRejected},
	{errclass.DetailUnsupportedOpcode, "unsupported_opcode", errclass.ClassProtocol},
	{errclass.DetailMessageTooLarge, "message_too_large", errclass.ClassProtocol},
	{errclass.DetailMalformedMessage, "malformed_message", errclass.ClassProtocol},
	{errclass.DetailChecksumMismatch, "checksum_mismatch", errclass.ClassProtocol},
	{errclass.DetailUnexpectedResponse, "unexpected_response", errclass.ClassProtocol},
}

// SPEC-001 FR-29 (Amendment 3): the constants carry the spec tokens and the
// class constant has the value "rejected".
func TestAmendment3ConstantValues_FR29(t *testing.T) {
	if string(errclass.ClassRejected) != "rejected" {
		t.Fatalf("ClassRejected = %q", errclass.ClassRejected)
	}
	for _, tc := range amendment3 {
		if string(tc.detail) != tc.token {
			t.Errorf("detail constant = %q, want %q", tc.detail, tc.token)
		}
	}
}

// AC-41: Classes() has 7 values and Details() 28, no duplicates.
func TestEnumSizes_AC41(t *testing.T) {
	if n := len(errclass.Classes()); n != 7 {
		t.Errorf("len(Classes()) = %d, want 7", n)
	}
	if n := len(errclass.Details()); n != 28 {
		t.Errorf("len(Details()) = %d, want 28", n)
	}
	seen := map[errclass.Detail]bool{}
	for _, d := range errclass.Details() {
		if seen[d] {
			t.Errorf("duplicate detail %q", d)
		}
		seen[d] = true
	}
}

// FR-29: DetailClass returns the fixed class of each Amendment 3 detail.
func TestDetailClass_FixedClasses_FR29(t *testing.T) {
	for _, tc := range amendment3 {
		t.Run(tc.token, func(t *testing.T) {
			c, ok := errclass.DetailClass(tc.detail)
			if !ok || c != tc.class {
				t.Fatalf("DetailClass(%q) = (%q, %v), want (%q, true)", tc.detail, c, ok, tc.class)
			}
		})
	}
}

// DetailClass reports ok=false for the 14 original details (their class
// depends on the error, not the token) and for unknown values.
func TestDetailClass_NotFixed(t *testing.T) {
	orig := []errclass.Detail{
		errclass.DetailECONNREFUSED, errclass.DetailECONNRESET, errclass.DetailECONNABORTED,
		errclass.DetailETIMEDOUT, errclass.DetailEPIPE, errclass.DetailEHOSTUNREACH,
		errclass.DetailENETUNREACH, errclass.DetailEADDRINUSE, errclass.DetailDeadlineExceeded,
		errclass.DetailCanceled, errclass.DetailEOF, errclass.DetailTLSHandshake,
		errclass.DetailDNSResolution, errclass.DetailOther,
	}
	for _, d := range orig {
		if _, ok := errclass.DetailClass(d); ok {
			t.Errorf("DetailClass(%q) ok = true, want false", d)
		}
	}
	for _, d := range []errclass.Detail{"", "bogus", "Pool_Wait_Timeout", "pool_wait_timeout "} {
		if _, ok := errclass.DetailClass(d); ok {
			t.Errorf("DetailClass(%q) ok = true, want false", d)
		}
	}
}

// The fixed-class details are exactly the 14 Amendment 3 details.
func TestDetailClass_ExactlyFourteenFixed(t *testing.T) {
	n := 0
	for _, d := range errclass.Details() {
		if _, ok := errclass.DetailClass(d); ok {
			n++
		}
	}
	if n != 14 {
		t.Fatalf("fixed-class details = %d, want 14", n)
	}
}

// ADR-0016: each Amendment 3 detail via a DetailedError classifies to its
// declared (class, detail), directly and when wrapped; the text is ignored.
func TestClassify_DetailedError_ADR0016(t *testing.T) {
	m := newMarker(t)
	var cases []classCase
	for _, tc := range amendment3 {
		cases = append(cases, classCase{
			name:   tc.token,
			err:    &fakeDetailedErr{detail: tc.token, msg: "refusal " + m},
			class:  string(tc.class),
			detail: tc.token,
		})
	}
	runCases(t, cases)
}

// LogAttrs and RecordSpan agree with Classify for detailed errors and never
// add a status code to a synthesized error.
func TestDetailedError_LogAttrsAndSpan_ADR0016(t *testing.T) {
	m := newMarker(t)
	for _, tc := range amendment3 {
		t.Run(tc.token, func(t *testing.T) {
			err := &fakeDetailedErr{detail: tc.token, msg: "text " + m}
			got := attrMap(errclass.LogAttrs(err))
			if v := got[attr.Error]; v.String() != string(tc.class) {
				t.Errorf("error attr = %q, want %q", v.String(), tc.class)
			}
			if v := got[attr.ErrorDetail]; v.String() != tc.token {
				t.Errorf("detail attr = %q, want %q", v.String(), tc.token)
			}
			if _, ok := got[attr.DBResponseStatusCode]; ok {
				t.Error("synthesized error carries a status code")
			}
			span := recordOne(t, err)
			amap := map[string]string{}
			for _, kv := range span.Attributes() {
				amap[string(kv.Key)] = kv.Value.Emit()
			}
			if amap[attr.ErrorType] != string(tc.class) || amap[attr.ErrorDetail] != tc.token {
				t.Errorf("span attrs = %v", amap)
			}
			if _, ok := amap[attr.DBResponseStatusCode]; ok {
				t.Error("span carries a status code for a synthesized error")
			}
		})
	}
}

// ADR-0016: an unknown detail token is ignored and classification continues
// down the existing rules.
func TestClassify_UnknownDetailFallsThrough_ADR0016(t *testing.T) {
	reset := os.NewSyscallError("read", syscall.ECONNRESET)
	cases := []classCase{
		{"unknown alone", &fakeDetailedErr{detail: "made_up"}, "internal", "other"},
		{"empty token", &fakeDetailedErr{detail: ""}, "internal", "other"},
		{"original detail token is not accepted", &fakeDetailedErr{detail: "ECONNRESET"}, "internal", "other"},
		{"unknown over errno", &fakeDetailedErr{detail: "made_up", cause: &net.OpError{Op: "read", Net: "tcp", Err: reset}}, "network", "ECONNRESET"},
		{"unknown over eof", &fakeDetailedErr{detail: "nope", cause: io.EOF}, "network", "eof"},
		{"unknown over deadline", &fakeDetailedErr{detail: "nope", cause: context.DeadlineExceeded}, "timeout", "deadline_exceeded"},
		{"unknown over server", &fakeDetailedErr{detail: "nope", cause: &fakeServerErr{code: 13, name: "Unauthorized"}}, "server", "other"},
		{"unknown over protocol", &fakeDetailedErr{detail: "nope", cause: &fakeProtocolErr{msg: "x"}}, "protocol", "other"},
	}
	runCases(t, cases)
}

// A DetailedError wrapping a net.OpError still gives the detailed pair, and
// so does one found deeper in the chain.
func TestClassify_DetailedBeatsNetwork_ADR0016(t *testing.T) {
	op := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []classCase{
		{"detailed wraps net error", &fakeDetailedErr{detail: "upstream_unavailable", cause: op}, "network", "upstream_unavailable"},
		{"detailed wraps deadline", &fakeDetailedErr{detail: "pool_wait_timeout", cause: context.DeadlineExceeded}, "timeout", "pool_wait_timeout"},
		{"detailed wraps canceled", &fakeDetailedErr{detail: "shutting_down", cause: context.Canceled}, "rejected", "shutting_down"},
		{"net error wraps detailed", fmt.Errorf("x: %w", &net.OpError{Op: "read", Net: "tcp", Err: &fakeDetailedErr{detail: "malformed_message"}}), "protocol", "malformed_message"},
		{"joined", errors.Join(io.EOF, &fakeDetailedErr{detail: "pool_queue_full"}), "rejected", "pool_queue_full"},
	}
	runCases(t, cases)
}

// ADR-0016 precedence: panic > detailed > config > server > protocol.
func TestClassify_DetailedPrecedence_ADR0016(t *testing.T) {
	det := func(d string, cause error) error { return &fakeDetailedErr{detail: d, cause: cause} }
	cfg := &fakeConfigErr{setting: "upstream.hosts", cause: errors.New("x")}
	srv := &fakeServerErr{code: 18, name: "AuthenticationFailed", msg: "text"}
	proto := &fakeProtocolErr{msg: "p"}
	cases := []classCase{
		{"detailed over config", det("pool_queue_full", cfg), "rejected", "pool_queue_full"},
		{"config sibling of detailed", errors.Join(cfg, det("pool_queue_full", nil)), "rejected", "pool_queue_full"},
		{"detailed over protocol", det("command_blocked", proto), "rejected", "command_blocked"},
		{"detailed over server", det("unexpected_response", srv), "protocol", "unexpected_response"},
		{"upstream_incompatible is config without ConfigError", det("upstream_incompatible", nil), "config", "upstream_incompatible"},
	}
	runCases(t, cases)

	t.Run("panic over detailed", func(t *testing.T) {
		err := errors.Join(det("pool_queue_full", nil), errclass.FromPanic("v", []byte("stack")))
		c, d := errclass.Classify(err)
		if c != errclass.ClassInternal || d != errclass.DetailOther {
			t.Fatalf("Classify = (%s, %s), want (internal, other)", c, d)
		}
	})
}

// AC-41 and ADR-0016: upstream_auth_failed wrapping a ServerError carries the
// status code and code name in LogAttrs and RecordSpan.
func TestUpstreamAuthFailed_WithServerError_AC41(t *testing.T) {
	m := newMarker(t)
	err := &fakeDetailedErr{
		detail: "upstream_auth_failed",
		msg:    "auth failed " + m,
		cause:  &fakeServerErr{code: 18, name: "AuthenticationFailed", msg: "errmsg " + m},
	}
	c, d := errclass.Classify(err)
	if c != errclass.ClassServer || d != errclass.DetailUpstreamAuthFailed {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
	got := attrMap(errclass.LogAttrs(err))
	if got[attr.DBResponseStatusCode].String() != "18" {
		t.Errorf("log status code = %q, want 18", got[attr.DBResponseStatusCode].String())
	}
	if got[attr.DBResponseCodeName].String() != "AuthenticationFailed" {
		t.Errorf("log code name = %q", got[attr.DBResponseCodeName].String())
	}
	span := recordOne(t, err)
	amap := map[string]string{}
	for _, kv := range span.Attributes() {
		amap[string(kv.Key)] = kv.Value.Emit()
	}
	if amap[attr.DBResponseStatusCode] != "18" || amap[attr.DBResponseCodeName] != "AuthenticationFailed" {
		t.Errorf("span attrs = %v", amap)
	}
	if amap[attr.ErrorType] != "server" || amap[attr.ErrorDetail] != "upstream_auth_failed" {
		t.Errorf("span class/detail = %v", amap)
	}
}

// AC-41 last bullet: the same detail without a ServerError node has class
// server but no status code and no code name, in logs and spans.
func TestUpstreamAuthFailed_WithoutServerError_AC41(t *testing.T) {
	err := &fakeDetailedErr{detail: "upstream_auth_failed", msg: "signature mismatch"}
	got := attrMap(errclass.LogAttrs(err))
	if got[attr.Error].String() != "server" || got[attr.ErrorDetail].String() != "upstream_auth_failed" {
		t.Fatalf("log class/detail = %v", got)
	}
	for _, k := range []string{attr.DBResponseStatusCode, attr.DBResponseCodeName} {
		if _, ok := got[k]; ok {
			t.Errorf("log carries %q for a synthesized server error", k)
		}
	}
	span := recordOne(t, err)
	for _, kv := range span.Attributes() {
		k := string(kv.Key)
		if k == attr.DBResponseStatusCode || k == attr.DBResponseCodeName {
			t.Errorf("span carries %q for a synthesized server error", k)
		}
	}
}

// Server attributes need class server: a protocol-class detailed error that
// wraps a ServerError gets none.
func TestNonServerDetailWrappingServerError_NoServerAttrs(t *testing.T) {
	err := &fakeDetailedErr{detail: "unexpected_response", cause: &fakeServerErr{code: 11000, name: "DuplicateKey"}}
	got := attrMap(errclass.LogAttrs(err))
	if _, ok := got[attr.DBResponseStatusCode]; ok {
		t.Error("protocol-class error carries a status code")
	}
}

// Existing contract: the ProtocolError marker alone stays protocol/other.
func TestProtocolMarkerStillProtocolOther(t *testing.T) {
	c, d := errclass.Classify(&fakeProtocolErr{msg: "x"})
	if c != errclass.ClassProtocol || d != errclass.DetailOther {
		t.Fatalf("Classify = (%s, %s)", c, d)
	}
}

// DetailedError is a usable interface: the fake satisfies it.
var _ errclass.DetailedError = (*fakeDetailedErr)(nil)
