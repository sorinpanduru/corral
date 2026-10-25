// Package upstream is the domain model of the MongoDB upstream deployment as
// corral sees it: known members and discovery, the check schedule, primary
// selection, compatibility, the authentication state, readiness and
// admission (SPEC-002 FR-18, FR-19, FR-22, FR-35, FR-37, FR-46, FR-56;
// ADR-0019).
//
// It imports the standard library and other domain packages only. Checks run
// through the Checker port, time through the clock port. State changes are
// applied under a mutex; observers and change callbacks run after the mutex
// is released, so they may call back into the Monitor.
package upstream

import (
	"context"
	"errors"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/pool"
)

// Fixed constants.
const (
	// MinHeartbeat is the SDAM minimum between checks of one member.
	MinHeartbeat = 500 * time.Millisecond
	// WireMin and WireMax are the accepted wire-version range (7.0 to 8.0).
	WireMin = 21
	WireMax = 25
)

// Config is the monitor configuration.
type Config struct {
	Seeds             []string // upstream.hosts, in order
	ReplicaSet        string
	HasReplicaSet     bool // false: single-mongos mode
	HeartbeatInterval time.Duration
	Authenticate      bool // mechanism is not "none"
	// Rand supplies jitter in [0,1) for the authentication backoff; nil uses
	// math/rand.
	Rand func() float64
}

// Params are the upstream parameters read from a hello reply.
type Params struct {
	MaxBsonObjectSize            int32
	MaxMessageSizeBytes          int32
	MaxWriteBatchSize            int32
	LogicalSessionTimeoutMinutes int32
	HasLSTM                      bool
	MinWire                      int32
	MaxWire                      int32
}

// Report is the result of one check of one member.
type Report struct {
	Host string
	Err  error
	// AuthErr is the outcome of authentication on that check, separate from
	// Err (Amendment 1, G-8). nil means success or not attempted.
	AuthErr error

	IsWritablePrimary bool
	Secondary         bool
	ArbiterOnly       bool
	IsMongos          bool
	// IsReplicaSet is hello's isreplicaset: true without a setName marks a
	// ghost member (starting up, restarting or not yet initiated).
	IsReplicaSet bool
	// Transient says a failed check (Err) was a network, TLS or timeout error.
	// Only transient failures of the target are retried once (FR-35). The
	// checker classifies; the domain does not inspect transport errors.
	Transient       bool
	SetName         string
	HasSetName      bool
	Hosts, Passives []string
	Primary         string
	ElectionID      [12]byte
	HasElectionID   bool
	SetVersion      int64
	HasSetVersion   bool
	Params          Params
}

// CheckRequest asks the Checker for one check.
type CheckRequest struct {
	Host         string
	Authenticate bool
	Fresh        bool // use a new monitoring connection
}

// Checker runs one check. It must honour ctx and return a Report with Err set
// on failure.
type Checker interface {
	Check(ctx context.Context, req CheckRequest) Report
	// CloseHost closes the monitoring connection to a host that left the
	// known set (FR-37), with reason pool.CloseIdle.
	CloseHost(host string, reason pool.CloseReason)
}

// AuthFailed is an authentication failure that counts for the authentication
// state (FR-22): codes 18 and 334, or a failed server signature.
type AuthFailed struct{ Err error }

func (e *AuthFailed) Error() string { return "upstream authentication failed" }

// Unwrap returns the cause.
func (e *AuthFailed) Unwrap() error { return e.Err }

// CorralErrorDetail implements the errclass.DetailedError contract.
func (e *AuthFailed) CorralErrorDetail() string { return "upstream_auth_failed" }

// IsAuthFailed reports whether err is, or wraps, an AuthFailed.
func IsAuthFailed(err error) bool {
	var a *AuthFailed
	return errors.As(err, &a)
}

// Reason is a FR-18 incompatibility token.
type Reason string

// Incompatibility reasons.
const (
	ReasonWireVersion            Reason = "wire_version"
	ReasonStandalone             Reason = "standalone"
	ReasonTopologyMismatch       Reason = "topology_mismatch"
	ReasonReplicaSetNameMismatch Reason = "replica_set_name_mismatch"
)

// State is a snapshot of what the monitor knows.
type State struct {
	Reached        bool
	Incompatible   bool
	IncompatReason Reason
	IncompatHost   string
	AuthFailing    bool
	Target         string
	HasTarget      bool
	Params         Params
	HasParams      bool
}

