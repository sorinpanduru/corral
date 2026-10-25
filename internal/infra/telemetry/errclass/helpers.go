package errclass

import (
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

// stackLimit caps the stack attribute (bytes).
const stackLimit = 32 * 1024

// serverAttrs returns the server code and, when valid, the code name of a
// typed server error. Compliance: values come from the typed accessors only,
// never from error text, and the name is emitted only if it matches
// ^[A-Za-z0-9_]{1,64}$ (name is "" otherwise; the code is still returned).
func serverAttrs(nodes []error) (code, name string, ok bool) {
	for _, n := range nodes {
		if se, is := n.(ServerError); is {
			name = se.ServerCodeName()
			if !validCodeName(name) {
				name = ""
			}
			name = strings.Clone(name) // never alias memory owned by the error
			return strconv.FormatInt(int64(se.ServerCode()), 10), name, true
		}
	}
	return "", "", false
}

func validCodeName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// capStack limits s to stackLimit bytes, cutting at a line boundary when one
// exists within the last 256 bytes of the budget, else at a rune boundary.
func capStack(s string) string {
	if len(s) <= stackLimit {
		return s
	}
	if i := strings.LastIndexByte(s[:stackLimit], '\n'); i >= stackLimit-256 {
		return s[:i+1]
	}
	cut := stackLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// LogAttrs returns the log attributes for err: error=<class>,
// corral.error.detail=<detail>, server attributes for server errors and the
// stack (capped at 32 KiB) for recovered panics. It never uses err.Error().
// LogAttrs(nil) returns nil.
func LogAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	nodes := walk(err)
	c, d := classify(nodes)
	attrs := []slog.Attr{
		slog.String(attr.Error, string(c)),
		slog.String(attr.ErrorDetail, string(d)),
	}
	if c == ClassServer {
		if code, name, ok := serverAttrs(nodes); ok {
			attrs = append(attrs, slog.String(attr.DBResponseStatusCode, code))
			if name != "" {
				attrs = append(attrs, slog.String(attr.DBResponseCodeName, name))
			}
		}
	}
	if st, ok := Stack(err); ok {
		attrs = append(attrs, slog.String(attr.Stack, capStack(string(st))))
	}
	return attrs
}

// RecordSpan marks span as failed: status Error with an empty description,
// error.type, corral.error.detail and server attributes for server errors.
// It never calls RecordError and adds no events (FR-26). RecordSpan(span,
// nil) is a no-op.
func RecordSpan(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	nodes := walk(err)
	c, d := classify(nodes)
	kvs := []attribute.KeyValue{
		attribute.String(attr.ErrorType, string(c)),
		attribute.String(attr.ErrorDetail, string(d)),
	}
	if c == ClassServer {
		if code, name, ok := serverAttrs(nodes); ok {
			kvs = append(kvs, attribute.String(attr.DBResponseStatusCode, code))
			if name != "" {
				kvs = append(kvs, attribute.String(attr.DBResponseCodeName, name))
			}
		}
	}
	span.SetStatus(codes.Error, "")
	span.SetAttributes(kvs...)
}
