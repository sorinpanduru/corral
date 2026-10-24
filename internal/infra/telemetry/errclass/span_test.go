package errclass_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

func recordOne(t *testing.T, err error) sdktrace.ReadOnlySpan {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	_, span := tp.Tracer("errclass-test").Start(context.Background(), "synthetic span")
	errclass.RecordSpan(span, err)
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	return ended[0]
}

// AC-35 / FR-29 / FR-26: status Error with empty description, error.type and
// corral.error.detail set, no events, no exception.* attributes, no marker.
func TestRecordSpan_AC35(t *testing.T) {
	m := newMarker(t)
	for _, tc := range markedCases(m) {
		t.Run(tc.name, func(t *testing.T) {
			s := recordOne(t, tc.err)
			if s.Status().Code != codes.Error {
				t.Fatalf("status code = %v, want Error", s.Status().Code)
			}
			if s.Status().Description != "" {
				t.Fatalf("status description = %q, want empty", s.Status().Description)
			}
			if n := len(s.Events()); n != 0 {
				t.Fatalf("events = %d, want 0 (RecordError must not be called)", n)
			}
			attrs := map[string]attribute.Value{}
			for _, kv := range s.Attributes() {
				k := string(kv.Key)
				attrs[k] = kv.Value
				if strings.HasPrefix(k, "exception.") {
					t.Errorf("exception attribute %q present", k)
				}
				assertNoMarker(t, "span attribute key", m, k)
				assertNoMarker(t, "span attribute "+k, m, kv.Value.Emit())
			}
			assertNoMarker(t, "span name", m, s.Name())
			assertNoMarker(t, "status", m, s.Status().Description)
			if v, ok := attrs[attr.ErrorType]; !ok || v.AsString() != tc.class {
				t.Errorf("%s = %v, want %q", attr.ErrorType, v.Emit(), tc.class)
			}
			if v, ok := attrs[attr.ErrorDetail]; !ok || v.AsString() != tc.detail {
				t.Errorf("%s = %v, want %q", attr.ErrorDetail, v.Emit(), tc.detail)
			}
			code, hasCode := attrs[attr.DBResponseStatusCode]
			name, hasName := attrs[attr.DBResponseCodeName]
			if tc.server {
				if !hasCode || code.Type() != attribute.STRING || code.AsString() != tc.code {
					t.Errorf("%s = %v, want string %q", attr.DBResponseStatusCode, code.Emit(), tc.code)
				}
				if !hasName || name.AsString() != tc.codeName {
					t.Errorf("%s = %v, want %q", attr.DBResponseCodeName, name.Emit(), tc.codeName)
				}
			} else if hasCode || hasName {
				t.Error("server attributes present on a non-server error")
			}
			// Exact allowlist of what the helper may set (FR-23).
			allowed := map[string]bool{attr.ErrorType: true, attr.ErrorDetail: true, attr.DBResponseStatusCode: true, attr.DBResponseCodeName: true}
			for k := range attrs {
				if !allowed[k] {
					t.Errorf("helper set attribute %q outside the allowlist", k)
				}
			}
		})
	}
}

// FR-29: span and log helpers agree on class and detail for the same error.
func TestRecordSpan_MatchesClassify_FR29(t *testing.T) {
	m := newMarker(t)
	for _, tc := range markedCases(m) {
		t.Run(tc.name, func(t *testing.T) {
			c, d := errclass.Classify(tc.err)
			s := recordOne(t, tc.err)
			got := map[string]string{}
			for _, kv := range s.Attributes() {
				got[string(kv.Key)] = kv.Value.Emit()
			}
			if got[attr.ErrorType] != string(c) || got[attr.ErrorDetail] != string(d) {
				t.Fatalf("span (%s,%s) != Classify (%s,%s)", got[attr.ErrorType], got[attr.ErrorDetail], c, d)
			}
		})
	}
}
