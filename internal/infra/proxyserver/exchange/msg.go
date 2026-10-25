package exchange

import (
	"context"
	"encoding/binary"
	"io"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/bson"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// msg is one client message being handled.
type msg struct {
	s         *session
	start     time.Time
	h         wire.Header
	prefix    []byte
	remaining int64 // bytes of the message not yet read from the client
	req       wire.Request
	v         *wire.MsgValidator
	cmd       string // command name once read, for accounting
}

func (x *msg) complete(o proxy.Outcome, err error, dropped bool) {
	d := &x.s.e.d
	d.Obs.RequestCompleted(x.s.ctx, proxy.RequestResult{Command: x.cmd, Outcome: o, Err: err,
		Duration: d.Clock.Now().Sub(x.start), Dropped: dropped})
}

// clientIOFail ends the exchange because reading from or writing to the client
// failed: a timeout is io_timeout, anything else is a client that went away.
func (x *msg) clientIOFail(err error) (proxy.CloseReason, bool) {
	if isTimeout(err) && x.s.ctx.Err() == nil {
		x.s.e.d.Obs.ClientIOTimeout(x.s.ctx, x.s.cc.ID)
		x.complete(proxy.OutcomeProxyError, err, false)
		return proxy.CloseIOTimeout, false
	}
	x.complete(proxy.OutcomeProxyError, err, false)
	return proxy.CloseClientClosed, false
}

// protocolFail ends the exchange because of a client protocol error.
func (x *msg) protocolFail(err error) (proxy.CloseReason, bool) {
	x.s.e.d.Obs.ClientProtocolError(x.s.ctx, x.s.cc.ID, err)
	x.complete(proxy.OutcomeProxyError, err, false)
	return proxy.CloseProtocolError, false
}

func (x *msg) run() (proxy.CloseReason, bool) {
	s := x.s
	d := &s.e.d
	if err := s.readBudget(s.buf[1:16]); err != nil {
		return x.clientIOFail(err)
	}
	x.h = wire.ParseHeader(s.buf[:16])
	if err := wire.CheckClientHeader(x.h, d.Upstream.MaxMessageSize()); err != nil {
		return x.protocolFail(err)
	}
	n := int(x.h.Length)
	if n > proxy.RequestBufferLimit {
		n = proxy.RequestBufferLimit
	}
	if got, err := s.readBudgetN(s.buf[16:n]); err != nil {
		// The client went away mid-message: label the request with the command
		// name if it had already arrived (Amendment 2, R-3).
		x.cmd = partialCommand(s.buf[:16+got])
		return x.clientIOFail(err)
	}
	x.prefix = s.buf[:n]
	x.remaining = int64(x.h.Length) - int64(n)
	if d.Probe != nil {
		d.Probe.ReadBeforeLease(s.cc.ID, n)
	}

	req, err := wire.ClassifyPrefix(x.prefix, x.h.Length)
	if err != nil {
		return x.protocolFail(err)
	}
	x.req, x.cmd = req, req.Command
	if d.PanicCommand != "" && req.Command == d.PanicCommand {
		panic("synthetic handler panic")
	}
	d.Obs.RequestReceived(s.ctx, req.Command)
	if req.Kind == wire.KindOpMsg {
		x.v = wire.NewMsgValidator(x.h, false)
		if err := x.v.Write(x.prefix[16:]); err != nil {
			return x.protocolFail(err)
		}
		if x.remaining == 0 {
			if err := x.v.Close(); err != nil {
				return x.protocolFail(err)
			}
		}
	}

	phase := proxy.PhaseServing
	if d.Shutdown != nil {
		phase = d.Shutdown.Phase()
	}
	dec := proxy.Decide(req, &s.state, d.Upstream, phase)
	if dec.Metadata.Has && !s.meta.Has {
		s.meta = dec.Metadata
	}
	if dec.Kind == proxy.Forward {
		return x.forward()
	}
	return x.local(dec, phase == proxy.PhaseLate)
}

// frame wraps a reply document for the client in the form of the request: an
// OP_MSG, or an OP_REPLY for OP_QUERY.
func (x *msg) frame(body []byte, replyFlags int32) []byte {
	s := x.s
	if x.req.Kind == wire.KindOpMsg {
		return wire.BuildMsg(s.ids.Next(), x.req.RequestID, 0, body)
	}
	return wire.BuildReply(s.ids.Next(), x.req.RequestID, replyFlags, body)
}

// drain reads and discards the rest of the message (FR-38) within the budget.
func (x *msg) drain() error {
	buf := x.s.buf
	for x.remaining > 0 {
		n := int64(len(buf))
		if n > x.remaining {
			n = x.remaining
		}
		if err := x.s.readBudget(buf[:n]); err != nil {
			return err
		}
		x.remaining -= n
	}
	return nil
}

// local answers a message without the upstream: handshakes, refusals and the
// OP_QUERY failure.
func (x *msg) local(dec proxy.Decision, closeAfter bool) (proxy.CloseReason, bool) {
	s := x.s
	d := &s.e.d
	var reply []byte
	var refusal error
	outcome := proxy.OutcomeProxyError
	op := "op_msg"
	if x.req.Kind != wire.KindOpMsg {
		op = "op_query"
	}
	switch dec.Kind {
	case proxy.Handshake:
		body := proxy.HandshakeReply(*x.req.Handshake, dec.Params, s.cc.ID, d.Clock.Now())
		reply, outcome = x.frame(body, 0), proxy.OutcomeOK
		d.Obs.HandshakeAnswered(s.ctx, proxy.HandshakeInfo{ID: s.cc.ID, OpCode: op, OK: true, Metadata: dec.Metadata})
	case proxy.HandshakeError:
		reply = x.frame(proxy.ErrorBody(dec.Refusal, proxy.Labels(dec.Refusal, x.req)), 0)
		refusal = dec.Refusal
		d.Obs.HandshakeAnswered(s.ctx, proxy.HandshakeInfo{ID: s.cc.ID, OpCode: op, OK: false, Metadata: dec.Metadata})
	case proxy.QueryFailure:
		reply = x.frame(proxy.QueryFailureDoc(), 2)
		refusal = proxy.UnsupportedOpQuery
	default: // Refuse
		reply = x.frame(proxy.ErrorBody(dec.Refusal, proxy.Labels(dec.Refusal, x.req)), 0)
		refusal = dec.Refusal
		if dec.ClientAuth {
			d.Obs.ClientAuthRejected(s.ctx, s.cc.ID, dec.Refusal)
		}
		if dec.Blocked {
			d.Obs.CommandBlocked(s.ctx, s.cc.ID, x.req.Command)
		}
	}
	if err := x.drain(); err != nil {
		return x.clientIOFail(err)
	}
	if !dec.NoReply {
		if err := s.write(reply); err != nil {
			return x.clientIOFail(err)
		}
	}
	// Spans: refusals other than client auth get a root span in error;
	// handshakes and auth commands get none (FR-50).
	if dec.Kind == proxy.QueryFailure || (dec.Kind == proxy.Refuse && !dec.ClientAuth) {
		_, ex := d.Spans.Start(s.ctx, s.rootInfo(&x.req))
		ex.End(refusal, nil)
	}
	x.complete(outcome, refusal, dec.NoReply && dec.Kind != proxy.Handshake)
	if closeAfter {
		return proxy.CloseShutdown, false
	}
	return "", true
}

// forward leases an upstream connection and carries the message to it.
func (x *msg) forward() (proxy.CloseReason, bool) {
	s := x.s
	d := &s.e.d
	ctx, ex := d.Spans.Start(s.ctx, s.rootInfo(&x.req))

	// Checkout. While waiting, a fully read message lets us watch the client
	// (FR-41): a departed client leaves the queue.
	checkCtx, cancel := context.WithCancel(ctx)
	gone := make(chan struct{})
	stop := make(chan struct{})
	watching := x.remaining == 0
	if watching {
		closed := upstreamnet.WatchClose(s.cc.Conn, stop)
		go func() {
			select {
			case <-closed:
				close(gone)
				cancel()
			case <-stop:
			}
		}()
	}
	co := ex.Checkout(ctx)
	lease, err := d.Leaser.Checkout(checkCtx)
	if watching {
		close(stop)
	}
	cancel()
	if err != nil {
		select {
		case <-gone:
			gerr := io.EOF
			co.End(nil, nil)
			ex.End(gerr, nil)
			x.complete(proxy.OutcomeProxyError, gerr, false)
			return proxy.CloseClientClosed, false
		default:
		}
		return x.checkoutFailed(co, ex, err)
	}
	co.End(nil, nil)
	s.lease = lease
	defer func() { s.lease = nil }()
	return x.leased(ctx, ex, lease)
}

// checkoutFailed answers a failed checkout with a synthesized error (FR-39).
func (x *msg) checkoutFailed(co spans.Step, ex spans.Exchange, err error) (proxy.CloseReason, bool) {
	s := x.s
	var ref proxy.Refusal
	if de, ok := err.(interface{ CorralErrorDetail() string }); ok {
		ref, _ = proxy.RefusalForDetail(de.CorralErrorDetail())
	}
	if ref == (proxy.Refusal{}) {
		ref = proxy.UpstreamUnavailable
	}
	co.End(ref, nil)
	ex.End(ref, nil)
	noReply := x.req.Flags&wire.FlagMoreToCome != 0
	if err := x.drain(); err != nil {
		return x.clientIOFail(err)
	}
	if !noReply {
		if err := s.write(x.frame(proxy.ErrorBody(ref, proxy.Labels(ref, x.req)), 0)); err != nil {
			return x.clientIOFail(err)
		}
	}
	x.complete(proxy.OutcomeProxyError, ref, noReply)
	return "", true
}

// unknownFail ends the exchange in outcome-unknown: the upstream connection is
// discarded and the client closed (FR-40).
func (x *msg) upstreamFail(lease Lease, rt spans.Step, ex spans.Exchange, err error) (proxy.CloseReason, bool) {
	lease.Discard(pool.CloseError)
	if isProtocolErr(err) {
		x.s.e.d.Obs.UpstreamProtocolError(x.s.ctx, lease.Upstream().Host(), err)
	}
	rt.End(err, nil)
	ex.End(err, nil)
	x.complete(proxy.OutcomeProxyError, err, false)
	return proxy.CloseUpstreamFailure, false
}

// isProtocolErr reports whether err is an upstream protocol violation (not a
// transport failure and not an ok:0 reply).
func isProtocolErr(err error) bool {
	switch _, d := errclass.Classify(err); string(d) {
	case "unexpected_response", "message_too_large", "malformed_message", "checksum_mismatch":
		return true
	}
	return false
}

func classOf(err error) string { c, _ := errclass.Classify(err); return string(c) }

// partialCommand returns the command name of a partly received OP_MSG: the
// first key of the kind-0 body, if the key is complete in the bytes received.
// It returns "" otherwise. It reads no values, and keeps nothing.
func partialCommand(b []byte) string {
	if len(b) < 21 || wire.ParseHeader(b).OpCode != wire.OpMsg {
		return ""
	}
	pos := 20 // the first section kind byte, after the flags
	for i := 0; i < 8 && pos < len(b); i++ {
		switch b[pos] {
		case 0:
			el, st, err := bson.FirstElementPartial(b[pos+1:])
			if err != nil || (st != bson.PartialOK && st != bson.PartialKeyOnly) {
				return ""
			}
			return el.Key()
		case 1:
			if pos+5 > len(b) {
				return ""
			}
			// The size is attacker-controlled: bound it in int64 so the skip
			// cannot wrap on a 32-bit platform.
			size := int64(int32(binary.LittleEndian.Uint32(b[pos+1:])))
			if size < 5 || size > int64(len(b))-int64(pos)-1 { // 4-byte length plus the identifier's NUL at least
				return ""
			}
			pos += 1 + int(size)
		default:
			return ""
		}
	}
	return ""
}
