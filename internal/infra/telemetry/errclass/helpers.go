package errclass

import (
	"errors"
	"log/slog"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

// serverAttrs returns the server code and code name when err is a typed
// server error. Compliance: values come from the typed accessors only, never
// from error text.
func serverAttrs(err error) (code, name string, ok bool) {
	var se ServerError
	if err != nil && errors.As(err, &se) {
		return strconv.FormatInt(int64(se.ServerCode()), 10), se.ServerCodeName(), true
	}
	return "", "", false
}

// LogAttrs returns the log attributes for err: error=<class>,
// corral.error.detail=<detail>, server attributes for server errors and the
// stack for recovered panics. It never uses err.Error().
func LogAttrs(err error) []slog.Attr {
	c, d := Classify(err)
	attrs := []slog.Attr{
		slog.String(attr.Error, string(c)),
		slog.String(attr.ErrorDetail, string(d)),
	}
	if code, name, ok := serverAttrs(err); ok {
		attrs = append(attrs,
			slog.String(attr.DBResponseStatusCode, code),
			slog.String(attr.DBResponseCodeName, name))
	}
	if st, ok := Stack(err); ok {
		attrs = append(attrs, slog.String(attr.Stack, string(st)))
	}
	return attrs
}

// RecordSpan marks span as failed: status Error with an empty description,
// error.type, corral.error.detail and server attributes for server errors.
// It never calls RecordError and adds no events (FR-26).
func RecordSpan(span trace.Span, err error) {
	if span == nil {
		return
	}
	c, d := Classify(err)
	kvs := []attribute.KeyValue{
		attribute.String(attr.ErrorType, string(c)),
		attribute.String(attr.ErrorDetail, string(d)),
	}
	if code, name, ok := serverAttrs(err); ok {
		kvs = append(kvs,
			attribute.String(attr.DBResponseStatusCode, code),
			attribute.String(attr.DBResponseCodeName, name))
	}
	span.SetStatus(codes.Error, "")
	span.SetAttributes(kvs...)
}
