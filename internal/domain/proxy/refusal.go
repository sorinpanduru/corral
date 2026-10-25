package proxy

import (
	"time"

	"github.com/sorinpanduru/corral/internal/domain/bson"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Refusal is one row of the synthesized-error table (FR-39, Interfaces). It is
// a comparable value and an error with constant text; it never wraps an
// upstream error (ADR-0016).
type Refusal struct{ id uint8 }

type refusalRow struct {
	code          int32
	name, errmsg  string
	detail, class string
	labels        bool
}

var rows = [...]refusalRow{
	0:  {},
	1:  {262, "ExceededTimeLimit", "Timed out waiting for an upstream connection.", "pool_wait_timeout", "timeout", true},
	2:  {262, "ExceededTimeLimit", "Too many requests are waiting for an upstream connection.", "pool_queue_full", "rejected", true},
	3:  {6, "HostUnreachable", "The upstream MongoDB deployment is not available.", "upstream_unavailable", "network", true},
	4:  {6, "HostUnreachable", "The upstream MongoDB deployment rejected corral's credentials.", "upstream_auth_failed", "server", true},
	5:  {6, "HostUnreachable", "The upstream MongoDB deployment is not compatible with this proxy.", "upstream_incompatible", "config", true},
	6:  {91, "ShutdownInProgress", "The proxy is shutting down.", "shutting_down", "rejected", true},
	7:  {18, "AuthenticationFailed", "Clients do not authenticate to this proxy.", "client_auth_unsupported", "rejected", false},
	8:  {115, "CommandNotSupported", "This command is not supported by this proxy.", "client_auth_unsupported", "rejected", false},
	9:  {13, "Unauthorized", "This command is not allowed through this proxy.", "command_blocked", "rejected", false},
	10: {352, "UnsupportedOpQueryCommand", "Only the handshake is supported over OP_QUERY.", "unsupported_opcode", "protocol", false},
	11: {186, "ClientMetadataCannotBeMutated", "The client metadata document may only be sent once per connection.", "handshake_rejected", "rejected", false},
	12: {72, "InvalidOptions", "Load-balanced mode is not supported by this proxy.", "handshake_rejected", "rejected", false},
}

// The refusals.
var (
	PoolWaitTimeout      = Refusal{1}
	PoolQueueFull        = Refusal{2}
	UpstreamUnavailable  = Refusal{3}
	UpstreamAuthFailed   = Refusal{4}
	UpstreamIncompatible = Refusal{5}
	ShuttingDown         = Refusal{6}
	ClientAuthSASL       = Refusal{7}
	ClientAuthOther      = Refusal{8}
	CommandBlocked       = Refusal{9}
	UnsupportedOpQuery   = Refusal{10}
	MetadataRepeated     = Refusal{11}
	LoadBalanced         = Refusal{12}
)

func (r Refusal) row() refusalRow {
	if int(r.id) >= len(rows) {
		return refusalRow{}
	}
	return rows[r.id]
}

// Code is the MongoDB error code.
func (r Refusal) Code() int32 { return r.row().code }

// CodeName is the MongoDB code name.
func (r Refusal) CodeName() string { return r.row().name }

// Errmsg is the fixed message text (no runtime data).
func (r Refusal) Errmsg() string { return r.row().errmsg }

// Detail is the SPEC-001 FR-29 detail.
func (r Refusal) Detail() string { return r.row().detail }

// Class is the SPEC-001 FR-29 class.
func (r Refusal) Class() string { return r.row().class }

// CorralErrorDetail implements the errclass.DetailedError contract.
func (r Refusal) CorralErrorDetail() string { return r.row().detail }

// Error is constant text.
func (r Refusal) Error() string { return "proxy: refused: " + r.row().detail }

// RefusalForDetail maps the pool and upstream checkout details to their
// refusals, and nothing else.
func RefusalForDetail(detail string) (Refusal, bool) {
	for id := 1; id <= 6; id++ {
		if rows[id].detail == detail {
			return Refusal{uint8(id)}, true
		}
	}
	return Refusal{}, false
}

// Labels returns the FR-39 error labels for a synthesized error. Only codes
// 262, 6 and 91 carry labels.
func Labels(r Refusal, req wire.Request) []string {
	if !r.row().labels {
		return nil
	}
	cmd := req.Command
	if cmd == "commitTransaction" || cmd == "abortTransaction" {
		return []string{"RetryableWriteError"}
	}
	if req.AutocommitFalse {
		return []string{"TransientTransactionError"}
	}
	if req.HasTxnNumber {
		return []string{"RetryableWriteError"}
	}
	return nil
}

// ErrorBody builds the reply document of a synthesized error: ok 0, code,
// codeName, errmsg, and errorLabels when there are any.
func ErrorBody(r Refusal, labels []string) []byte {
	b := bson.NewBuilder()
	b.AppendDouble("ok", 0)
	b.AppendInt32("code", r.Code())
	b.AppendString("codeName", r.CodeName())
	b.AppendString("errmsg", r.Errmsg())
	if len(labels) > 0 {
		b.StartArray("errorLabels")
		for i, l := range labels {
			b.AppendString(itoa(i), l)
		}
		b.EndArray()
	}
	return b.Bytes()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d [8]byte
	n := len(d)
	for i > 0 {
		n--
		d[n] = byte('0' + i%10)
		i /= 10
	}
	return string(d[n:])
}

// QueryFailureDoc is the OP_REPLY document for a non-handshake OP_QUERY
// (FR-8): $err, code 352, codeName, ok 0.
func QueryFailureDoc() []byte {
	r := UnsupportedOpQuery
	b := bson.NewBuilder()
	b.AppendString("$err", r.Errmsg())
	b.AppendInt32("code", r.Code())
	b.AppendString("codeName", r.CodeName())
	b.AppendDouble("ok", 0)
	return b.Bytes()
}

// HandshakeReply builds the FR-14 reply: exactly the listed fields, from the
// upstream parameters, with maxWireVersion capped at 25.
func HandshakeReply(h wire.Handshake, p upstream.Params, connID int32, now time.Time) []byte {
	b := bson.NewBuilder()
	if h.Name == "hello" {
		b.AppendBool("isWritablePrimary", true)
	} else {
		b.AppendBool("ismaster", true)
	}
	if h.HelloOK {
		b.AppendBool("helloOk", true)
	}
	b.AppendString("msg", "isdbgrid")
	b.AppendInt32("maxBsonObjectSize", p.MaxBsonObjectSize)
	b.AppendInt32("maxMessageSizeBytes", p.MaxMessageSizeBytes)
	b.AppendInt32("maxWriteBatchSize", p.MaxWriteBatchSize)
	b.AppendInt32("logicalSessionTimeoutMinutes", p.LogicalSessionTimeoutMinutes)
	b.AppendInt32("minWireVersion", p.MinWire)
	maxWire := p.MaxWire
	if maxWire > upstream.WireMax {
		maxWire = upstream.WireMax
	}
	b.AppendInt32("maxWireVersion", maxWire)
	b.AppendDateTime("localTime", now)
	b.AppendInt32("connectionId", connID)
	b.AppendBool("readOnly", false)
	b.AppendDouble("ok", 1)
	return b.Bytes()
}

// Outcome is the FR-48 request outcome.
type Outcome int

const (
	OutcomeOK Outcome = iota
	OutcomeServerError
	OutcomeProxyError
)

func (o Outcome) String() string {
	switch o {
	case OutcomeServerError:
		return "server_error"
	case OutcomeProxyError:
		return "proxy_error"
	}
	return "ok"
}

// OutcomeForReply classifies a forwarded reply: ok 0 is a server error.
func OutcomeForReply(f wire.ReplyFields) Outcome {
	if f.HasOK && f.OK != 1 {
		return OutcomeServerError
	}
	return OutcomeOK
}
