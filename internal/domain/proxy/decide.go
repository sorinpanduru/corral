package proxy

import (
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// Phase is the shutdown phase seen by the per-message decision.
type Phase int

const (
	// PhaseServing is normal service (including the listener-close delay).
	PhaseServing Phase = iota
	// PhaseDraining is reserved for engines that distinguish the drain; Decide
	// treats it like PhaseServing.
	PhaseDraining
	// PhaseLate is after FR-47 step 3: a message read now gets code 91.
	PhaseLate
)

// DecisionKind is what to do with a message.
type DecisionKind int

const (
	// Handshake: answer with HandshakeReply.
	Handshake DecisionKind = iota
	// HandshakeError: answer the handshake with Refusal.
	HandshakeError
	// QueryFailure: answer an OP_QUERY with the 352 failure document.
	QueryFailure
	// Refuse: answer with the synthesized error Refusal.
	Refuse
	// Forward: lease an upstream connection and forward.
	Forward
)

// ConnState is the per-connection state Decide reads and updates.
type ConnState struct {
	// MetadataSeen is set by the first handshake command read on the
	// connection, whatever its outcome (FR-16).
	MetadataSeen bool
}

// HandshakeView gives the FR-19 handshake availability.
type HandshakeView interface {
	HandshakeAdmission() (upstream.Params, string)
}

// Metadata is a handshake's client metadata, raw (the caller sanitises it).
type Metadata struct {
	Has                                bool
	AppName, DriverName, DriverVersion string
}

// Decision is the outcome of Decide.
type Decision struct {
	Kind    DecisionKind
	Refusal Refusal
	// NoReply: send nothing back (FR-59, moreToCome).
	NoReply bool
	// Blocked and ClientAuth say which counter a refusal belongs to.
	Blocked, ClientAuth bool
	// Params are the upstream parameters for Kind Handshake.
	Params upstream.Params
	// Metadata is the handshake's client metadata, kept for logs and spans
	// even when the handshake was refused.
	Metadata Metadata
}

var blockedCommands = []string{"currentOp", "killOp", "killAllSessions", "killAllSessionsByPattern"}

func isBlocked(req wire.Request) bool {
	for _, c := range blockedCommands {
		if req.Command == c {
			return true
		}
	}
	if req.Command == "killSessions" && req.KillSessionsEmpty {
		return true // an empty list kills every session, like killAllSessions
	}
	return req.Command == "aggregate" && req.FirstStage == "$currentOp"
}

// Decide applies the FR-61 handling order. The first rule that applies wins:
// shutdown, handshake commands, client auth commands, blocked commands,
// non-handshake OP_QUERY, otherwise forward. Matching is exact and
// case-sensitive. It changes only cs.MetadataSeen.
func Decide(req wire.Request, cs *ConnState, hv HandshakeView, phase Phase) Decision {
	noReplyIf := func(d Decision) Decision {
		if req.Kind == wire.KindOpMsg && req.Flags&wire.FlagMoreToCome != 0 {
			d.NoReply = true
		}
		return d
	}
	if phase == PhaseLate {
		return noReplyIf(Decision{Kind: Refuse, Refusal: ShuttingDown})
	}
	if h := req.Handshake; h != nil {
		seen := cs.MetadataSeen
		cs.MetadataSeen = true
		d := Decision{}
		if h.HasClient {
			d.Metadata = Metadata{Has: true, AppName: h.AppName, DriverName: h.DriverName, DriverVersion: h.DriverVersion}
		}
		switch {
		case h.LoadBalanced:
			d.Kind, d.Refusal = HandshakeError, LoadBalanced
		case h.HasClient && seen:
			d.Kind, d.Refusal = HandshakeError, MetadataRepeated
		default:
			p, detail := hv.HandshakeAdmission()
			if detail != "" {
				r, ok := RefusalForDetail(detail)
				if !ok {
					r = UpstreamUnavailable
				}
				d.Kind, d.Refusal = HandshakeError, r
			} else {
				d.Kind, d.Params = Handshake, p
			}
		}
		return noReplyIf(d)
	}
	if req.Kind == wire.KindOpMsg {
		switch req.Command {
		case "saslStart", "saslContinue", "authenticate":
			return noReplyIf(Decision{Kind: Refuse, Refusal: ClientAuthSASL, ClientAuth: true})
		case "getnonce", "logout":
			return noReplyIf(Decision{Kind: Refuse, Refusal: ClientAuthOther, ClientAuth: true})
		}
		if isBlocked(req) {
			return noReplyIf(Decision{Kind: Refuse, Refusal: CommandBlocked, Blocked: true})
		}
	}
	if req.Kind == wire.KindOpQueryOther {
		return Decision{Kind: QueryFailure, Refusal: UnsupportedOpQuery}
	}
	return Decision{Kind: Forward}
}
