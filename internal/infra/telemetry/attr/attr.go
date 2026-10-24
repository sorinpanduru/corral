// Package attr holds the string constants for every allowed log attribute key
// and every FR-23 span attribute key (SPEC-001 FR-23, FR-39). The allowlist
// exists once here; logging, errclass, tracing and the log checker use it.
//
// Compliance (FR-24): no constant here names query text, credentials or other
// forbidden data. db.query.text is deliberately absent.
package attr

// FR-39 fixed log keys.
const (
	Time      = "time"
	Level     = "level"
	Msg       = "msg"
	Component = "component"
	Error     = "error"
	TraceID   = "trace_id"
	SpanID    = "span_id"
	Stack     = "stack"
	Setting   = "setting"
)

// Numeric count key used by rate-limited sources (FR-28, FR-33).
const SuppressedCount = "suppressed_count"

// FR-23 required attributes (also used as log keys, FR-39). ErrorDetail is
// the FR-29 key in both spans and logs.
const (
	DBSystemName         = "db.system.name"
	DBOperationName      = "db.operation.name"
	DBNamespace          = "db.namespace"
	DBCollectionName     = "db.collection.name"
	ClientAddress        = "client.address"
	ClientPort           = "client.port"
	ErrorType            = "error.type"
	ErrorDetail          = "corral.error.detail"
	DBResponseStatusCode = "db.response.status_code"
	DBResponseCodeName   = "corral.db.response.code_name"
)

// FR-23 optional attributes.
const (
	ServerAddress        = "server.address"
	ServerPort           = "server.port"
	ConnectionID         = "corral.connection.id"
	UpstreamConnectionID = "corral.upstream.connection.id"
	RequestID            = "corral.request.id"
	SessionID            = "corral.session.id"
	CursorID             = "corral.cursor.id"
	ClientAppName        = "corral.client.app_name"
	ClientDriverName     = "corral.client.driver.name"
	ClientDriverVersion  = "corral.client.driver.version"
)
