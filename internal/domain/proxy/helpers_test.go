package proxy_test

// Interface assumptions beyond the plan's text (stated in the test-writer
// report):
//   - proxy.Handshake, HandshakeError, QueryFailure, Refuse and Forward are the
//     DecisionKind constants (the plan lists them in that order).
//   - proxy.ActionCloseNow, ActionFinishThenClose and ActionRefuseThenClose are
//     the Action values returned by Shutdown.ConnAction.
//   - Shutdown has Force() (second signal) and DelayExpired() <-chan struct{}.
//   - proxy.Refusal is a comparable value type.

import (
	"errors"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

var t0 = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

func params() upstream.Params {
	return upstream.Params{MaxBsonObjectSize: 16777216, MaxMessageSizeBytes: 48000000, MaxWriteBatchSize: 100000,
		LogicalSessionTimeoutMinutes: 30, HasLSTM: true, MinWire: 0, MaxWire: 21}
}

// view is a HandshakeView with a fixed answer.
type view struct {
	p      upstream.Params
	detail string
}

func (v view) HandshakeAdmission() (upstream.Params, string) { return v.p, v.detail }

var okView = view{p: params()}

func msg(command string, flags uint32) wire.Request {
	return wire.Request{Kind: wire.KindOpMsg, RequestID: 1, Flags: flags, Command: command}
}

func hs(name string, h wire.Handshake, flags uint32) wire.Request {
	h.Name = name
	return wire.Request{Kind: wire.KindOpMsg, RequestID: 1, Flags: flags, Command: name, Handshake: &h}
}

func hsQuery(name string, h wire.Handshake) wire.Request {
	h.Name = name
	h.ViaOpQuery = true
	return wire.Request{Kind: wire.KindOpQueryHandshake, RequestID: 1, Command: name, Handshake: &h}
}

func queryOther(command string) wire.Request {
	return wire.Request{Kind: wire.KindOpQueryOther, RequestID: 1, Command: command}
}

const moreToCome = uint32(wire.FlagMoreToCome)

func detailOf(err error) string {
	var d interface{ CorralErrorDetail() string }
	if errors.As(err, &d) {
		return d.CorralErrorDetail()
	}
	return ""
}

func decide(req wire.Request, cs *proxy.ConnState, v proxy.HandshakeView, phase proxy.Phase) proxy.Decision {
	return proxy.Decide(req, cs, v, phase)
}
