// Package health defines the domain readiness port (SPEC-001 FR-18).
//
// It holds domain types only and imports the standard library only (FR-1).
package health

import "context"

// Reason is a fixed, low-cardinality token explaining why corral is not ready.
type Reason string

const (
	// ReasonStarting means startup has not completed.
	ReasonStarting Reason = "starting"
	// ReasonShuttingDown means graceful shutdown has begun.
	ReasonShuttingDown Reason = "shutting_down"
	// ReasonUpstreamUnavailable means no upstream server is reachable.
	ReasonUpstreamUnavailable Reason = "upstream_unavailable"
	// ReasonUpstreamAuthFailed means upstream authentication failed.
	ReasonUpstreamAuthFailed Reason = "upstream_auth_failed"
	// ReasonUpstreamIncompatible means the upstream server is incompatible.
	ReasonUpstreamIncompatible Reason = "upstream_incompatible"
	// ReasonUnknown stands for any reason outside the defined set.
	ReasonUnknown Reason = "unknown"
)

// Reasons returns the full defined reason set. The caller owns the slice.
func Reasons() []Reason {
	return []Reason{ReasonStarting, ReasonShuttingDown, ReasonUpstreamUnavailable,
		ReasonUpstreamAuthFailed, ReasonUpstreamIncompatible, ReasonUnknown}
}

// Status is a readiness report. The zero value is not ready.
type Status struct {
	Ready  bool
	Reason Reason
}

// Readiness is the port through which the domain reports readiness.
type Readiness interface {
	Readiness(ctx context.Context) Status
}
