package proxy

import (
	"context"
	"time"
)

// CloseReason is why a client connection was closed (corral_client_connections_closed_total).
type CloseReason string

// The nine client close reasons.
const (
	CloseClientClosed    CloseReason = "client_closed"
	CloseIdleTimeout     CloseReason = "idle_timeout"
	CloseIOTimeout       CloseReason = "io_timeout"
	CloseProtocolError   CloseReason = "protocol_error"
	CloseUpstreamFailure CloseReason = "upstream_failure"
	CloseShutdown        CloseReason = "shutdown"
	CloseTLSFailure      CloseReason = "tls_failure"
	CloseLimitExceeded   CloseReason = "limit_exceeded"
	CloseInternalError   CloseReason = "internal_error"
)

// RejectReason is why a connection was rejected at accept time.
type RejectReason string

// RejectLimitExceeded is the only reject reason.
const RejectLimitExceeded RejectReason = "limit_exceeded"

// ConnInfo describes an accepted connection.
type ConnInfo struct {
	ID         int32
	ClientAddr string
	ClientPort int
	TLS        bool
}

// HandshakeInfo describes an answered handshake. Metadata is raw: the caller
// sanitises it before it reaches a log or span.
type HandshakeInfo struct {
	ID       int32
	OpCode   string // op_msg or op_query
	OK       bool
	Metadata Metadata
}

// RequestResult is the end of one request.
type RequestResult struct {
	Command  string
	Outcome  Outcome
	Duration time.Duration
	Err      error
	Dropped  bool
}

// Observer receives proxy events for logs, metrics and spans. Domain code
// never calls it with message bytes.
type Observer interface {
	ConnOpened(ctx context.Context, info ConnInfo)
	ConnClosed(ctx context.Context, id int32, reason CloseReason)
	ConnRejected(ctx context.Context, reason RejectReason)
	TLSFailed(ctx context.Context, err error)
	HandshakeAnswered(ctx context.Context, info HandshakeInfo)
	ClientAuthRejected(ctx context.Context, id int32, r Refusal)
	CommandBlocked(ctx context.Context, id int32, command string)
	ClientProtocolError(ctx context.Context, id int32, err error)
	ClientIOTimeout(ctx context.Context, id int32)
	RequestReceived(ctx context.Context, command string)
	RequestCompleted(ctx context.Context, res RequestResult)
	PanicRecovered(ctx context.Context, err error)
}

// Noop is an Observer that does nothing; embed it to override single methods.
type Noop struct{}

func (Noop) ConnOpened(context.Context, ConnInfo)               {}
func (Noop) ConnClosed(context.Context, int32, CloseReason)     {}
func (Noop) ConnRejected(context.Context, RejectReason)         {}
func (Noop) TLSFailed(context.Context, error)                   {}
func (Noop) HandshakeAnswered(context.Context, HandshakeInfo)   {}
func (Noop) ClientAuthRejected(context.Context, int32, Refusal) {}
func (Noop) CommandBlocked(context.Context, int32, string)      {}
func (Noop) ClientProtocolError(context.Context, int32, error)  {}
func (Noop) ClientIOTimeout(context.Context, int32)             {}
func (Noop) RequestReceived(context.Context, string)            {}
func (Noop) RequestCompleted(context.Context, RequestResult)    {}
func (Noop) PanicRecovered(context.Context, error)              {}