// WireAccepted is the FR-18 acceptance rule: the server's range overlaps
// WireMin to WireMax.
func WireAccepted(min, max int32) bool { return max >= WireMin && min <= WireMax }

// EvaluateReadiness is FR-46 as a pure function: ready when reached,
// compatible, a target is known and authentication is not failing; otherwise
// exactly one reason by the precedence incompatible, auth failed, unavailable.
func EvaluateReadiness(s State) health.Status {
	if s.Reached && !s.Incompatible && s.HasTarget && !s.AuthFailing {
		return health.Status{Ready: true}
	}
	switch {
	case s.Incompatible:
		return health.Status{Reason: health.ReasonUpstreamIncompatible}
	case s.AuthFailing:
		return health.Status{Reason: health.ReasonUpstreamAuthFailed}
	}
	return health.Status{Reason: health.ReasonUpstreamUnavailable}
}

// AdmissionKind is the FR-56 decision for a request.
type AdmissionKind int

const (
	// Ready: a lease may be given.
	Ready AdmissionKind = iota
	// Wait: queue the request (transient unavailability).
	Wait
	// FailFast: refuse at once with code 6 and Detail.
	FailFast
)

// Admission is the FR-56 decision with its target and detail.
type Admission struct {
	Kind   AdmissionKind
	Target string
	Detail string
}

func evaluateAdmission(s State) Admission {
	switch {
	case s.Incompatible:
		return Admission{Kind: FailFast, Detail: "upstream_incompatible"}
	case s.AuthFailing:
		return Admission{Kind: FailFast, Detail: "upstream_auth_failed"}
	case !s.Reached:
		return Admission{Kind: FailFast, Detail: "upstream_unavailable"}
	case !s.HasTarget:
		return Admission{Kind: Wait}
	}
	return Admission{Kind: Ready, Target: s.Target}
}

// ReplySource says where a reply code came from.
type ReplySource int

const (
	// Forwarded is a reply forwarded for a client request.
	Forwarded ReplySource = iota
	// Barrier is the reply to corral's own barrier ping.
	Barrier
)

// ReplyCodeInfo carries the FR-9 reply codes. writeErrors are never read.
type ReplyCodeInfo struct {
	HasTop  bool
	OKZero  bool
	Top     int32
	TopName string
	HasWCE  bool
	WCE     int32
}

// ChangeKind classifies a Change.
type ChangeKind int

const (
	TargetChanged ChangeKind = iota
	HostNotPrimary
	HostUnknown
	BecameIncompatible
	AvailabilityChanged
)

// Change is reported to the OnChange callbacks so the application can drive
// the pool (wake waiters, close hosts, abort barriers).
type Change struct {
	Kind   ChangeKind
	Host   string
	Reason string
}

// Observer receives monitor events for logs and metrics. Callbacks run
// outside the monitor's lock.
type Observer interface {
	AvailabilityChanged(ctx context.Context, ready bool, reason health.Reason)
	Incompatible(ctx context.Context, reason Reason, host string)
	Available(ctx context.Context)
	PrimaryChanged(ctx context.Context, host string)
	TargetMarkedNotPrimary(ctx context.Context, host string, code int32, codeName string)
	MonitorConnOpened(ctx context.Context, host string, id uint64)
	MonitorConnClosed(ctx context.Context, host string, reason string)
	MonitorConnFailed(ctx context.Context, host string, err error)
	AuthSucceeded(ctx context.Context, kind, host string)
	AuthFailed(ctx context.Context, kind, host string, err error)
}

// Noop is an Observer that does nothing. Embed it to override single methods.
type Noop struct{}

func (Noop) AvailabilityChanged(context.Context, bool, health.Reason)      {}
func (Noop) Incompatible(context.Context, Reason, string)                  {}
func (Noop) Available(context.Context)                                     {}
func (Noop) PrimaryChanged(context.Context, string)                        {}
func (Noop) TargetMarkedNotPrimary(context.Context, string, int32, string) {}
func (Noop) MonitorConnOpened(context.Context, string, uint64)             {}
func (Noop) MonitorConnClosed(context.Context, string, string)             {}
func (Noop) MonitorConnFailed(context.Context, string, error)              {}
func (Noop) AuthSucceeded(context.Context, string, string)                 {}
func (Noop) AuthFailed(context.Context, string, string, error)             {}
