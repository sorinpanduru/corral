package logging

import (
	"regexp"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

var allowedKeys = map[string]struct{}{
	attr.Time: {}, attr.Level: {}, attr.Msg: {}, attr.Component: {},
	attr.Error: {}, attr.TraceID: {}, attr.SpanID: {}, attr.Stack: {},
	attr.Setting: {}, attr.SuppressedCount: {},
	attr.DBSystemName: {}, attr.DBOperationName: {}, attr.DBNamespace: {},
	attr.DBCollectionName: {}, attr.ClientAddress: {}, attr.ClientPort: {},
	attr.ErrorType: {}, attr.ErrorDetail: {}, attr.DBResponseStatusCode: {},
	attr.DBResponseCodeName: {}, attr.ServerAddress: {}, attr.ServerPort: {},
	attr.ConnectionID: {}, attr.UpstreamConnectionID: {}, attr.RequestID: {},
	attr.SessionID: {}, attr.CursorID: {}, attr.ClientAppName: {},
	attr.ClientDriverName: {}, attr.ClientDriverVersion: {},
}

var numericKeyRE = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*_(seconds|bytes|count)$`)

// NumericKey reports whether key is a snake_case name ending in _seconds,
// _bytes or _count (FR-39). The value must be numeric; that is checked by the
// test log handler, not here.
func NumericKey(key string) bool { return numericKeyRE.MatchString(key) }

// AllowedKey reports whether a log record may carry key (FR-39): the fixed
// keys, the FR-23 keys, or a numeric key.
func AllowedKey(key string) bool {
	if _, ok := allowedKeys[key]; ok {
		return true
	}
	return NumericKey(key)
}
